package server

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"gitmirror/internal/forge"
	"gitmirror/internal/forge/fake"
	"gitmirror/internal/resolve"
)

// ---- metrics listener ----

func TestMetricsNotOnMainListener(t *testing.T) {
	f := newFixture(t)
	s := f.server()
	if rec := get(s.Handler(), "/metrics"); rec.Code != http.StatusNotFound {
		t.Fatalf("main GET /metrics = %d, want 404", rec.Code)
	}
	if rec := get(s.Handler(), "/healthz"); rec.Code != http.StatusOK {
		t.Fatalf("/healthz = %d", rec.Code)
	}
}

func status(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestMetricsServedOnSeparateListener(t *testing.T) {
	f := newFixture(t)
	s := f.server()
	mln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s.MetricsListener = mln
	r := f.start(s)
	base := "http://" + mln.Addr().String()
	if code, body := status(t, base+"/metrics"); code != http.StatusOK || !strings.Contains(body, "gitmirror_queue_depth") {
		t.Fatalf("metrics listener: %d %.200s", code, body)
	}
	if code, _ := status(t, base+"/healthz"); code != http.StatusNotFound {
		t.Fatalf("metrics listener /healthz = %d", code)
	}
	if code, _ := status(t, "http://"+s.Addr().String()+"/metrics"); code != http.StatusNotFound {
		t.Fatalf("main /metrics = %d", code)
	}
	if err := r.stop(t, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if c, err := net.DialTimeout("tcp", mln.Addr().String(), time.Second); err == nil {
		c.Close()
		t.Fatal("metrics listener still accepting after shutdown")
	}
}

func TestMetricsOffServesNone(t *testing.T) {
	f := newFixture(t)
	s := f.server() // no MetricsListener: metrics disabled
	f.start(s)
	if code, _ := status(t, "http://"+s.Addr().String()+"/metrics"); code != http.StatusNotFound {
		t.Fatalf("/metrics with metrics off = %d", code)
	}
}

// ---- repo: rules and webhooks ----

const repoRuleTmpl = `
state_dir: '@STATE@'
forges:
  src: {type: fake, token: @TOKEN@}
  dst: {type: fake, token: @TOKEN@}
  plain: {type: fake, token: @TOKEN@}
server:
  listen: 127.0.0.1:0
  webhook_secret: '@SECRET@'
rules:
  - name: single
    from: {forge: src, repo: Carol/Thing}
    to: {forge: dst, owner: mirrors}
  - name: other
    from: {forge: plain, repo: carol/thing}
    to: {forge: dst, owner: mirrors2}
`

func TestWebhookRoutesRepoRuleBeforeFirstSync(t *testing.T) {
	f := newFixtureTmpl(t, secret, repoRuleTmpl)
	s := f.server()
	s.init()
	if !s.route(f.app.Config(), "src", &forge.Event{Kind: forge.EventPush, Owner: "carol", Name: "THING"}) {
		t.Fatal("route returned false for a repo: rule")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.whOrder) != 1 {
		t.Fatalf("pending webhook jobs = %d, want 1 (rule on another forge must not match)", len(s.whOrder))
	}
	j := s.whOrder[0].job
	if len(j.Rules) != 1 || j.Rules[0] != "single" || len(j.Repos) != 1 || !strings.EqualFold(j.Repos[0], "carol/thing") {
		t.Fatalf("job = %+v", j)
	}
}

func TestWebhookRepoRuleNoMatchIgnored(t *testing.T) {
	f := newFixtureTmpl(t, secret, repoRuleTmpl)
	s := f.server()
	s.init()
	if s.route(f.app.Config(), "src", &forge.Event{Kind: forge.EventPush, Owner: "carol", Name: "different"}) {
		t.Fatal("route matched an unrelated repo")
	}
}

func TestRememberResolveKeepsPairsOnRuleError(t *testing.T) {
	f := newFixture(t)
	s := f.server()
	s.init()
	pair := resolve.Pair{Rule: "r1", SrcForge: "src"}
	s.rememberResolve([]string{"r1", "r2"}, resolve.Result{
		Pairs:    []resolve.Pair{pair},
		Complete: map[string]bool{"r1": true, "r2": true},
	})
	// r1 errored on the next full sync; r2 listed incompletely.
	s.rememberResolve([]string{"r1", "r2"}, resolve.Result{
		Errors:   []resolve.RuleError{{Rule: "r1", Err: errBoom}},
		Complete: map[string]bool{"r1": false, "r2": false},
	})
	s.mu.Lock()
	n := len(s.lastResolve["r1"])
	s.mu.Unlock()
	if n != 1 {
		t.Fatalf("r1 pairs lost after resolve error: %d", n)
	}
	// A complete, empty listing does clear.
	s.rememberResolve([]string{"r1"}, resolve.Result{Complete: map[string]bool{"r1": true}})
	s.mu.Lock()
	n = len(s.lastResolve["r1"])
	s.mu.Unlock()
	if n != 0 {
		t.Fatalf("complete empty listing should clear, got %d", n)
	}
}

// ---- refreshPairs serialisation ----

func TestRefreshPairsConcurrentScrape(t *testing.T) {
	f := newFixture(t)
	f.addSource("alice", "a")
	f.addSource("bob", "c")
	s := f.server()
	f.start(s)
	f.initialSync(s)
	ctx := context.Background()
	s.refreshPairs(ctx)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					s.refreshPairs(ctx)
				}
			}
		}()
	}
	defer func() { close(stop); wg.Wait() }()
	for i := 0; i < 200; i++ {
		m := metricsText(t, s)
		if n := strings.Count(grepMetrics(m, "gitmirror_pair_consecutive_failures{"), "gitmirror_pair"); n != 2 {
			t.Fatalf("scrape %d saw %d failure gauges, want 2:\n%s", i, n, grepMetrics(m, "gitmirror_pair_"))
		}
	}
}

// ---- bad-signature log rate limit ----

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func TestBadSignatureLogRateLimited(t *testing.T) {
	f := newFixture(t)
	s := f.server()
	buf := &lockedBuf{}
	s.Logger = slog.New(slog.NewTextHandler(buf, nil))
	body := []byte(`{"kind":"push","owner":"alice","name":"a"}`)
	bad := func() {
		rec := do(s.Handler(), http.MethodPost, "/webhook/src", body, map[string]string{fake.SignatureHeader: fake.Sign("wrong", body)})
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("got %d", rec.Code)
		}
	}
	for i := 0; i < 5; i++ {
		bad()
	}
	if n := strings.Count(buf.String(), "bad signature"); n != 1 {
		t.Fatalf("logged %d lines within the window, want 1:\n%s", n, buf.String())
	}
	f.clock.Advance(61 * time.Second)
	bad()
	out := buf.String()
	if n := strings.Count(out, "bad signature"); n != 2 {
		t.Fatalf("logged %d lines after window, want 2:\n%s", n, out)
	}
	if !strings.Contains(out, "suppressed=4") {
		t.Fatalf("second line lacks suppressed=4:\n%s", out)
	}
}
