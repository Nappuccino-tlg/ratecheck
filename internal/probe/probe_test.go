package probe

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func options(url string, burst int) Options {
	return Options{
		Request: Request{
			Method:  http.MethodGet,
			URL:     url,
			Headers: http.Header{},
			Timeout: 5 * time.Second,
		},
		RejectStatus: http.StatusTooManyRequests,
		MaxProbes:    60,
		Burst:        burst,
		ResetWait:    5 * time.Second,
	}
}

// -- the point of the tool -----------------------------------------------------------

func TestARacyLimiterIsCaught(t *testing.T) {
	// The headline. A limiter that reads its counter, works, and writes it back allows
	// far more than its limit when requests overlap -- and every sequential test of it
	// passes, which is why this needs a tool rather than a code review.
	server := newServer(&racyLimiter{limit: 5, gap: 30 * time.Millisecond, window: time.Second})
	defer server.Close()

	report, err := Run(t.Context(), options(server.URL, 20), nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if report.Verdict != Leaked {
		t.Fatalf("verdict = %q, want %q (note: %s)", report.Verdict, Leaked, report.Note)
	}
	if report.Discovery.Limit != 5 {
		t.Errorf("discovered limit = %d, want 5", report.Discovery.Limit)
	}
	if report.Burst.Allowed <= report.Burst.Budget {
		t.Errorf("allowed %d with a budget of %d, which is not a leak",
			report.Burst.Allowed, report.Burst.Budget)
	}
}

func TestAnAtomicLimiterIsNotAccused(t *testing.T) {
	// The other half, and the half that decides whether anyone keeps using this: a tool
	// that cries wolf on a correct limiter gets uninstalled.
	server := newServer(&atomicLimiter{limit: 5, window: time.Second})
	defer server.Close()

	report, err := Run(t.Context(), options(server.URL, 20), nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if report.Verdict != Held {
		t.Fatalf("verdict = %q, want %q (note: %s)", report.Verdict, Held, report.Note)
	}
	if report.Burst.Allowed > report.Burst.Budget {
		t.Errorf("allowed %d over a budget of %d", report.Burst.Allowed, report.Burst.Budget)
	}
}

func TestTheBurstActuallyOverlaps(t *testing.T) {
	// Everything above is only worth reading if the burst really did arrive together.
	// Without warm connections these requests would be spread over the time it takes to
	// do twenty TCP handshakes, and a racy limiter would have time to serialise.
	server := newServer(&atomicLimiter{limit: 5, window: time.Second})
	defer server.Close()

	report, err := Run(t.Context(), options(server.URL, 20), nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !report.Burst.Concurrent() {
		t.Fatalf("burst spread over %.1fms, which is not concurrent", report.Burst.SpreadMS)
	}
	// Most of them, not all of them. A connection freed by a request that finished early
	// can be picked up by one still starting, so the pool ends a little short of the burst
	// and one cold dial in twenty invalidates nothing. Without any warming at all this
	// would be the other way round -- the barrier releases every request at once against
	// an empty pool, so almost none of them would find a connection waiting.
	if cold := report.Burst.Fired - report.Burst.Warm; cold > report.Burst.Fired/5 {
		t.Errorf("%d of %d requests went out cold; the pool was not warmed",
			cold, report.Burst.Fired)
	}
}

// -- discovery -------------------------------------------------------------------------

func TestAnEndpointWithNoLimitSaysSo(t *testing.T) {
	server := newServer(unlimited{})
	defer server.Close()

	opts := options(server.URL, 10)
	opts.MaxProbes = 15
	report, err := Run(t.Context(), opts, nil)

	if err == nil || !strings.Contains(err.Error(), "no limit") {
		t.Fatalf("err = %v, want ErrNoLimit", err)
	}
	if report.Discovery.Rejected {
		t.Error("nothing was rejected, but Rejected is true")
	}
	if report.Discovery.Limit != 15 {
		t.Errorf("probed %d times, want 15", report.Discovery.Limit)
	}
}

func TestTheHeadersTheApiSendsAreReportedBack(t *testing.T) {
	server := newServer(&atomicLimiter{limit: 3, window: time.Second}, withRateHeaders("3"))
	defer server.Close()

	opts := options(server.URL, 10)
	opts.ResetWait = 0
	report, err := Run(t.Context(), opts, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if report.Discovery.Claim.Limit != "3" {
		t.Errorf("header limit = %q, want %q", report.Discovery.Claim.Limit, "3")
	}
	if report.Discovery.RetryAfter != "1" {
		t.Errorf("Retry-After = %q, want %q", report.Discovery.RetryAfter, "1")
	}
}

func TestADifferentRejectionStatusIsHonoured(t *testing.T) {
	// Plenty of APIs answer 403 rather than 429, and assuming 429 would report those as
	// having no limit at all -- the most dangerous wrong answer this tool could give.
	// Atomic, not a bare int: the warm phase and the burst both arrive concurrently, and
	// a handler that counted with count++ would be the data race this tool is about.
	var count atomic.Int64
	server := newServerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if count.Add(1) > 4 {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	defer server.Close()

	opts := options(server.URL, 10)
	opts.RejectStatus = http.StatusForbidden
	opts.ResetWait = 0
	report, err := Run(t.Context(), opts, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if report.Discovery.Limit != 4 {
		t.Errorf("limit = %d, want 4", report.Discovery.Limit)
	}
}

// -- the window ------------------------------------------------------------------------

func TestAFixedWindowIsNamed(t *testing.T) {
	server := newServer(&atomicLimiter{limit: 6, window: 500 * time.Millisecond})
	defer server.Close()

	report, err := Run(t.Context(), options(server.URL, 15), nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if report.Window == nil || !report.Window.Returned {
		t.Fatal("the window never reopened")
	}
	if !strings.Contains(report.Window.Shape, "fixed window") {
		t.Errorf("shape = %q, want it to name a fixed window", report.Window.Shape)
	}
}

func TestASlidingWindowIsNamed(t *testing.T) {
	server := newServer(&atomicLimiter{limit: 6, window: 400 * time.Millisecond, sliding: true})
	defer server.Close()

	report, err := Run(t.Context(), options(server.URL, 15), nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if report.Window == nil || !report.Window.Returned {
		t.Fatal("the window never reopened")
	}
	if !strings.Contains(report.Window.Shape, "a request at a time") {
		t.Errorf("shape = %q, want it to name a sliding window", report.Window.Shape)
	}
}

func TestAWindowThatNeverReopensIsNotAPass(t *testing.T) {
	// An hour-long window and a ninety-second wait is the ordinary case, and answering
	// "held" because the burst never ran would be a lie in the most useful direction.
	server := newServer(&atomicLimiter{limit: 3, window: time.Hour})
	defer server.Close()

	opts := options(server.URL, 10)
	opts.ResetWait = 1500 * time.Millisecond
	report, err := Run(t.Context(), opts, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if report.Verdict != Unknown {
		t.Errorf("verdict = %q, want %q", report.Verdict, Unknown)
	}
	if !strings.Contains(report.Note, "-reset") {
		t.Errorf("note does not say how to fix it: %q", report.Note)
	}
}

// -- refusing to answer ------------------------------------------------------------------

func TestABurstSmallerThanTheLimitProvesNothing(t *testing.T) {
	server := newServer(&atomicLimiter{limit: 10, window: time.Second})
	defer server.Close()

	opts := options(server.URL, 4)
	report, err := Run(t.Context(), opts, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if report.Verdict != Unknown {
		t.Errorf("verdict = %q, want %q", report.Verdict, Unknown)
	}
	if !strings.Contains(report.Note, "-burst") {
		t.Errorf("note does not say how to fix it: %q", report.Note)
	}
	if report.Burst != nil {
		t.Error("a burst that could prove nothing was fired anyway")
	}
}

func TestAnUnreachableHostIsAnError(t *testing.T) {
	opts := options("http://127.0.0.1:1/nothing-here", 5)
	opts.Request.Timeout = time.Second
	_, err := Run(t.Context(), opts, nil)
	if err == nil {
		t.Fatal("want an error for an unreachable host")
	}
}

func TestCancellingStopsTheRun(t *testing.T) {
	server := newServer(&atomicLimiter{limit: 3, window: time.Hour})
	defer server.Close()

	ctx, cancel := context.WithCancel(t.Context())
	opts := options(server.URL, 10)
	opts.ResetWait = time.Minute

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = Run(ctx, opts, nil)
	}()

	time.Sleep(200 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the run kept going after its context was cancelled")
	}
}

// -- progress ----------------------------------------------------------------------------

func TestProgressIsReported(t *testing.T) {
	server := newServer(&atomicLimiter{limit: 3, window: 300 * time.Millisecond})
	defer server.Close()

	var lines []string
	if _, err := Run(t.Context(), options(server.URL, 10), func(line string) {
		lines = append(lines, line)
	}); err != nil {
		t.Fatalf("run: %v", err)
	}

	joined := strings.Join(lines, "\n")
	for _, want := range []string{"Finding the limit", "warming", "Releasing"} {
		if !strings.Contains(joined, want) {
			t.Errorf("progress never mentioned %q:\n%s", want, joined)
		}
	}
}

func TestANilProgressFuncIsFine(t *testing.T) {
	server := newServer(&atomicLimiter{limit: 2, window: 200 * time.Millisecond})
	defer server.Close()

	if _, err := Run(t.Context(), options(server.URL, 8), nil); err != nil {
		t.Fatalf("run: %v", err)
	}
}

func TestAnAlreadyExhaustedQuotaIsNotAPass(t *testing.T) {
	// Rejected from the first request. Nothing was ever allowed, so there is no limit to
	// exceed -- and reporting "held" would be technically true and completely useless.
	server := newServerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	})
	defer server.Close()

	report, err := Run(t.Context(), options(server.URL, 10), nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if report.Verdict != Unknown {
		t.Errorf("verdict = %q, want %q", report.Verdict, Unknown)
	}
	if report.Burst != nil {
		t.Error("a burst was fired against a window that was never open")
	}
	if !strings.Contains(report.Note, "already spent") {
		t.Errorf("note does not explain what to do: %q", report.Note)
	}
}
