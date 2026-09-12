package probe

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Options is everything a run needs. Zero values are not defaults; cmd/ratecheck fills
// them in so that the defaults live in one place, next to the flags that document them.
type Options struct {
	Request Request
	// RejectStatus is what "denied" looks like. 429 for almost everyone, but plenty of
	// APIs answer 403, and a tool that assumed 429 would call those unlimited.
	RejectStatus int
	// MaxProbes caps the requests spent looking for the limit. A ceiling, not a target:
	// discovery stops at the first rejection.
	MaxProbes int
	// Burst is how many requests are fired at once. It has to exceed the limit or the
	// test proves nothing -- the limiter would be right to allow all of them. Zero means
	// work it out from whatever discovery finds.
	Burst int
	// ResetWait is how long to wait for quota to come back before giving up on the
	// concurrent test. Zero skips straight to it, which is only right if you know the
	// window is already open.
	ResetWait time.Duration
	// Pace is the gap between discovery requests. Some limiters are per-second, and
	// hammering them as fast as the wire allows finds a different limit than a caller
	// would.
	Pace time.Duration
}

// Discovery is what sequential probing found.
type Discovery struct {
	Limit      int    `json:"limit"`
	Rejected   bool   `json:"rejected"`
	Took       string `json:"took"`
	RetryAfter string `json:"retry_after,omitempty"`
	// Claim is what the API's own headers said. Named rather than embedded because a
	// Discovery has two different limits in it -- the one it measured and the one it was
	// told -- and the whole point is that those can disagree.
	Claim HeaderClaim `json:"claim"`
}

// HeaderClaim is what the API said about itself, which is worth repeating back because it
// is quite often not what the API then does.
type HeaderClaim struct {
	Limit     string `json:"header_limit,omitempty"`
	Remaining string `json:"header_remaining,omitempty"`
	Reset     string `json:"header_reset,omitempty"`
}

// Window is what happened while waiting for the quota to return.
type Window struct {
	Waited   string `json:"waited"`
	Returned bool   `json:"returned"`
	// Shape is filled in after the burst, not before it. Measuring how much quota came
	// back would mean spending that quota, and then the burst -- the only measurement
	// that matters -- would have none left to work with. The burst answers both: how many
	// it was allowed is how much had come back.
	Shape string `json:"shape,omitempty"`
}

// Burst is the concurrent test: the one a sequential test cannot do.
type Burst struct {
	Fired   int `json:"fired"`
	Allowed int `json:"allowed"`
	Denied  int `json:"denied"`
	Failed  int `json:"failed"`
	// Budget is how many the limiter should have allowed, given what discovery found and
	// what the reset probe already spent.
	Budget int `json:"budget"`

	// SpreadMS is the wall-clock gap between the first and last request reaching the
	// wire. This is the number that says whether the test was real.
	SpreadMS float64 `json:"spread_ms"`
	// Warm is how many went out on a connection that was already open.
	Warm int `json:"warm"`
}

// Concurrent reports whether the burst actually overlapped enough to mean anything.
//
// The threshold is deliberately generous: a limiter with a check-then-increment race
// leaks for as long as the gap between its read and its write, which is a database round
// trip -- single-digit milliseconds at best. Requests spread over more than a tenth of a
// second are not testing that.
func (b Burst) Concurrent() bool {
	return b.SpreadMS <= 100 && b.Fired > 0
}

// Report is a whole run.
type Report struct {
	URL       string    `json:"url"`
	Discovery Discovery `json:"discovery"`
	Window    *Window   `json:"window,omitempty"`
	Burst     *Burst    `json:"burst,omitempty"`
	Verdict   Verdict   `json:"verdict"`
	Note      string    `json:"note,omitempty"`
}

// Verdict is the answer, and the process exit code comes from it.
type Verdict string

const (
	// Held: the limiter allowed no more under concurrency than it did one at a time.
	Held Verdict = "held"
	// Leaked: more requests got through together than the limit allows. This is what a
	// check-then-increment limiter looks like from the outside.
	Leaked Verdict = "leaked"
	// Unknown: the run could not answer the question. Not a pass.
	Unknown Verdict = "unknown"
)

// ErrNoLimit means nothing was rejected within the probe budget.
var ErrNoLimit = errors.New("no limit found")

// Run does the whole thing: find the limit, wait for it to come back, then hit it with
// everything at once.
func Run(ctx context.Context, opts Options, progress func(string)) (Report, error) {
	if progress == nil {
		progress = func(string) {}
	}
	report := Report{URL: opts.Request.URL, Verdict: Unknown}

	// Discovery needs one connection and one at a time is the whole point of it, so it
	// gets its own small client. The burst cannot be sized until this has run.
	prober := NewClient(opts.Request, 1)
	defer prober.Close()

	progress("Finding the limit, one request at a time")
	discovery, err := discover(ctx, prober, opts)
	report.Discovery = discovery
	if err != nil {
		return report, err
	}
	if !discovery.Rejected {
		report.Note = fmt.Sprintf(
			"%d requests in a row were all allowed, so there is no limit here to test.",
			discovery.Limit,
		)
		return report, ErrNoLimit
	}
	if discovery.Limit == 0 {
		// Rejected from the very first request, so nothing was ever allowed and there is
		// no limit here to exceed. Answering "held" would be true and useless: a burst
		// against a closed window proves only that the window is closed.
		report.Note = "Every request was rejected, starting with the first. There is no " +
			"limit to measure until something is allowed -- the quota is most likely " +
			"already spent, so wait for the window to reopen and run this again."
		return report, nil
	}
	progress(fmt.Sprintf("  %d allowed, then %d", discovery.Limit, opts.RejectStatus))

	// Sized here, from the limit that was just measured, and never by discovering it
	// twice: a second pass would spend the very quota the burst needs, and the run would
	// arrive at a window that was already closed.
	if opts.Burst == 0 {
		opts.Burst = burstFor(discovery.Limit)
		progress(fmt.Sprintf("  firing %d at once, half again over the limit", opts.Burst))
	}

	if opts.Burst <= discovery.Limit {
		report.Note = fmt.Sprintf(
			"A burst of %d cannot test a limit of %d: the limiter would be right to allow "+
				"every one. Raise -burst above the limit.",
			opts.Burst, discovery.Limit,
		)
		return report, nil
	}

	// A second client, now that the burst size is known, holding a connection per request
	// in the burst.
	client := NewClient(opts.Request, opts.Burst)
	defer client.Close()

	// Warm the pool now, while requests are being rejected. Once the window reopens every
	// request costs quota, and a handshake at that moment is quota spent on TLS rather
	// than on the measurement.
	progress(fmt.Sprintf("  warming %d connections against the closed window", opts.Burst))
	warmed := warm(ctx, client, opts.Burst)
	// How many the pool gained, not how many the burst will find warm -- the burst counts
	// that for itself, and reports it, because that is the number worth trusting.
	progress(fmt.Sprintf("  %d new connections in the pool", warmed))

	// Everything from here is on a clock. The burst has to land inside the window that
	// just opened, so nothing slow goes between the reset and the release.
	spent := 0
	if opts.ResetWait > 0 {
		progress("Waiting for the quota to come back")
		window, consumed := waitForReset(ctx, client, opts)
		report.Window = &window
		spent = consumed
		if !window.Returned {
			report.Note = fmt.Sprintf(
				"The quota had not come back after %s, so the concurrent test never ran. "+
					"Raise -reset if the window is longer than that.",
				window.Waited,
			)
			return report, nil
		}
		progress(fmt.Sprintf("  quota returned after %s", window.Waited))
	}

	budget := discovery.Limit - spent
	if budget < 0 {
		budget = 0
	}

	progress(fmt.Sprintf("Releasing %d requests at once", opts.Burst))
	burst := fire(ctx, client, opts)
	burst.Budget = budget
	report.Burst = &burst

	switch {
	case !burst.Concurrent():
		report.Verdict = Unknown
		report.Note = fmt.Sprintf(
			"The burst reached the server over %.0fms, which is too spread out to catch a "+
				"race. Only %d of %d went out on a warm connection.",
			burst.SpreadMS, burst.Warm, burst.Fired,
		)
	case burst.Allowed > budget:
		report.Verdict = Leaked
	default:
		report.Verdict = Held
	}

	// Now the shape, from the burst rather than from a phase of its own: with a limiter
	// that held, the number it allowed is the number that had come back.
	if report.Window != nil && report.Verdict == Held {
		report.Window.Shape = shapeOf(burst.Allowed+spent, discovery.Limit)
	}
	return report, nil
}

// discover sends requests one at a time until one is rejected.
func discover(ctx context.Context, client *Client, opts Options) (Discovery, error) {
	started := time.Now()
	var discovery Discovery

	for i := 0; i < opts.MaxProbes; i++ {
		if i > 0 && opts.Pace > 0 {
			if err := sleep(ctx, opts.Pace); err != nil {
				return discovery, err
			}
		}
		result := client.Do(ctx)
		if result.Err != nil {
			discovery.Took = since(started)
			return discovery, fmt.Errorf("request %d: %w", i+1, result.Err)
		}
		discovery.Limit = i
		discovery.Claim = HeaderClaim{
			Limit:     result.Limit,
			Remaining: result.Remaining,
			Reset:     result.Reset,
		}
		if result.Status == opts.RejectStatus {
			discovery.Rejected = true
			discovery.RetryAfter = result.RetryAfter
			discovery.Took = since(started)
			return discovery, nil
		}
		discovery.Limit = i + 1
	}
	discovery.Took = since(started)
	return discovery, nil
}

// warm opens n connections by sending rejected requests and letting the transport keep
// the sockets.
//
// Rejections are the cheapest request there is against a limited endpoint, and this is the
// only moment in a run where sending a pile of them costs nothing that matters.
//
// It takes more than one volley. A rejection comes back in well under a millisecond, so by
// the time the last goroutine in a volley of thirty asks the transport for a connection,
// the first few have already finished and put theirs back -- and it reuses one of those
// instead of opening its own. Each round adds the connections the previous round was short
// of, and two rounds is enough in practice for the sizes this fires.
func warm(ctx context.Context, client *Client, n int) int {
	opened := 0
	for round := 0; round < warmRounds && opened < n; round++ {
		for _, result := range concurrently(ctx, client, n) {
			// A connection the transport did not reuse is one it had to open, which is
			// one more in the pool afterwards.
			if result.Err == nil && !result.Reused {
				opened++
			}
		}
	}
	return opened
}

// warmRounds bounds the rejected requests spent on warming at warmRounds x burst. Cheap,
// but not free, and somebody else's service is paying.
const warmRounds = 2

// waitForReset polls one request at a time until one is allowed, and returns as soon as
// that happens.
//
// One request at a time, because every probe that succeeds is quota the burst will not
// have. It returns how many it spent so the burst can be judged against what is left
// rather than against the whole limit.
func waitForReset(ctx context.Context, client *Client, opts Options) (Window, int) {
	started := time.Now()
	deadline := started.Add(opts.ResetWait)
	var window Window
	spent := 0

	for time.Now().Before(deadline) {
		result := client.Do(ctx)
		if result.Err != nil {
			break
		}
		if result.Status != opts.RejectStatus {
			spent++
			window.Returned = true
			break
		}
		if err := sleep(ctx, pollEvery); err != nil {
			break
		}
	}
	window.Waited = since(started)
	return window, spent
}

// burstFor picks a burst half again as large as the limit, and at least five over it.
//
// Over the limit because a burst that fits inside the quota proves nothing; not far over,
// because every request past the limit is load on somebody's service for no extra
// information -- the race shows up in the first few.
func burstFor(limit int) int {
	burst := limit + limit/2
	if burst < limit+5 {
		burst = limit + 5
	}
	return burst
}

// pollEvery is how often to ask whether the window has reopened. Frequent enough not to
// add much to the reported wait, rare enough not to be its own load test.
const pollEvery = time.Second

// shapeOf names the window from how much allowance turned out to be there.
func shapeOf(restored, limit int) string {
	switch {
	case restored >= limit:
		return fmt.Sprintf("the whole allowance of %d came back at once -- a fixed window", limit)
	case restored <= 2:
		return "quota comes back a request at a time -- a sliding window or token bucket"
	default:
		return fmt.Sprintf("%d of %d came back -- the window is partway through refilling",
			restored, limit)
	}
}

// fire releases the burst and measures how tightly it landed.
func fire(ctx context.Context, client *Client, opts Options) Burst {
	results := concurrently(ctx, client, opts.Burst)

	burst := Burst{Fired: len(results)}
	var earliest, latest time.Time
	for _, result := range results {
		switch {
		case result.Err != nil:
			burst.Failed++
		case result.Allowed(opts.RejectStatus):
			burst.Allowed++
		default:
			burst.Denied++
		}
		if result.Reused {
			burst.Warm++
		}
		if result.WroteAt.IsZero() {
			continue
		}
		if earliest.IsZero() || result.WroteAt.Before(earliest) {
			earliest = result.WroteAt
		}
		if result.WroteAt.After(latest) {
			latest = result.WroteAt
		}
	}
	if !earliest.IsZero() {
		burst.SpreadMS = float64(latest.Sub(earliest).Microseconds()) / 1000
	}
	return burst
}

// concurrently releases n requests against a barrier.
//
// Every goroutine is started and parked first, so releasing them is one channel close
// rather than n goroutine startups -- the difference between requests that leave together
// and requests that leave in the order the scheduler got round to them.
func concurrently(ctx context.Context, client *Client, n int) []Result {
	results := make([]Result, n)
	release := make(chan struct{})
	var ready, done sync.WaitGroup

	ready.Add(n)
	done.Add(n)
	for i := 0; i < n; i++ {
		go func(slot int) {
			defer done.Done()
			ready.Done()
			<-release
			results[slot] = client.Do(ctx)
		}(i)
	}

	ready.Wait()
	close(release)
	done.Wait()
	return results
}

func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func since(t time.Time) string {
	return time.Since(t).Round(time.Millisecond).String()
}
