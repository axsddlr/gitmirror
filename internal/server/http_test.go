package server

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"gitmirror/internal/forge"
	"gitmirror/internal/state"
)

func metricsText(t *testing.T, s *Server) string {
	t.Helper()
	rec := get(s.Handler(), "/metrics")
	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics = %d", rec.Code)
	}
	return rec.Body.String()
}

func grepMetrics(m, prefix string) string {
	var out []string
	for _, l := range strings.Split(m, "\n") {
		if strings.HasPrefix(l, prefix) {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

func TestHealthAndReadiness(t *testing.T) {
	f := newFixture(t)
	s := f.server()
	h := s.Handler()
	if rec := get(h, "/healthz"); rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "ok" {
		t.Fatalf("/healthz before Run = %d %q", rec.Code, rec.Body.String())
	}
	if rec := get(h, "/readyz"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz before Run = %d", rec.Code)
	}
	r := f.start(s)
	if s.Addr() == nil || !strings.HasPrefix(s.Addr().String(), "127.0.0.1:") {
		t.Fatalf("Addr = %v", s.Addr())
	}
	// Real HTTP round trip on the listener.
	resp, err := http.Get("http://" + s.Addr().String() + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /healthz over TCP = %d", resp.StatusCode)
	}
	if rec := get(h, "/readyz"); rec.Code != http.StatusOK {
		t.Fatalf("/readyz while running = %d", rec.Code)
	}
	if err := r.stop(t, 30*time.Second); err != nil {
		t.Fatalf("Run = %v", err)
	}
	if rec := get(h, "/readyz"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz after shutdown = %d", rec.Code)
	}
}

func TestMetricsAfterSync(t *testing.T) {
	f := newFixture(t)
	f.addSource("alice", "a")
	f.addSource("bob", "c")
	f.src.SetRate(forge.Rate{Limit: 5000, Remaining: 4321, Reset: time.Now().Add(time.Hour)})
	s := f.server()
	f.start(s)
	f.initialSync(s)
	// A bad signature so the webhook counter has a sample.
	do(s.Handler(), http.MethodPost, "/webhook/src", []byte(`{}`), nil)

	m := metricsText(t, s)
	t.Logf("metrics excerpt:\n%s", grepMetrics(m, "gitmirror_"))
	for _, want := range []string{
		`gitmirror_sync_runs_total{rule="r1",status="ok"} 1`,
		`gitmirror_sync_runs_total{rule="r2",status="ok"} 1`,
		`gitmirror_sync_duration_seconds_count{rule="r1"} 1`,
		`gitmirror_sync_duration_seconds_bucket{rule="r1",le="+Inf"} 1`,
		`gitmirror_pair_last_success_timestamp_seconds{dst="dst:mirrors/a",rule="r1",src="src:alice/a"} `,
		`gitmirror_pair_consecutive_failures{dst="dst:mirrors/a",rule="r1",src="src:alice/a"} 0`,
		`gitmirror_queue_depth{lane="scheduled"} 0`,
		`gitmirror_queue_depth{lane="webhook"} 0`,
		`gitmirror_webhooks_total{forge="src",result="unauthorized"} 1`,
		`gitmirror_rule_errors_total{rule="r1"} 0`,
		`gitmirror_forge_rate_limit_remaining{forge="src"} 4321`,
		`go_goroutines `,
	} {
		if !strings.Contains(m, want) {
			t.Errorf("metrics lack %q", want)
		}
	}
	if strings.Contains(m, `gitmirror_forge_rate_limit_remaining{forge="dst"}`) {
		t.Error("rate limit gauge set for a forge with Limit 0")
	}
	if strings.Contains(m, sentinel) || strings.Contains(m, secret) || strings.Contains(m, "SENTINEL") {
		t.Fatal("metrics contain a secret")
	}
}

func TestRuleErrorsCounted(t *testing.T) {
	f := newFixture(t)
	f.addSource("alice", "a")
	f.src.FailAlways("ListRepos", errBoom)
	s := f.server()
	f.start(s)
	f.initialSync(s)
	m := metricsText(t, s)
	if !strings.Contains(m, `gitmirror_rule_errors_total{rule="r1"} 1`) {
		t.Fatalf("metrics:\n%s", grepMetrics(m, "gitmirror_rule_errors_total"))
	}
}

func TestGracefulShutdownWaitsForRunningJob(t *testing.T) {
	f := newFixture(t)
	f.addSource("alice", "a")
	s := f.server()
	s.InitialDelay = 24 * time.Hour
	s.ShutdownTimeout = time.Minute
	r := f.start(s)

	f.src.arm()
	s.enqueueScheduled("r1")
	select {
	case <-f.src.entered:
	case <-time.After(30 * time.Second):
		t.Fatal("job did not start")
	}
	r.cancel()
	waitFor(t, 10*time.Second, func() bool { return get(s.Handler(), "/readyz").Code == http.StatusServiceUnavailable }, "readyz 503 during shutdown")
	if rec := hook(s.Handler(), "src", "push", "alice", "a"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("webhook during shutdown = %d", rec.Code)
	}
	if rec := get(s.Handler(), "/healthz"); rec.Code != http.StatusOK {
		t.Fatalf("healthz during shutdown = %d", rec.Code)
	}
	select {
	case err := <-r.done:
		t.Fatalf("Run returned (%v) while a job was running", err)
	case <-time.After(20 * time.Millisecond):
	}
	f.src.open()
	if err := r.stop(t, 30*time.Second); err != nil {
		t.Fatalf("Run = %v", err)
	}
	runs := f.runs(key("r1", "alice", "a", "mirrors"))
	if len(runs) != 1 || runs[0].Status != state.RunOK {
		t.Fatalf("runs = %+v, want one ok run", runs)
	}
}

func TestShutdownTimeoutCancelsJob(t *testing.T) {
	f := newFixture(t)
	f.addSource("alice", "a")
	s := f.server()
	s.InitialDelay = 24 * time.Hour
	s.ShutdownTimeout = 100 * time.Millisecond
	r := f.start(s)

	f.src.arm() // never opened
	s.enqueueScheduled("r1")
	select {
	case <-f.src.entered:
	case <-time.After(30 * time.Second):
		t.Fatal("job did not start")
	}
	if err := r.stop(t, 30*time.Second); err != nil {
		t.Fatalf("Run = %v", err)
	}
	runs := f.runs(key("r1", "alice", "a", "mirrors"))
	if len(runs) != 1 || runs[0].Status != state.RunAborted {
		t.Fatalf("runs = %+v, want one aborted run", runs)
	}
}
