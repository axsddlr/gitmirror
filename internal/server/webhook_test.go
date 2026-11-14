package server

import (
	"bytes"
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"gitmirror/internal/forge"
	"gitmirror/internal/forge/fake"
)

func TestWebhookDisabledIs404(t *testing.T) {
	f := newFixtureSecret(t, "")
	s := f.server()
	rec := hook(s.Handler(), "src", "push", "alice", "a")
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "webhooks are not configured") {
		t.Fatalf("got %d %q", rec.Code, rec.Body.String())
	}
	if m := metricsText(t, s); !strings.Contains(m, `gitmirror_webhooks_total{forge="src",result="disabled"} 1`) {
		t.Fatalf("metrics:\n%s", grepMetrics(m, "gitmirror_webhooks_total"))
	}
}

func TestWebhookUnknownForgeIs404(t *testing.T) {
	f := newFixture(t)
	s := f.server()
	if rec := hook(s.Handler(), "nope", "push", "alice", "a"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown forge: %d", rec.Code)
	}
	if rec := hook(s.Handler(), "plain", "push", "alice", "a"); rec.Code != http.StatusNotFound {
		t.Fatalf("forge without webhook support: %d", rec.Code)
	}
}

func TestWebhookMethodNotAllowed(t *testing.T) {
	f := newFixture(t)
	h := f.server().Handler()
	if rec := get(h, "/webhook/src"); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /webhook/src = %d", rec.Code)
	}
	for _, p := range []string{"/healthz", "/readyz"} {
		if rec := do(h, http.MethodPost, p, nil, nil); rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("POST %s = %d", p, rec.Code)
		}
	}
	if rec := get(h, "/nope"); rec.Code != http.StatusNotFound {
		t.Fatalf("GET /nope = %d", rec.Code)
	}
}

func TestWebhookBadSignatureIs401(t *testing.T) {
	f := newFixture(t)
	s := f.server()
	body := []byte(`{"kind":"push","owner":"alice","name":"a"}`)
	rec := do(s.Handler(), http.MethodPost, "/webhook/src", body, map[string]string{fake.SignatureHeader: fake.Sign("wrong", body)})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("got %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "signature") || strings.Contains(rec.Body.String(), "fake") {
		t.Fatalf("401 body leaks detail: %q", rec.Body.String())
	}
	rec = do(s.Handler(), http.MethodPost, "/webhook/src", body, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unsigned: got %d", rec.Code)
	}
	if m := metricsText(t, s); !strings.Contains(m, `gitmirror_webhooks_total{forge="src",result="unauthorized"} 2`) {
		t.Fatalf("metrics:\n%s", grepMetrics(m, "gitmirror_webhooks_total"))
	}
}

func TestWebhookBadPayloadIs400(t *testing.T) {
	f := newFixture(t)
	s := f.server()
	body := []byte(`not json`)
	rec := do(s.Handler(), http.MethodPost, "/webhook/src", body, map[string]string{fake.SignatureHeader: fake.Sign(secret, body)})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestWebhookOversizeIs413(t *testing.T) {
	f := newFixture(t)
	s := f.server()
	body := bytes.Repeat([]byte("x"), maxWebhookBody+1)
	rec := do(s.Handler(), http.MethodPost, "/webhook/src", body, map[string]string{fake.SignatureHeader: fake.Sign(secret, body)})
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("got %d", rec.Code)
	}
	if m := metricsText(t, s); !strings.Contains(m, `gitmirror_webhooks_total{forge="src",result="too_large"} 1`) {
		t.Fatalf("metrics:\n%s", grepMetrics(m, "gitmirror_webhooks_total"))
	}
}

func TestWebhookPingIs204(t *testing.T) {
	f := newFixture(t)
	s := f.server()
	if rec := hook(s.Handler(), "src", "ping", "", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestWebhookUnmatchedIsIgnored(t *testing.T) {
	f := newFixture(t)
	s := f.server()
	rec := hook(s.Handler(), "src", "push", "zed", "x")
	if rec.Code != http.StatusAccepted || strings.TrimSpace(rec.Body.String()) != "ignored" {
		t.Fatalf("got %d %q", rec.Code, rec.Body.String())
	}
	if d := s.webhookDepth(); d != 0 {
		t.Fatalf("webhook queue = %d", d)
	}
	if q := s.queuedRules(); len(q) != 0 {
		t.Fatalf("scheduled queue = %v", q)
	}
}

func TestWebhookPushRunsTargetedSyncWithoutReconcile(t *testing.T) {
	f := newFixture(t)
	f.addSource("alice", "a")
	f.addSource("alice", "b")
	s := f.server()
	f.start(s)
	f.initialSync(s)

	// b's source disappears; only a full (reconciling) sync would notice.
	f.src.RemoveRepo("alice", "b")
	bKey := key("r1", "alice", "b", "mirrors")

	rec := hook(s.Handler(), "src", "push", "ALICE", "A")
	if rec.Code != http.StatusAccepted || strings.TrimSpace(rec.Body.String()) != "accepted" {
		t.Fatalf("got %d %q", rec.Code, rec.Body.String())
	}
	f.clock.Advance(s.Debounce)
	jobs := f.waitJobs(3)
	j := jobs[2]
	if j.Lane != laneWebhook || !slices.Equal(j.Job.Rules, []string{"r1"}) || !slices.Equal(j.Job.Repos, []string{"alice/a"}) || j.Job.Trigger != "webhook" {
		t.Fatalf("webhook job = %+v", j)
	}
	if _, ok, err := f.app.Store.MissingSince(context.Background(), bKey); err != nil || ok {
		t.Fatalf("MissingSince(b) = %v, %v: the webhook path must not reconcile", ok, err)
	}
	runs := f.runs(key("r1", "alice", "a", "mirrors"))
	if len(runs) != 2 || runs[0].Trigger != "webhook" {
		t.Fatalf("runs(a) = %+v", runs)
	}
	if m := metricsText(t, s); !strings.Contains(m, `gitmirror_webhooks_total{forge="src",result="accepted"} 1`) {
		t.Fatalf("metrics:\n%s", grepMetrics(m, "gitmirror_webhooks_total"))
	}
}

func TestWebhookDebounceCollapsesPushes(t *testing.T) {
	f := newFixture(t)
	f.addSource("alice", "a")
	s := f.server()
	f.start(s)
	f.initialSync(s)

	for range 5 {
		if rec := hook(s.Handler(), "src", "push", "alice", "a"); rec.Code != http.StatusAccepted {
			t.Fatalf("got %d", rec.Code)
		}
	}
	if d := s.webhookDepth(); d != 1 {
		t.Fatalf("webhook queue = %d, want 1", d)
	}
	if n := len(f.doneJobs()); n != 2 {
		t.Fatalf("a webhook job ran before the debounce window ended (%d jobs)", n)
	}
	f.clock.Advance(s.Debounce)
	f.waitJobs(3)
	waitFor(t, 10*time.Second, func() bool { return s.webhookDepth() == 0 && !s.busy(laneWebhook) }, "webhook lane idle")
	jobs := f.doneJobs()
	if len(jobs) != 3 || jobs[2].Lane != laneWebhook {
		t.Fatalf("jobs = %+v, want exactly one webhook job", jobs)
	}
	if runs := f.runs(key("r1", "alice", "a", "mirrors")); len(runs) != 2 {
		t.Fatalf("runs = %d, want 2 (initial + one webhook)", len(runs))
	}
}

func TestWebhookUnknownRepoOnOwnerRuleRunsFullSync(t *testing.T) {
	f := newFixture(t)
	f.addSource("alice", "a")
	s := f.server()
	f.start(s)
	f.initialSync(s)

	f.addSource("alice", "fresh")
	rec := hook(s.Handler(), "src", string(forge.EventRepo), "alice", "fresh")
	if rec.Code != http.StatusAccepted || strings.TrimSpace(rec.Body.String()) != "accepted" {
		t.Fatalf("got %d %q", rec.Code, rec.Body.String())
	}
	jobs := f.waitJobs(3)
	j := jobs[2]
	if j.Lane != laneScheduled || !slices.Equal(j.Job.Rules, []string{"r1"}) || len(j.Job.Repos) != 0 {
		t.Fatalf("job = %+v, want a full r1 sync", j)
	}
	if !f.dstHas("mirrors", "fresh") {
		t.Fatal("new repo not mirrored")
	}

	// Now it is in r1's last resolve, so a push targets it.
	hook(s.Handler(), "src", "push", "alice", "fresh")
	f.clock.Advance(s.Debounce)
	jobs = f.waitJobs(4)
	if j := jobs[3]; j.Lane != laneWebhook || !slices.Equal(j.Job.Repos, []string{"alice/fresh"}) {
		t.Fatalf("job = %+v", j)
	}
}

func TestWebhookRejectedDuringShutdown(t *testing.T) {
	f := newFixture(t)
	s := f.server()
	s.init()
	s.stopping.Store(true)
	if rec := hook(s.Handler(), "src", "push", "alice", "a"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d", rec.Code)
	}
}
