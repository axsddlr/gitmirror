package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"gitmirror/internal/forge/fake"
)

// blockingBody blocks in Read until release is closed.
type blockingBody struct {
	started chan<- struct{}
	release <-chan struct{}
	once    sync.Once
}

func (b *blockingBody) Read([]byte) (int, error) {
	b.once.Do(func() { b.started <- struct{}{} })
	<-b.release
	return 0, io.EOF
}

func (b *blockingBody) Close() error { return nil }

func TestWebhookBusyIs503AndRecovers(t *testing.T) {
	const limit = 3
	f := newFixture(t)
	s := f.server()
	s.MaxConcurrentWebhooks = limit
	h := s.Handler()

	before := runtime.NumGoroutine()
	started := make(chan struct{}, limit)
	release := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < limit; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/webhook/src", nil)
			req.Body = &blockingBody{started: started, release: release}
			h.ServeHTTP(httptest.NewRecorder(), req)
		}()
	}
	for i := 0; i < limit; i++ {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("slow requests did not start")
		}
	}

	rec := hook(h, "src", "push", "alice", "a")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want 503", rec.Code)
	}
	if ra := rec.Header().Get("Retry-After"); ra != "1" {
		t.Fatalf("Retry-After = %q", ra)
	}
	if m := metricsText(t, s); !strings.Contains(m, `gitmirror_webhooks_total{forge="src",result="busy"} 1`) {
		t.Fatalf("metrics:\n%s", grepMetrics(m, "gitmirror_webhooks_total"))
	}

	close(release)
	wg.Wait()

	if rec := hook(h, "src", "push", "alice", "a"); rec.Code != http.StatusAccepted {
		t.Fatalf("after release got %d", rec.Code)
	}
	waitFor(t, 5*time.Second, func() bool { return runtime.NumGoroutine() <= before+3 }, "goroutines to settle")
}

func TestWebhookBodyLimitField(t *testing.T) {
	f := newFixture(t)
	s := f.server()
	s.MaxWebhookBody = 64
	body := []byte(strings.Repeat("x", 65))
	rec := do(s.Handler(), http.MethodPost, "/webhook/src", body, map[string]string{fake.SignatureHeader: fake.Sign(secret, body)})
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("got %d", rec.Code)
	}
}
