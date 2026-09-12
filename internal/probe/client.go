// Package probe measures what a rate limiter actually does, as opposed to what it says.
//
// The measurement that matters is the concurrent one, and it is the one that is easy to
// get wrong: requests fired from a cold client do not arrive together. Each one opens a
// socket and negotiates TLS first, which takes tens to hundreds of milliseconds and varies
// per connection, so "fifty at once" reaches the server spread over half a second. That is
// long enough for a limiter with a race in it to serialise and look correct.
//
// So the client here holds a warm connection per worker, and every burst reports how
// tightly it actually landed. A verdict from requests that did not overlap is not a
// verdict, and saying so is more useful than a confident wrong answer.
package probe

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"strings"
	"time"
)

// Request is what to send. One shape for every phase, so the limiter cannot tell the
// discovery requests from the burst ones -- if it could, the burst would not be measuring
// the same thing discovery measured.
type Request struct {
	Method  string
	URL     string
	Headers http.Header
	Timeout time.Duration
}

// Result is one response, plus what the transport did to get it.
type Result struct {
	Status int
	// Headers worth reading back to the user: the limiter's own account of itself.
	RetryAfter string
	Limit      string
	Remaining  string
	Reset      string

	// WroteAt is when the request reached the wire, not when it was queued. The spread of
	// these across a burst is the only evidence that the burst was concurrent.
	WroteAt time.Time
	// Reused is whether the transport had a warm connection for it. A burst where these
	// are false was a burst of TLS handshakes.
	Reused bool

	Err error
}

// Allowed reports whether the limiter let this one through.
//
// Anything other than the rejection status counts as allowed, including a 500: a limiter
// that is failing open is letting requests through, and reporting that as "denied" would
// hide it.
func (r Result) Allowed(rejectStatus int) bool {
	return r.Err == nil && r.Status != rejectStatus
}

// Client sends requests over a pool of connections it can warm on demand.
type Client struct {
	http *http.Client
	req  Request
}

// NewClient builds a client that will hold up to maxConns connections open.
//
// The idle pool is sized to the burst rather than left at Go's default of 2 per host,
// because the whole point is to have a connection already established for every request
// in the burst. With the default, all but two of them would start with a handshake.
func NewClient(req Request, maxConns int) *Client {
	if maxConns < 1 {
		maxConns = 1
	}
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:        maxConns * 2,
		MaxIdleConnsPerHost: maxConns * 2,
		MaxConnsPerHost:     0, // never make a request wait for a connection
		IdleConnTimeout:     5 * time.Minute,
		TLSHandshakeTimeout: 10 * time.Second,
		// HTTP/2 multiplexes every request onto one connection, so a warm pool of fifty
		// would be fifty streams sharing a socket, written in whatever order that
		// connection's flow control allowed. That is not fifty callers arriving at once,
		// which is the thing being measured. HTTP/1.1 with a connection each is the
		// closer model, and the one where the arrival spread means something.
		ForceAttemptHTTP2: false,
		TLSClientConfig:   &tls.Config{MinVersion: tls.VersionTLS12},
	}
	return &Client{
		http: &http.Client{
			Transport: transport,
			// Redirects are followed by browsers but not by this: a 301 to a path with a
			// different limit would silently change what is being measured.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		req: req,
	}
}

// Do sends one request and records how it went.
func (c *Client) Do(ctx context.Context) Result {
	var result Result

	ctx, cancel := context.WithTimeout(ctx, c.req.Timeout)
	defer cancel()

	trace := &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			result.Reused = info.Reused
		},
		WroteRequest: func(httptrace.WroteRequestInfo) {
			result.WroteAt = time.Now()
		},
	}

	req, err := http.NewRequestWithContext(
		httptrace.WithClientTrace(ctx, trace), c.req.Method, c.req.URL, nil,
	)
	if err != nil {
		result.Err = err
		return result
	}
	req.Header = c.req.Headers.Clone()
	if req.Header == nil {
		req.Header = http.Header{}
	}

	response, err := c.http.Do(req)
	if err != nil {
		result.Err = err
		return result
	}
	defer response.Body.Close()

	// Drained, not ignored. A body left unread means the connection cannot go back in the
	// pool, and the next request in the burst would open a fresh one -- which is exactly
	// the warmth this whole package is trying to keep.
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))

	result.Status = response.StatusCode
	result.RetryAfter = response.Header.Get("Retry-After")
	result.Limit = firstHeader(response.Header, "X-RateLimit-Limit", "RateLimit-Limit")
	result.Remaining = firstHeader(response.Header, "X-RateLimit-Remaining", "RateLimit-Remaining")
	result.Reset = firstHeader(response.Header, "X-RateLimit-Reset", "RateLimit-Reset")
	return result
}

// Close releases every pooled connection.
func (c *Client) Close() {
	c.http.CloseIdleConnections()
}

func firstHeader(h http.Header, names ...string) string {
	for _, name := range names {
		if value := h.Get(name); value != "" {
			return value
		}
	}
	return ""
}

// ParseHeader turns a "Name: value" flag into its two halves.
func ParseHeader(raw string) (string, string, error) {
	name, value, found := strings.Cut(raw, ":")
	if !found {
		return "", "", fmt.Errorf("header %q is not in Name: value form", raw)
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return "", "", fmt.Errorf("header %q has no name", raw)
	}
	return name, strings.TrimSpace(value), nil
}
