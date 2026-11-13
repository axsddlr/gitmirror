// Package httpx provides an http.RoundTripper that makes forge API calls
// resilient to transient failures and rate limits without ever duplicating a
// non-idempotent write.
package httpx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	defaultMaxRetries = 4
	defaultMaxWait    = 15 * time.Minute
	defaultUserAgent  = "gitmirror"
	backoffBase       = time.Second
	backoffCap        = 30 * time.Second
	drainLimit        = 64 << 10
)

type idempotentKey struct{}

// WithIdempotent marks requests made with the returned context as safe to
// retry even if their method is not idempotent (for example a PATCH that sets
// fields to absolute values).
func WithIdempotent(ctx context.Context) context.Context {
	return context.WithValue(ctx, idempotentKey{}, true)
}

// RateLimitError is returned when a rate limit would require waiting longer
// than Transport.MaxWait.
type RateLimitError struct {
	// Reset is when the limit is expected to lift.
	Reset time.Time
	// Status is the HTTP status of the limiting response.
	Status int
	// URL is the request URL with userinfo, query and fragment removed.
	URL string
}

// Error implements error. It never includes the URL query.
func (e *RateLimitError) Error() string {
	return fmt.Sprintf("httpx: rate limited (status %d) for %s; resets at %s",
		e.Status, e.URL, e.Reset.UTC().Format(time.RFC3339))
}

// Transport is an http.RoundTripper that retries transient failures and
// honours rate-limit headers. It holds no per-request state and is safe for
// concurrent use.
type Transport struct {
	// Base performs each attempt; nil means DefaultBase().
	Base http.RoundTripper
	// MaxRetries is the number of retries after the first attempt; 0 means 4
	// and a negative value disables retries.
	MaxRetries int
	// MaxWait is the longest single wait honoured for Retry-After or a
	// rate-limit reset; 0 means 15 minutes.
	MaxWait time.Duration
	// UserAgent is set on requests lacking one; "" means "gitmirror".
	UserAgent string
	// Sleep waits for d or until ctx ends (test hook). nil uses a timer.
	Sleep func(ctx context.Context, d time.Duration) error
	// Now returns the current time (test hook). nil means time.Now.
	Now func() time.Time
	// Rand returns a jitter value in [0,1) (test hook). nil uses math/rand/v2.
	Rand func() float64
}

var defaultBase = DefaultBase()

// DefaultBase returns a new http.Transport cloned from http.DefaultTransport
// with conservative timeouts suitable for API clients.
func DefaultBase() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = http.ProxyFromEnvironment
	t.DialContext = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	t.TLSHandshakeTimeout = 10 * time.Second
	t.ResponseHeaderTimeout = 60 * time.Second
	t.IdleConnTimeout = 90 * time.Second
	t.MaxIdleConnsPerHost = 8
	t.ForceAttemptHTTP2 = true
	return t
}

// NewClient returns a new http.Client that wraps base's transport in a
// retrying Transport. base is never mutated and may be nil. The client has no
// overall Timeout so large downloads work; callers should use contexts.
func NewClient(base *http.Client) *http.Client {
	c := &http.Client{}
	var rt http.RoundTripper
	if base != nil {
		rt = base.Transport
		c.Jar = base.Jar
		c.CheckRedirect = base.CheckRedirect
	}
	if rt == nil {
		rt = DefaultBase()
	}
	c.Transport = &Transport{Base: rt}
	return c
}

func (t *Transport) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}

func (t *Transport) sleep(ctx context.Context, d time.Duration) error {
	if t.Sleep != nil {
		return t.Sleep(ctx, d)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (t *Transport) jitter() float64 {
	if t.Rand != nil {
		return t.Rand()
	}
	return rand.Float64()
}

func (t *Transport) base() http.RoundTripper {
	if t.Base != nil {
		return t.Base
	}
	return defaultBase
}

func retryableMethod(req *http.Request) bool {
	switch req.Method {
	case "", http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodPut, http.MethodDelete:
		return true
	}
	v, _ := req.Context().Value(idempotentKey{}).(bool)
	return v
}

func drainClose(resp *http.Response) {
	_, _ = io.CopyN(io.Discard, resp.Body, drainLimit)
	_ = resp.Body.Close()
}

func stripURL(u *url.URL) string {
	c := *u
	c.User = nil
	c.RawQuery = ""
	c.ForceQuery = false
	c.Fragment = ""
	c.RawFragment = ""
	return c.String()
}

// RoundTrip implements http.RoundTripper.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	maxRetries := t.MaxRetries
	if maxRetries == 0 {
		maxRetries = defaultMaxRetries
	}
	maxWait := t.MaxWait
	if maxWait <= 0 {
		maxWait = defaultMaxWait
	}
	ua := t.UserAgent
	if ua == "" {
		ua = defaultUserAgent
	}
	canRetry := maxRetries > 0 && retryableMethod(req) &&
		(req.Body == nil || req.Body == http.NoBody || req.GetBody != nil)

	ctx := req.Context()
	base := t.base()

	for attempt := 0; ; attempt++ {
		r := req.Clone(ctx)
		if r.Header.Get("User-Agent") == "" {
			r.Header.Set("User-Agent", ua)
		}
		if attempt > 0 && req.GetBody != nil {
			body, err := req.GetBody()
			if err != nil {
				return nil, err
			}
			r.Body = body
		}

		resp, err := base.RoundTrip(r)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil, err
			}
			if !canRetry || attempt >= maxRetries {
				return nil, err
			}
			if serr := t.sleep(ctx, t.backoff(attempt)); serr != nil {
				return nil, serr
			}
			continue
		}

		if !retryableStatus(resp) || !canRetry {
			return resp, nil
		}

		wait, reset, limited := t.rateLimitWait(resp)
		if limited && wait > maxWait {
			drainClose(resp)
			return nil, &RateLimitError{Reset: reset, Status: resp.StatusCode, URL: stripURL(req.URL)}
		}
		if attempt >= maxRetries {
			return resp, nil
		}
		if !limited {
			wait = t.backoff(attempt)
		}
		drainClose(resp)
		if serr := t.sleep(ctx, wait); serr != nil {
			return nil, serr
		}
	}
}

func retryableStatus(resp *http.Response) bool {
	switch resp.StatusCode {
	case http.StatusTooManyRequests, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	case http.StatusForbidden:
		return resp.Header.Get("Retry-After") != "" || remainingZero(resp)
	}
	return false
}

func remainingZero(resp *http.Response) bool {
	v := strings.TrimSpace(resp.Header.Get("X-RateLimit-Remaining"))
	return v == "0"
}

// rateLimitWait derives the server-directed wait. limited is false when the
// response carries no usable Retry-After / reset information.
func (t *Transport) rateLimitWait(resp *http.Response) (wait time.Duration, reset time.Time, limited bool) {
	now := t.now()
	if ra := strings.TrimSpace(resp.Header.Get("Retry-After")); ra != "" {
		if secs, err := strconv.ParseInt(ra, 10, 64); err == nil && secs >= 0 {
			wait = time.Duration(secs) * time.Second
			return wait, now.Add(wait), true
		}
		if at, err := http.ParseTime(ra); err == nil {
			wait = max(at.Sub(now), 0)
			return wait, at, true
		}
	}
	if remainingZero(resp) {
		if secs, err := strconv.ParseInt(strings.TrimSpace(resp.Header.Get("X-RateLimit-Reset")), 10, 64); err == nil {
			reset = time.Unix(secs, 0)
			wait = max(reset.Add(time.Second).Sub(now), 0)
			return wait, reset, true
		}
	}
	return 0, time.Time{}, false
}

func (t *Transport) backoff(attempt int) time.Duration {
	d := backoffCap
	if attempt < 6 {
		d = min(backoffBase<<uint(attempt), backoffCap)
	}
	return time.Duration(float64(d) * (0.5 + 0.5*t.jitter()))
}
