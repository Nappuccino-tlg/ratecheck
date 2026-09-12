package probe

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"time"
)

// Two limiters, one of them wrong, and the whole tool exists to tell them apart.
//
// Both are real HTTP servers rather than stubs, because everything ratecheck measures --
// connection reuse, the spread across a burst, the status a limiter answers with -- only
// exists on a socket. A fake that returned canned Results would be testing the assertions
// against themselves.

// atomicLimiter counts under a mutex, so the check and the increment cannot interleave.
// A correct limiter, and the thing ratecheck must not accuse.
type atomicLimiter struct {
	mu      sync.Mutex
	limit   int
	count   int
	window  time.Duration
	resetAt time.Time
	// sliding gives capacity back one request at a time instead of all at once, which is
	// how a token bucket behaves and a different shape to report.
	sliding bool
}

func (l *atomicLimiter) allow() bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	if l.resetAt.IsZero() {
		l.resetAt = now.Add(l.window)
	}
	if now.After(l.resetAt) {
		if l.sliding {
			// One request's worth of room comes back per window.
			if l.count > 0 {
				l.count--
			}
			l.resetAt = now.Add(l.window)
		} else {
			l.count = 0
			l.resetAt = now.Add(l.window)
		}
	}
	if l.count >= l.limit {
		return false
	}
	l.count++
	return true
}

// racyLimiter is the mistake this tool is looking for: it reads the counter, does some
// work, and writes it back. Every request that arrives during that gap reads the same
// number and every one of them thinks there is room.
//
// The sleep is what a real one does for free -- a Redis GET and SET, or a SELECT then an
// UPDATE, with a network in between. Making it explicit is what makes the test
// deterministic instead of a coin flip.
type racyLimiter struct {
	mu      sync.Mutex // guards count for the read and for the write, never across them
	limit   int
	count   int
	gap     time.Duration
	window  time.Duration
	resetAt time.Time
}

func (l *racyLimiter) allow() bool {
	l.mu.Lock()
	now := time.Now()
	if l.resetAt.IsZero() || now.After(l.resetAt) {
		l.count = 0
		l.resetAt = now.Add(l.window)
	}
	seen := l.count
	l.mu.Unlock()

	if seen >= l.limit {
		return false
	}

	time.Sleep(l.gap) // the round trip that makes the race a race

	l.mu.Lock()
	l.count = seen + 1
	l.mu.Unlock()
	return true
}

type limiter interface{ allow() bool }

// newServer wraps a limiter in an HTTP server that answers the way a real API does.
func newServer(l limiter, opts ...func(http.ResponseWriter)) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if l.allow() {
			for _, opt := range opts {
				opt(w)
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		for _, opt := range opts {
			opt(w)
		}
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"slow down"}`))
	}))
}

// withRateHeaders makes the server advertise itself the way a well-behaved API does.
func withRateHeaders(limit string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("X-RateLimit-Limit", limit)
		w.Header().Set("Retry-After", "1")
	}
}

// unlimited never says no.
type unlimited struct{}

func (unlimited) allow() bool { return true }

// newServerFunc is for the handful of tests that need a handler rather than a limiter.
func newServerFunc(h http.HandlerFunc) *httptest.Server {
	return httptest.NewServer(h)
}
