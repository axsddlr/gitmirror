package httpx

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var fixedNow = time.Unix(1_700_000_000, 0)

type harness struct {
	tr     *Transport
	sleeps []time.Duration
}

func newHarness() *harness {
	h := &harness{}
	h.tr = &Transport{
		Sleep: func(ctx context.Context, d time.Duration) error {
			h.sleeps = append(h.sleeps, d)
			return ctx.Err()
		},
		Now:  func() time.Time { return fixedNow },
		Rand: func() float64 { return 0 }, // jitter multiplier 0.5
	}
	return h
}

func (h *harness) do(t *testing.T, method, url string, body io.Reader) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	return h.tr.RoundTrip(req)
}

// scripted serves the given statuses in order (last repeats) with optional headers.
func scripted(hits *int32, statuses []int, hdr http.Header) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(atomic.AddInt32(hits, 1)) - 1
		if n >= len(statuses) {
			n = len(statuses) - 1
		}
		for k, v := range hdr {
			w.Header()[k] = v
		}
		w.WriteHeader(statuses[n])
		_, _ = w.Write([]byte("body"))
	}))
}

func TestPOSTNeverRetried(t *testing.T) {
	var hits int32
	srv := scripted(&hits, []int{503}, nil)
	defer srv.Close()
	h := newHarness()
	resp, err := h.do(t, "POST", srv.URL, strings.NewReader("x"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 503 || atomic.LoadInt32(&hits) != 1 || len(h.sleeps) != 0 {
		t.Fatalf("status=%d hits=%d sleeps=%v", resp.StatusCode, atomic.LoadInt32(&hits), h.sleeps)
	}
}

func TestPATCHNeverRetried(t *testing.T) {
	var hits int32
	srv := scripted(&hits, []int{429}, nil)
	defer srv.Close()
	h := newHarness()
	resp, err := h.do(t, "PATCH", srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if atomic.LoadInt32(&hits) != 1 {
		t.Fatalf("hits=%d", atomic.LoadInt32(&hits))
	}
}

func TestWithIdempotentRetriesPATCH(t *testing.T) {
	var hits int32
	srv := scripted(&hits, []int{503, 200}, nil)
	defer srv.Close()
	h := newHarness()
	req, _ := http.NewRequestWithContext(WithIdempotent(context.Background()), "PATCH", srv.URL, strings.NewReader("x"))
	resp, err := h.tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || atomic.LoadInt32(&hits) != 2 {
		t.Fatalf("status=%d hits=%d", resp.StatusCode, atomic.LoadInt32(&hits))
	}
}

func TestRetryableMethodsRetry(t *testing.T) {
	for _, m := range []string{"GET", "HEAD", "OPTIONS", "PUT", "DELETE"} {
		t.Run(m, func(t *testing.T) {
			var hits int32
			srv := scripted(&hits, []int{502, 200}, nil)
			defer srv.Close()
			h := newHarness()
			resp, err := h.do(t, m, srv.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != 200 || atomic.LoadInt32(&hits) != 2 {
				t.Fatalf("status=%d hits=%d", resp.StatusCode, atomic.LoadInt32(&hits))
			}
		})
	}
}

func TestRetryStatuses(t *testing.T) {
	for _, code := range []int{429, 502, 503, 504} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			var hits int32
			srv := scripted(&hits, []int{code, 200}, nil)
			defer srv.Close()
			h := newHarness()
			resp, err := h.do(t, "GET", srv.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != 200 || atomic.LoadInt32(&hits) != 2 {
				t.Fatalf("status=%d hits=%d", resp.StatusCode, atomic.LoadInt32(&hits))
			}
		})
	}
}

func TestNonRetryableStatus(t *testing.T) {
	var hits int32
	srv := scripted(&hits, []int{500}, nil)
	defer srv.Close()
	h := newHarness()
	resp, err := h.do(t, "GET", srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 500 || atomic.LoadInt32(&hits) != 1 {
		t.Fatalf("status=%d hits=%d", resp.StatusCode, atomic.LoadInt32(&hits))
	}
}

func TestPlain403NotRetried(t *testing.T) {
	var hits int32
	srv := scripted(&hits, []int{403}, nil)
	defer srv.Close()
	h := newHarness()
	resp, err := h.do(t, "GET", srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 || atomic.LoadInt32(&hits) != 1 {
		t.Fatalf("status=%d hits=%d", resp.StatusCode, atomic.LoadInt32(&hits))
	}
}

func TestRetryAfter403Retried(t *testing.T) {
	var hits int32
	srv := scripted(&hits, []int{403, 200}, http.Header{"Retry-After": {"3"}})
	defer srv.Close()
	h := newHarness()
	resp, err := h.do(t, "GET", srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if atomic.LoadInt32(&hits) != 2 || len(h.sleeps) != 1 || h.sleeps[0] != 3*time.Second {
		t.Fatalf("hits=%d sleeps=%v", atomic.LoadInt32(&hits), h.sleeps)
	}
}

func TestRemainingZero403UsesResetFromNow(t *testing.T) {
	var hits int32
	reset := fixedNow.Add(30 * time.Second).Unix()
	srv := scripted(&hits, []int{403, 200}, http.Header{
		"X-Ratelimit-Remaining": {"0"},
		"X-Ratelimit-Reset":     {strconv.FormatInt(reset, 10)},
	})
	defer srv.Close()
	h := newHarness()
	resp, err := h.do(t, "GET", srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if atomic.LoadInt32(&hits) != 2 || len(h.sleeps) != 1 || h.sleeps[0] != 31*time.Second {
		t.Fatalf("hits=%d sleeps=%v", atomic.LoadInt32(&hits), h.sleeps)
	}
}

func TestRetryAfterHTTPDate(t *testing.T) {
	var hits int32
	date := fixedNow.Add(42 * time.Second).UTC().Format(http.TimeFormat)
	srv := scripted(&hits, []int{429, 200}, http.Header{"Retry-After": {date}})
	defer srv.Close()
	h := newHarness()
	resp, err := h.do(t, "GET", srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(h.sleeps) != 1 || h.sleeps[0] != 42*time.Second {
		t.Fatalf("sleeps=%v", h.sleeps)
	}
}

func TestExponentialBackoffWithJitter(t *testing.T) {
	var hits int32
	srv := scripted(&hits, []int{503}, nil)
	defer srv.Close()
	h := newHarness()
	h.tr.MaxRetries = 4
	resp, err := h.do(t, "GET", srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	want := []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second}
	if len(h.sleeps) != len(want) {
		t.Fatalf("sleeps=%v", h.sleeps)
	}
	for i := range want {
		if h.sleeps[i] != want[i] {
			t.Fatalf("sleeps=%v want %v", h.sleeps, want)
		}
	}
}

func TestBackoffCappedAt30s(t *testing.T) {
	var hits int32
	srv := scripted(&hits, []int{503}, nil)
	defer srv.Close()
	h := newHarness()
	h.tr.MaxRetries = 8
	h.tr.Rand = func() float64 { return 0.999999 }
	resp, err := h.do(t, "GET", srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	var max time.Duration
	for _, d := range h.sleeps {
		if d > max {
			max = d
		}
	}
	if max > 30*time.Second || max < 29*time.Second {
		t.Fatalf("max sleep %v, sleeps=%v", max, h.sleeps)
	}
}

func TestExhaustedReturnsLastResponseIntact(t *testing.T) {
	var hits int32
	srv := scripted(&hits, []int{503}, nil)
	defer srv.Close()
	h := newHarness()
	h.tr.MaxRetries = 2
	resp, err := h.do(t, "GET", srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 503 || string(b) != "body" || atomic.LoadInt32(&hits) != 3 {
		t.Fatalf("status=%d body=%q hits=%d", resp.StatusCode, b, atomic.LoadInt32(&hits))
	}
}

func hijackAndClose(hits *int32) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		c, _, _ := w.(http.Hijacker).Hijack()
		c.Close()
	}))
}

func TestTransportErrorRetriedThenReturned(t *testing.T) {
	var hits int32
	srv := hijackAndClose(&hits)
	defer srv.Close()
	h := newHarness()
	h.tr.MaxRetries = 2
	h.tr.Base = &http.Transport{DisableKeepAlives: true}
	_, err := h.do(t, "GET", srv.URL, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if atomic.LoadInt32(&hits) != 3 || len(h.sleeps) != 2 {
		t.Fatalf("hits=%d sleeps=%v", atomic.LoadInt32(&hits), h.sleeps)
	}
}

func TestTransportErrorNotRetriedForPOST(t *testing.T) {
	var hits int32
	srv := hijackAndClose(&hits)
	defer srv.Close()
	h := newHarness()
	h.tr.Base = &http.Transport{DisableKeepAlives: true}
	if _, err := h.do(t, "POST", srv.URL, nil); err == nil || atomic.LoadInt32(&hits) != 1 {
		t.Fatalf("err=%v hits=%d", err, atomic.LoadInt32(&hits))
	}
}

func TestRateLimitErrorBeyondMaxWait(t *testing.T) {
	var hits int32
	reset := fixedNow.Add(2 * time.Hour).Unix()
	srv := scripted(&hits, []int{403}, http.Header{
		"X-Ratelimit-Remaining": {"0"},
		"X-Ratelimit-Reset":     {strconv.FormatInt(reset, 10)},
	})
	defer srv.Close()
	h := newHarness()
	req, _ := http.NewRequest("GET", srv.URL+"/x?access_token=SECRET", nil)
	resp, err := h.tr.RoundTrip(req)
	if resp != nil {
		resp.Body.Close()
		t.Fatal("expected nil response")
	}
	var rl *RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("err=%v", err)
	}
	if rl.Status != 403 || !rl.Reset.Equal(time.Unix(reset, 0)) {
		t.Fatalf("%+v", rl)
	}
	msg := err.Error()
	if strings.Contains(msg, "SECRET") || !strings.Contains(msg, "403") || !strings.Contains(msg, srv.URL+"/x") {
		t.Fatalf("msg=%q", msg)
	}
	if strings.Contains(rl.URL, "SECRET") || strings.Contains(rl.URL, "?") {
		t.Fatalf("URL=%q", rl.URL)
	}
	if atomic.LoadInt32(&hits) != 1 || len(h.sleeps) != 0 {
		t.Fatalf("hits=%d sleeps=%v", atomic.LoadInt32(&hits), h.sleeps)
	}
}

func TestRetryAfterBeyondMaxWait(t *testing.T) {
	var hits int32
	srv := scripted(&hits, []int{429}, http.Header{"Retry-After": {"7200"}})
	defer srv.Close()
	h := newHarness()
	h.tr.MaxWait = time.Minute
	_, err := h.do(t, "GET", srv.URL, nil)
	var rl *RateLimitError
	if !errors.As(err, &rl) || rl.Status != 429 {
		t.Fatalf("err=%v", err)
	}
}

func TestNegativeMaxRetriesSingleAttempt(t *testing.T) {
	var hits int32
	srv := scripted(&hits, []int{503}, nil)
	defer srv.Close()
	h := newHarness()
	h.tr.MaxRetries = -1
	resp, err := h.do(t, "GET", srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if atomic.LoadInt32(&hits) != 1 || resp.StatusCode != 503 {
		t.Fatalf("hits=%d", atomic.LoadInt32(&hits))
	}
}

func TestDefaultMaxRetriesIsFour(t *testing.T) {
	var hits int32
	srv := scripted(&hits, []int{503}, nil)
	defer srv.Close()
	h := newHarness()
	resp, err := h.do(t, "GET", srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if atomic.LoadInt32(&hits) != 5 {
		t.Fatalf("hits=%d", atomic.LoadInt32(&hits))
	}
}

func TestBodyWithoutGetBodyNotRetried(t *testing.T) {
	var hits int32
	srv := scripted(&hits, []int{503}, nil)
	defer srv.Close()
	h := newHarness()
	req, _ := http.NewRequest("PUT", srv.URL, io.NopCloser(strings.NewReader("data")))
	if req.GetBody != nil {
		t.Fatal("test setup: GetBody should be nil")
	}
	resp, err := h.tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if atomic.LoadInt32(&hits) != 1 {
		t.Fatalf("hits=%d", atomic.LoadInt32(&hits))
	}
}

func TestBodyResentOnRetry(t *testing.T) {
	var hits int32
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		if atomic.AddInt32(&hits, 1) == 1 {
			w.WriteHeader(503)
			return
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()
	h := newHarness()
	resp, err := h.do(t, "PUT", srv.URL, strings.NewReader("payload"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(bodies) != 2 || bodies[0] != "payload" || bodies[1] != "payload" {
		t.Fatalf("bodies=%q", bodies)
	}
}

func TestContextCancelDuringWait(t *testing.T) {
	var hits int32
	srv := scripted(&hits, []int{503}, nil)
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	tr := &Transport{
		Sleep: func(ctx context.Context, d time.Duration) error {
			cancel()
			return ctx.Err()
		},
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL, nil)
	resp, err := tr.RoundTrip(req)
	if resp != nil {
		resp.Body.Close()
	}
	if !errors.Is(err, context.Canceled) || atomic.LoadInt32(&hits) != 1 {
		t.Fatalf("err=%v hits=%d", err, atomic.LoadInt32(&hits))
	}
}

func TestContextCancelledNotRetried(t *testing.T) {
	var hits int32
	srv := scripted(&hits, []int{200}, nil)
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h := newHarness()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL, nil)
	_, err := h.tr.RoundTrip(req)
	if !errors.Is(err, context.Canceled) || len(h.sleeps) != 0 {
		t.Fatalf("err=%v sleeps=%v", err, h.sleeps)
	}
}

func TestDefaultSleepHonoursCancel(t *testing.T) {
	var hits int32
	srv := scripted(&hits, []int{503}, http.Header{"Retry-After": {"600"}})
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	tr := &Transport{}
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL, nil)
	start := time.Now()
	_, err := tr.RoundTrip(req)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 5*time.Second {
		t.Fatalf("err=%v", err)
	}
}

func TestDoesNotMutateRequest(t *testing.T) {
	var ua string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ua = r.Header.Get("User-Agent")
	}))
	defer srv.Close()
	h := newHarness()
	req, _ := http.NewRequest("GET", srv.URL, nil)
	resp, err := h.tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(req.Header) != 0 {
		t.Fatalf("caller header mutated: %v", req.Header)
	}
	if ua != "gitmirror" {
		t.Fatalf("ua=%q", ua)
	}
}

func TestExistingUserAgentKept(t *testing.T) {
	var ua string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ua = r.Header.Get("User-Agent")
	}))
	defer srv.Close()
	h := newHarness()
	h.tr.UserAgent = "custom"
	req, _ := http.NewRequest("GET", srv.URL, nil)
	req.Header.Set("User-Agent", "mine")
	resp, err := h.tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if ua != "mine" {
		t.Fatalf("ua=%q", ua)
	}
}

type countingRT struct {
	n    int32
	next http.RoundTripper
}

func (c *countingRT) RoundTrip(r *http.Request) (*http.Response, error) {
	atomic.AddInt32(&c.n, 1)
	return c.next.RoundTrip(r)
}

func TestEveryAttemptGoesThroughBase(t *testing.T) {
	var hits int32
	srv := scripted(&hits, []int{503, 503, 200}, nil)
	defer srv.Close()
	h := newHarness()
	base := &countingRT{next: http.DefaultTransport}
	h.tr.Base = base
	resp, err := h.do(t, "GET", srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if base.n != 3 {
		t.Fatalf("base calls=%d", base.n)
	}
}

func TestConcurrentUse(t *testing.T) {
	var hits int32
	srv := scripted(&hits, []int{503, 200}, nil)
	defer srv.Close()
	tr := &Transport{Sleep: func(context.Context, time.Duration) error { return nil }}
	done := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func() {
			req, _ := http.NewRequest("GET", srv.URL, nil)
			resp, err := tr.RoundTrip(req)
			if err == nil {
				resp.Body.Close()
			}
			done <- err
		}()
	}
	for i := 0; i < 8; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

func TestNewClientDoesNotMutateBase(t *testing.T) {
	rt := &countingRT{next: http.DefaultTransport}
	base := &http.Client{Transport: rt, Timeout: 5 * time.Second}
	c := NewClient(base)
	if c == base {
		t.Fatal("same client")
	}
	if base.Transport != http.RoundTripper(rt) || base.Timeout != 5*time.Second {
		t.Fatal("base mutated")
	}
	tr, ok := c.Transport.(*Transport)
	if !ok || tr.Base != http.RoundTripper(rt) {
		t.Fatalf("transport=%#v", c.Transport)
	}
	if c.Timeout != 0 {
		t.Fatalf("timeout=%v", c.Timeout)
	}
}

func TestNewClientNilAndCopies(t *testing.T) {
	c := NewClient(nil)
	tr, ok := c.Transport.(*Transport)
	if !ok || tr.Base == nil {
		t.Fatalf("transport=%#v", c.Transport)
	}
	cr := func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	c2 := NewClient(&http.Client{CheckRedirect: cr})
	if c2.CheckRedirect == nil {
		t.Fatal("CheckRedirect not copied")
	}
}

func TestDefaultBase(t *testing.T) {
	b := DefaultBase()
	if b.TLSHandshakeTimeout != 10*time.Second || b.ResponseHeaderTimeout != 60*time.Second ||
		b.IdleConnTimeout != 90*time.Second || b.MaxIdleConnsPerHost != 8 || !b.ForceAttemptHTTP2 || b.Proxy == nil {
		t.Fatalf("%+v", b)
	}
	if DefaultBase() == b {
		t.Fatal("must return new transport")
	}
}
