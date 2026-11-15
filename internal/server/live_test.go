package server

import (
	"context"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"gitmirror/internal/app"
	"gitmirror/internal/config"
	"gitmirror/internal/forge"
	"gitmirror/internal/resolve"
)

// watchReschedule records the scheduler's next-run times after every
// configuration change it applied.
func watchReschedule(s *Server) chan map[string]time.Time {
	ch := make(chan map[string]time.Time, 16)
	s.onReschedule = func(next map[string]time.Time) { ch <- maps.Clone(next) }
	return ch
}

func nextSchedule(t *testing.T, ch chan map[string]time.Time) map[string]time.Time {
	t.Helper()
	select {
	case m := <-ch:
		return m
	case <-time.After(30 * time.Second):
		t.Fatal("scheduler did not apply the change")
		return nil
	}
}

func putRule(t *testing.T, a *app.App, r config.Rule) {
	t.Helper()
	if err := a.UpdateUI(context.Background(), func(u *app.UIConfig) error { return u.PutRule(r) }); err != nil {
		t.Fatal(err)
	}
}

func uiRule1(interval string) config.Rule {
	return config.Rule{Name: "ui1", From: config.Source{Forge: "src", Owner: "carol"},
		To: config.Dest{Forge: "dst", Owner: "carolm"}, Interval: interval}
}

func TestSchedulerPicksUpAddedAndRemovedRules(t *testing.T) {
	f := newFixture(t)
	f.addSource("alice", "a")
	f.addSource("bob", "c")
	f.addSource("carol", "c1")
	s := f.server()
	changes := watchReschedule(s)
	f.start(s)
	f.initialSync(s)
	waitFor(t, 10*time.Second, func() bool { return f.clock.Waiters() == 1 }, "scheduler idle")

	putRule(t, f.app, uiRule1(""))
	next := nextSchedule(t, changes)
	if _, ok := next["ui1"]; !ok || len(next) != 3 {
		t.Fatalf("schedule after add = %v", next)
	}
	if ctr := grepMetrics(metricsText(t, s), `gitmirror_rule_errors_total{rule="ui1"}`); ctr == "" {
		t.Error("rule error metric not pre-initialised for the new rule")
	}
	// New rules start after the initial delay (1s in the fixture).
	f.clock.Advance(s.InitialDelay)
	jobs := f.waitJobs(3)
	if countRule(jobs, laneScheduled, "ui1") != 1 || !f.dstHas("carolm", "c1") {
		t.Fatalf("jobs after adding ui1 = %+v", jobs)
	}

	if err := f.app.UpdateUI(context.Background(), func(u *app.UIConfig) error { return u.DeleteRule("ui1") }); err != nil {
		t.Fatal(err)
	}
	next = nextSchedule(t, changes)
	if _, ok := next["ui1"]; ok || len(next) != 2 {
		t.Fatalf("schedule after remove = %v", next)
	}
	s.mu.Lock()
	_, kept := s.lastResolve["ui1"]
	s.mu.Unlock()
	if kept {
		t.Error("lastResolve of the removed rule was kept")
	}
	f.clock.Advance(70 * time.Minute)
	f.waitJobs(5)
	waitFor(t, 10*time.Second, func() bool { return len(s.queuedRules()) == 0 && !s.busy(laneScheduled) }, "lane idle")
	jobs = f.doneJobs()
	if countRule(jobs, laneScheduled, "ui1") != 1 || countRule(jobs, laneScheduled, "r1") != 2 {
		t.Fatalf("jobs after removing ui1 = %+v", jobs)
	}
}

func TestEditingARuleDoesNotRescheduleOthers(t *testing.T) {
	f := newFixture(t)
	f.addSource("alice", "a")
	f.addSource("bob", "c")
	s := f.server()
	changes := watchReschedule(s)
	f.start(s)
	f.initialSync(s)
	waitFor(t, 10*time.Second, func() bool { return f.clock.Waiters() == 1 }, "scheduler idle")

	putRule(t, f.app, uiRule1(""))
	before := nextSchedule(t, changes)
	f.clock.Advance(s.InitialDelay)
	f.waitJobs(3)

	// Shorten ui1's interval: only ui1 moves, to within the new interval.
	putRule(t, f.app, uiRule1("2m"))
	after := nextSchedule(t, changes)
	now := f.clock.Now()
	for _, r := range []string{"r1", "r2"} {
		if !after[r].Equal(before[r]) {
			t.Errorf("%s rescheduled by an unrelated edit: %v -> %v", r, before[r], after[r])
		}
	}
	if d := after["ui1"].Sub(now); d <= 0 || d > 2*time.Minute+12*time.Second {
		t.Errorf("ui1 next run in %v, want within the new 2m interval", d)
	}
	n := len(f.doneJobs())
	// Re-saving the same rule changes nothing either.
	putRule(t, f.app, uiRule1("2m"))
	if again := nextSchedule(t, changes); !maps.Equal(again, after) {
		t.Errorf("re-saving the same rule moved the schedule: %v -> %v", after, again)
	}
	f.clock.Advance(3 * time.Minute)
	f.waitJobs(n + 1)
	waitFor(t, 10*time.Second, func() bool { return len(s.queuedRules()) == 0 && !s.busy(laneScheduled) }, "lane idle")
	jobs := f.doneJobs()
	if countRule(jobs, laneScheduled, "ui1") != 2 || countRule(jobs, laneScheduled, "r1") != 1 {
		t.Fatalf("jobs = %+v", jobs)
	}
}

func TestRemovedRuleLeavesScheduledQueue(t *testing.T) {
	f := newFixture(t)
	f.addSource("alice", "a")
	s := f.server()
	s.InitialDelay = 24 * time.Hour // keep the scheduler out of the way
	changes := watchReschedule(s)
	f.start(s)
	putRule(t, f.app, uiRule1(""))
	nextSchedule(t, changes)

	f.src.arm()
	s.enqueueScheduled("r1")
	select {
	case <-f.src.entered:
	case <-time.After(30 * time.Second):
		t.Fatal("job did not start")
	}
	s.enqueueScheduled("ui1")
	s.enqueueScheduled("r2")
	if err := f.app.UpdateUI(context.Background(), func(u *app.UIConfig) error { return u.DeleteRule("ui1") }); err != nil {
		t.Fatal(err)
	}
	nextSchedule(t, changes)
	if got := s.queuedRules(); !slices.Equal(got, []string{"r2"}) {
		t.Fatalf("queue = %v, want [r2]", got)
	}
	f.src.open()
	f.waitJobs(2)
}

func TestWebhookRoutesToRuleAddedAtRuntime(t *testing.T) {
	f := newFixture(t)
	s := f.server()
	s.init()
	h := s.Handler()
	if rec := hook(h, "src", "push", "carol", "c1"); !strings.Contains(rec.Body.String(), "ignored") {
		t.Fatalf("before: %d %s", rec.Code, rec.Body)
	}
	putRule(t, f.app, config.Rule{Name: "ui1", From: config.Source{Forge: "src", Repo: "carol/c1"}, To: config.Dest{Forge: "dst", Owner: "m"}})
	rec := hook(h, "src", "push", "carol", "c1")
	if rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), "accepted") {
		t.Fatalf("after: %d %s", rec.Code, rec.Body)
	}

	// A removed rule's remembered pairs no longer route.
	s.rememberResolve([]string{"ui1"}, resolve.Result{Pairs: []resolve.Pair{{Rule: "ui1", SrcForge: "src", Src: forge.Repo{Owner: "carol", Name: "c1"}}}, Complete: map[string]bool{"ui1": true}})
	if err := f.app.UpdateUI(context.Background(), func(u *app.UIConfig) error { return u.DeleteRule("ui1") }); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	clear(s.whPending)
	s.whOrder = nil
	s.mu.Unlock()
	if rec := hook(h, "src", "push", "carol", "c1"); !strings.Contains(rec.Body.String(), "ignored") {
		t.Fatalf("after removal: %d %s", rec.Code, rec.Body)
	}
}
