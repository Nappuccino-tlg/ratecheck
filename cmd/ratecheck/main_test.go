package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The exit code is the whole interface when this runs in CI, so these are about the codes
// as much as the output. A tool that prints LEAKED and exits 0 is worse than no tool.

func exec(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut strings.Builder
	code = run(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

// -- a limiter that holds, and one that does not ---------------------------------------

// atomicServer counts under a lock: the check and the increment cannot interleave.
func atomicServer(limit int, window time.Duration) *httptest.Server {
	var (
		mu      sync.Mutex
		count   int
		resetAt time.Time
	)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		now := time.Now()
		if resetAt.IsZero() || now.After(resetAt) {
			count, resetAt = 0, now.Add(window)
		}
		allowed := count < limit
		if allowed {
			count++
		}
		mu.Unlock()

		if allowed {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusTooManyRequests)
	}))
}

// racyServer reads its counter, does a round trip's worth of work, then writes it back.
func racyServer(limit int, window, gap time.Duration) *httptest.Server {
	var (
		mu      sync.Mutex
		count   int
		resetAt time.Time
	)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		now := time.Now()
		if resetAt.IsZero() || now.After(resetAt) {
			count, resetAt = 0, now.Add(window)
		}
		seen := count
		mu.Unlock()

		if seen >= limit {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		time.Sleep(gap)
		mu.Lock()
		count = seen + 1
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
}

func TestALeakExitsOne(t *testing.T) {
	server := racyServer(5, time.Second, 30*time.Millisecond)
	defer server.Close()

	code, stdout, _ := exec(t, "-burst", "20", "-reset", "5s", server.URL)
	if code != exitLeaked {
		t.Fatalf("exit = %d, want %d\n%s", code, exitLeaked, stdout)
	}
	if !strings.Contains(stdout, "LEAKED") {
		t.Errorf("output does not say LEAKED:\n%s", stdout)
	}
}

func TestALimiterThatHoldsExitsZero(t *testing.T) {
	server := atomicServer(5, time.Second)
	defer server.Close()

	code, stdout, _ := exec(t, "-burst", "20", "-reset", "5s", server.URL)
	if code != exitHeld {
		t.Fatalf("exit = %d, want %d\n%s", code, exitHeld, stdout)
	}
	if !strings.Contains(stdout, "HELD") {
		t.Errorf("output does not say HELD:\n%s", stdout)
	}
}

func TestNoLimitAtAllIsNotACiFailure(t *testing.T) {
	// Worth being deliberate about: an endpoint with no limiter is a finding, but not one
	// this tool should fail a build over. It said what it found; that is the job.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	code, stdout, _ := exec(t, "-n", "10", "-burst", "5", server.URL)
	if code != exitHeld {
		t.Fatalf("exit = %d, want %d\n%s", code, exitHeld, stdout)
	}
	if !strings.Contains(stdout, "none found") {
		t.Errorf("output does not say no limit was found:\n%s", stdout)
	}
}

// -- the burst is sized from the limit when it is not given -----------------------------

func TestTheBurstIsSizedFromTheLimitWhenNotGiven(t *testing.T) {
	var seen atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		seen.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	// Everything is rejected, so discovery finds a limit of zero and the run stops. What
	// matters here is that -burst=0 does not crash and does not fire an unbounded burst.
	code, _, _ := exec(t, "-reset", "0", server.URL)
	if code != exitUnknown {
		t.Fatalf("exit = %d, want %d", code, exitUnknown)
	}
	if seen.Load() > 50 {
		t.Errorf("sent %d requests to an endpoint that rejected every one", seen.Load())
	}
}

// -- output shape -----------------------------------------------------------------------

func TestJsonIsValidAndCarriesTheVerdict(t *testing.T) {
	server := atomicServer(4, time.Second)
	defer server.Close()

	code, stdout, stderr := exec(t, "-json", "-burst", "15", "-reset", "5s", server.URL)
	if code != exitHeld {
		t.Fatalf("exit = %d, want %d\n%s", code, exitHeld, stdout)
	}

	var report struct {
		URL       string `json:"url"`
		Verdict   string `json:"verdict"`
		Discovery struct {
			Limit int `json:"limit"`
		} `json:"discovery"`
		Burst struct {
			Fired    int     `json:"fired"`
			SpreadMS float64 `json:"spread_ms"`
		} `json:"burst"`
	}
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout)
	}
	if report.Verdict != "held" {
		t.Errorf("verdict = %q, want %q", report.Verdict, "held")
	}
	if report.Discovery.Limit != 4 {
		t.Errorf("limit = %d, want 4", report.Discovery.Limit)
	}
	if report.Burst.Fired != 15 {
		t.Errorf("fired = %d, want 15", report.Burst.Fired)
	}
	// Progress on stdout would make the JSON unparseable for whatever is reading it.
	if stderr != "" {
		t.Errorf("-json still wrote progress to stderr:\n%s", stderr)
	}
}

// -- usage ------------------------------------------------------------------------------

func TestVersion(t *testing.T) {
	code, stdout, _ := exec(t, "-version")
	if code != exitHeld {
		t.Errorf("exit = %d, want %d", code, exitHeld)
	}
	if !strings.HasPrefix(stdout, "ratecheck ") {
		t.Errorf("stdout = %q", stdout)
	}
}

func TestUsageProblemsExitTwo(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"no url", nil, "usage:"},
		{"two urls", []string{"http://a", "http://b"}, "usage:"},
		{"no scheme", []string{"example.com"}, "http:// or https://"},
		{"negative burst", []string{"-burst", "-1", "http://example.com"}, "-burst"},
		{"zero probes", []string{"-n", "0", "http://example.com"}, "-n must be"},
		{"bad header", []string{"-H", "nocolon", "http://example.com"}, "Name: value"},
		{"unknown flag", []string{"-nope", "http://example.com"}, "flag provided but not defined"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, _, stderr := exec(t, c.args...)
			if code != exitUnknown {
				t.Errorf("exit = %d, want %d", code, exitUnknown)
			}
			if !strings.Contains(stderr, c.want) {
				t.Errorf("stderr does not mention %q:\n%s", c.want, stderr)
			}
		})
	}
}

func TestUsageWarnsWhatTheToolDoes(t *testing.T) {
	// It exhausts a quota against whatever it is pointed at. Someone reading -h should
	// not have to find that out by running it against production.
	_, _, stderr := exec(t)
	if !strings.Contains(stderr, "responsible for") {
		t.Errorf("usage does not say to point it at your own service:\n%s", stderr)
	}
	if !strings.Contains(stderr, "exhausts a quota") {
		t.Errorf("usage does not say it exhausts a quota:\n%s", stderr)
	}
}

func TestAnUnreachableHostExitsTwo(t *testing.T) {
	code, _, stderr := exec(t, "-burst", "3", "-timeout", "500ms", "http://127.0.0.1:1/nope")
	if code != exitUnknown {
		t.Errorf("exit = %d, want %d", code, exitUnknown)
	}
	if !strings.Contains(stderr, "ratecheck:") {
		t.Errorf("no error was reported:\n%s", stderr)
	}
}

func TestHeadersReachTheServer(t *testing.T) {
	got := make(chan http.Header, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case got <- r.Header.Clone():
		default:
		}
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	exec(t, "-H", "Authorization: Bearer secret", "-H", "X-Trace: 1",
		"-burst", "2", "-reset", "0", server.URL)

	headers := <-got
	if headers.Get("Authorization") != "Bearer secret" {
		t.Errorf("Authorization = %q", headers.Get("Authorization"))
	}
	if headers.Get("X-Trace") != "1" {
		t.Errorf("X-Trace = %q", headers.Get("X-Trace"))
	}
}

func TestTheMethodIsHonoured(t *testing.T) {
	got := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case got <- r.Method:
		default:
		}
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	exec(t, "-X", "post", "-burst", "2", "-reset", "0", server.URL)

	if method := <-got; method != http.MethodPost {
		t.Errorf("method = %q, want POST", method)
	}
}

func TestRedirectsAreNotFollowed(t *testing.T) {
	// A 301 to a path with a different limit would silently change what is measured, and
	// the report would name the URL that was asked for rather than the one that answered.
	var elsewhere atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		elsewhere.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusMovedPermanently)
	}))
	defer server.Close()

	exec(t, "-n", "3", "-burst", "2", "-reset", "0", server.URL)

	if elsewhere.Load() != 0 {
		t.Errorf("followed the redirect %d times", elsewhere.Load())
	}
}

var _ io.Writer = (*strings.Builder)(nil)

func TestStoppingIsReportedAsStoppedRatherThanAsAFailure(t *testing.T) {
	// Ctrl-C is a person changing their mind, not the tool breaking, and the message
	// should not read like a bug report.
	var stderr strings.Builder
	if code := reportError(&stderr, context.Canceled); code != exitUnknown {
		t.Errorf("exit = %d, want %d", code, exitUnknown)
	}
	if !strings.Contains(stderr.String(), "stopped") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestAStampedVersionWins(t *testing.T) {
	// The release build sets this with -ldflags, and it must beat whatever the module
	// system thinks, because a binary built from a tag is the tag.
	original := version
	t.Cleanup(func() { version = original })

	version = "v9.9.9"
	if got := versionString(); got != "v9.9.9" {
		t.Errorf("versionString() = %q, want the stamped value", got)
	}
}

func TestAnUnstampedBinaryFindsItsModuleVersion(t *testing.T) {
	// Without a stamp, `go install ...@v0.1.0` should still report v0.1.0 rather than
	// "dev". Under `go test` there is no module version to find, so the honest fallback
	// is what this can actually assert.
	original := version
	t.Cleanup(func() { version = original })

	version = "dev"
	got := versionString()
	if got == "" {
		t.Fatal("versionString() returned nothing")
	}
	if got != "dev" && !strings.HasPrefix(got, "v") {
		t.Errorf("versionString() = %q, want either the fallback or a module version", got)
	}
}
