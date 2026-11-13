package engine

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gitmirror/internal/config"
	"gitmirror/internal/forge"
	"gitmirror/internal/forge/fake"
	"gitmirror/internal/resolve"
	"gitmirror/internal/state"
)

// noDeleter hides the fake's forge.RepoDeleter implementation: the field
// shadows the promoted DeleteRepo method, so the wrapper's method set lacks it.
type noDeleter struct {
	*fake.Provider
	DeleteRepo struct{}
}

// testClock is a settable clock shared by the engine and the store.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// useClock installs one manual clock on both the store and the engine.
func (h *harness) useClock() *testClock {
	c := &testClock{t: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)}
	h.store.SetClock(c.Now)
	h.eng.Now = c.Now
	return c
}

func (h *harness) missingSince(k state.PairKey) (time.Time, bool) {
	h.t.Helper()
	at, ok, err := h.store.MissingSince(context.Background(), k)
	if err != nil {
		h.t.Fatal(err)
	}
	return at, ok
}

func (h *harness) runCount() int {
	h.t.Helper()
	runs, err := h.store.RecentRuns(context.Background(), 0)
	if err != nil {
		h.t.Fatal(err)
	}
	return len(runs)
}

func (h *harness) reconcileRuns(k state.PairKey) []state.Run {
	h.t.Helper()
	runs, err := h.store.Runs(context.Background(), k, 0)
	if err != nil {
		h.t.Fatal(err)
	}
	var out []state.Run
	for _, r := range runs {
		if strings.HasSuffix(r.Trigger, ":reconcile") {
			out = append(out, r)
		}
	}
	return out
}

func countCalls(calls []string, name string) int {
	n := 0
	for _, c := range calls {
		if c == name {
			n++
		}
	}
	return n
}

func TestReconcile(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	clk := h.useClock()
	dst2Base := filepath.Join(t.TempDir(), "dst2")
	dst2 := &noDeleter{Provider: fake.New(fake.Options{Type: "fakedst2", GitBaseDir: dst2Base, Username: "u", Token: "dst2token-9012"})}
	h.eng.Providers["dst2"] = dst2

	rs := func(onDeleted string, adopt bool) config.RuleSettings {
		s := settings("settings")
		s.OnSourceDeleted = onDeleted
		s.Adopt = adopt
		s.DeletionGrace = time.Hour
		return s
	}
	rules := map[string]config.RuleSettings{
		"keep":     rs("keep", false),
		"arch":     rs("archive", false),
		"del":      rs("delete", false),
		"deladopt": rs("delete", true),
		"delnd":    rs("delete", false),
		"inc":      rs("archive", false),
		"removed":  rs("archive", false),
	}
	type spec struct{ rule, name, dstForge string }
	specs := []spec{
		{"keep", "a", "dst"},
		{"arch", "b", "dst"},
		{"del", "c", "dst"},
		{"deladopt", "d", "dst"},
		{"delnd", "e", "dst2"},
		{"arch", "f", "dst"},    // still exists at the source (filtered out)
		{"inc", "g", "dst"},     // rule not complete
		{"arch", "h", "dst"},    // dropped
		{"removed", "i", "dst"}, // rule removed from config
	}
	if err := h.dst.AddRepo(forge.Repo{Owner: "mirrors", Name: "d", DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	pairs := map[string]resolve.Pair{}
	for _, sp := range specs {
		s := h.addSource(forge.Repo{Name: sp.name})
		p := resolve.Pair{Rule: sp.rule, Settings: rules[sp.rule], SrcForge: "src", Src: s.repo,
			DstForge: sp.dstForge, DstOwner: "mirrors", DstName: sp.name}
		r := h.sync(p, Options{})
		wantStatus(t, r, state.RunOK)
		pairs[sp.name] = p
	}
	for _, n := range []string{"a", "b", "c", "d", "e", "g", "h", "i"} {
		h.src.RemoveRepo("alice", n)
	}
	complete := map[string]bool{"keep": true, "arch": true, "del": true, "deladopt": true, "delnd": true, "removed": true, "inc": false}
	settingsMap := map[string]config.RuleSettings{}
	for k, v := range rules {
		if k != "removed" {
			settingsMap[k] = v
		}
	}
	dropped := []resolve.Pair{pairs["h"]}
	archived := func(p *fake.Provider, name string) bool {
		r, err := p.GetRepo(ctx, "mirrors", name)
		if err != nil {
			t.Fatalf("GetRepo mirrors/%s: %v", name, err)
		}
		return r.Archived
	}
	byName := func(reps []Report) map[string]Report {
		m := map[string]Report{}
		for _, r := range reps {
			m[strings.TrimPrefix(r.Key.SrcRepo, "alice/")] = r
		}
		return m
	}
	noWrites := func(what string, dstMark, dst2Mark int) {
		t.Helper()
		if w := writeCalls(h.dst.Calls()[dstMark:]); len(w) != 0 {
			t.Fatalf("%s: dst writes: %v", what, w)
		}
		if w := writeCalls(dst2.Calls()[dst2Mark:]); len(w) != 0 {
			t.Fatalf("%s: dst2 writes: %v", what, w)
		}
	}
	acted := []string{"a", "b", "c", "d", "e"}
	untouched := []string{"f", "g", "h", "i"}
	t0 := clk.Now()

	// Cycle 1, dry run: actions only, and no missing state is written.
	runsBefore := h.runCount()
	dstMark, dst2Mark := len(h.dst.Calls()), len(dst2.Calls())
	reps := h.eng.Reconcile(ctx, nil, complete, dropped, settingsMap, Options{DryRun: true, Trigger: "schedule"})
	if len(reps) != 5 {
		t.Fatalf("dry-run reports = %d, want 5: %+v", len(reps), reps)
	}
	noWrites("dry-run reconcile", dstMark, dst2Mark)
	if h.runCount() != runsBefore {
		t.Fatal("dry-run reconcile recorded runs")
	}
	for _, r := range reps {
		if len(r.Actions) == 0 {
			t.Fatalf("dry-run report without actions: %+v", r)
		}
		wantAction(t, r, "waiting until "+t0.Add(time.Hour).Format(time.RFC3339))
	}
	for _, p := range pairs {
		if _, ok := h.missingSince(p.Key()); ok {
			t.Fatalf("dry run recorded missing state for %s", p.Key())
		}
	}

	// Cycle 1, real: the grace window starts; nothing destructive, no runs.
	dstMark, dst2Mark = len(h.dst.Calls()), len(dst2.Calls())
	reps = h.eng.Reconcile(ctx, nil, complete, dropped, settingsMap, Options{Trigger: "schedule"})
	if len(reps) != 5 {
		t.Fatalf("in-grace reports = %d, want 5: %+v", len(reps), reps)
	}
	noWrites("in-grace reconcile", dstMark, dst2Mark)
	if h.runCount() != runsBefore {
		t.Fatal("in-grace reconcile recorded runs")
	}
	got := byName(reps)
	for _, n := range acted {
		r, ok := got[n]
		if !ok {
			t.Fatalf("no in-grace report for %s", n)
		}
		wantStatus(t, r, state.RunOK)
		if r.RunID != 0 {
			t.Fatalf("in-grace reconcile of %s recorded run %d", n, r.RunID)
		}
		wantAction(t, r, "source missing since "+t0.Format(time.RFC3339))
		at, ok := h.missingSince(pairs[n].Key())
		if !ok || !at.Equal(t0) {
			t.Fatalf("MissingSince(%s) = %v, %v; want %v", n, at, ok, t0)
		}
	}
	wantAction(t, got["b"], "before applying on_source_deleted=archive")
	wantAction(t, got["c"], "before applying on_source_deleted=delete")
	for _, n := range untouched {
		if _, ok := h.missingSince(pairs[n].Key()); ok {
			t.Fatalf("pair %s marked missing", n)
		}
	}

	// Still inside the window: first-seen is kept, still nothing happens.
	clk.Advance(59 * time.Minute)
	dstMark, dst2Mark = len(h.dst.Calls()), len(dst2.Calls())
	reps = h.eng.Reconcile(ctx, nil, complete, dropped, settingsMap, Options{Trigger: "schedule"})
	if len(reps) != 5 {
		t.Fatalf("late in-grace reports = %d, want 5", len(reps))
	}
	noWrites("late in-grace reconcile", dstMark, dst2Mark)
	if h.runCount() != runsBefore {
		t.Fatal("late in-grace reconcile recorded runs")
	}
	for _, r := range reps {
		wantAction(t, r, "source missing since "+t0.Format(time.RFC3339))
	}

	// Grace elapsed. Dry run first: describes the policy, writes nothing.
	clk.Advance(time.Minute)
	dstMark, dst2Mark = len(h.dst.Calls()), len(dst2.Calls())
	reps = h.eng.Reconcile(ctx, nil, complete, dropped, settingsMap, Options{DryRun: true, Trigger: "schedule"})
	if len(reps) != 5 {
		t.Fatalf("post-grace dry-run reports = %d, want 5", len(reps))
	}
	noWrites("post-grace dry-run reconcile", dstMark, dst2Mark)
	if h.runCount() != runsBefore {
		t.Fatal("post-grace dry-run recorded runs")
	}
	for _, r := range reps {
		wantAction(t, r, "source deleted")
	}
	for _, n := range acted {
		if at, _ := h.missingSince(pairs[n].Key()); !at.Equal(t0) {
			t.Fatalf("dry run changed MissingSince(%s) to %v", n, at)
		}
	}

	// Grace elapsed, real: the policy is applied once.
	dstMark = len(h.dst.Calls())
	reps = h.eng.Reconcile(ctx, nil, complete, dropped, settingsMap, Options{Trigger: "schedule"})
	got = byName(reps)
	if len(reps) != 5 {
		t.Fatalf("reports = %d, want 5: %+v", len(reps), reps)
	}
	for _, n := range untouched {
		if _, ok := got[n]; ok {
			t.Fatalf("pair %s should be untouched", n)
		}
		if archived(h.dst, n) {
			t.Fatalf("pair %s destination archived", n)
		}
	}
	for n, r := range got {
		wantStatus(t, r, state.RunOK)
		if n == "a" {
			continue
		}
		if r.RunID == 0 {
			t.Fatalf("reconcile of %s not recorded as a run", n)
		}
	}
	wantAction(t, got["a"], "source deleted; keeping destination")
	if got["a"].RunID != 0 || len(h.reconcileRuns(pairs["a"].Key())) != 0 {
		t.Fatal("keep policy recorded a run")
	}
	if archived(h.dst, "a") {
		t.Fatal("keep archived the destination")
	}
	if !archived(h.dst, "b") {
		t.Fatal("archive did not archive")
	}
	if n := countCalls(h.dst.Calls()[dstMark:], "DeleteRepo"); n != 1 {
		t.Fatalf("DeleteRepo calls = %d, want 1", n)
	}
	if _, err := h.dst.GetRepo(ctx, "mirrors", "c"); !errors.Is(err, forge.ErrNotFound) {
		t.Fatalf("deleted destination still exists: %v", err)
	}
	if _, ok, _ := h.store.Owned(ctx, "dst", "mirrors/c"); ok {
		t.Fatal("ownership of deleted repo not released")
	}
	states, err := h.store.Pairs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range states {
		if st.Key == pairs["c"].Key() {
			t.Fatal("deleted pair still in the state store")
		}
	}
	if _, ok := h.missingSince(pairs["c"].Key()); ok {
		t.Fatal("deleted pair's missing record survived")
	}
	if !archived(h.dst, "d") {
		t.Fatal("adopted destination not archived instead of deleted")
	}
	if !strings.Contains(fmt.Sprint(got["d"].Steps, got["d"].Actions), "adopted") {
		t.Fatalf("adopted report lacks a detail: %+v", got["d"])
	}
	if !archived(dst2.Provider, "e") {
		t.Fatal("destination without RepoDeleter not archived")
	}
	if !strings.Contains(fmt.Sprint(got["e"].Steps, got["e"].Actions), "cannot delete") {
		t.Fatalf("no-deleter report lacks a detail: %+v", got["e"])
	}
	for _, n := range []string{"b", "d", "e"} {
		runs := h.reconcileRuns(pairs[n].Key())
		if len(runs) != 1 || runs[0].Trigger != "schedule:reconcile" || runs[0].Status != state.RunOK {
			t.Fatalf("reconcile runs of %s = %+v, want exactly one ok schedule:reconcile run", n, runs)
		}
	}

	// A further cycle: archived destinations are left alone without new
	// runs, the deleted pair is gone, keep reports again but records nothing.
	runsBefore = h.runCount()
	dstMark, dst2Mark = len(h.dst.Calls()), len(dst2.Calls())
	clk.Advance(time.Hour)
	reps = h.eng.Reconcile(ctx, nil, complete, dropped, settingsMap, Options{Trigger: "schedule"})
	got = byName(reps)
	if len(reps) != 1 {
		t.Fatalf("after-action reports = %d, want only keep: %+v", len(reps), reps)
	}
	wantAction(t, got["a"], "source deleted; keeping destination")
	if h.runCount() != runsBefore {
		t.Fatal("cycle after the policy was applied recorded runs")
	}
	noWrites("cycle after the policy was applied", dstMark, dst2Mark)
}

func TestReconcileSkipsCurrentPairs(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.useClock()
	s := h.addSource(forge.Repo{Name: "a"})
	p := h.pair(s, settings("settings"))
	p.Settings.OnSourceDeleted = "archive"
	wantStatus(t, h.sync(p, Options{}), state.RunOK)
	h.src.RemoveRepo("alice", "a")
	reps := h.eng.Reconcile(ctx, []resolve.Pair{p}, map[string]bool{"r1": true}, nil,
		map[string]config.RuleSettings{"r1": p.Settings}, Options{})
	if len(reps) != 0 {
		t.Fatalf("current pair reconciled: %+v", reps)
	}
	if _, ok := h.missingSince(p.Key()); ok {
		t.Fatal("current pair marked missing")
	}
}

// reconcileFixture syncs one pair with the given policy and a 1h grace.
func reconcileFixture(t *testing.T, policy string) (*harness, *testClock, resolve.Pair, func(Options) []Report) {
	t.Helper()
	h := newHarness(t)
	clk := h.useClock()
	s := h.addSource(forge.Repo{Name: "a"})
	p := h.pair(s, settings("settings"))
	p.Settings.OnSourceDeleted = policy
	p.Settings.DeletionGrace = time.Hour
	wantStatus(t, h.sync(p, Options{}), state.RunOK)
	rec := func(opt Options) []Report {
		return h.eng.Reconcile(context.Background(), nil, map[string]bool{"r1": true}, nil,
			map[string]config.RuleSettings{"r1": p.Settings}, opt)
	}
	return h, clk, p, rec
}

func TestReconcileGraceResetsWhenSourceReappears(t *testing.T) {
	h, clk, p, rec := reconcileFixture(t, "archive")
	k := p.Key()
	h.src.RemoveRepo("alice", "a")
	t0 := clk.Now()
	if reps := rec(Options{}); len(reps) != 1 {
		t.Fatalf("reports = %+v", reps)
	}
	if at, ok := h.missingSince(k); !ok || !at.Equal(t0) {
		t.Fatalf("MissingSince = %v, %v", at, ok)
	}

	// The source comes back (merely filtered out of the resolution).
	clk.Advance(50 * time.Minute)
	if err := h.src.AddRepo(forge.Repo{Owner: "alice", Name: "a", DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	// A dry run does not clear the record.
	if reps := rec(Options{DryRun: true}); len(reps) != 0 {
		t.Fatalf("existing source reported: %+v", reps)
	}
	if _, ok := h.missingSince(k); !ok {
		t.Fatal("dry run cleared the missing record")
	}
	runs := h.runCount()
	if reps := rec(Options{}); len(reps) != 0 {
		t.Fatalf("existing source reported: %+v", reps)
	}
	if _, ok := h.missingSince(k); ok {
		t.Fatal("missing record not cleared when the source reappeared")
	}
	if h.runCount() != runs {
		t.Fatal("reconcile of an existing source recorded a run")
	}

	// It disappears again: a fresh window starts now, not at t0.
	clk.Advance(50 * time.Minute)
	h.src.RemoveRepo("alice", "a")
	t1 := clk.Now()
	reps := rec(Options{})
	if len(reps) != 1 {
		t.Fatalf("reports = %+v", reps)
	}
	wantAction(t, reps[0], "source missing since "+t1.Format(time.RFC3339))
	if at, ok := h.missingSince(k); !ok || !at.Equal(t1) {
		t.Fatalf("MissingSince = %v, %v; want fresh %v", at, ok, t1)
	}
	clk.Advance(59 * time.Minute) // past t0+1h, inside t1+1h
	dstMark := len(h.dst.Calls())
	rec(Options{})
	if w := writeCalls(h.dst.Calls()[dstMark:]); len(w) != 0 {
		t.Fatalf("destructive call inside the fresh window: %v", w)
	}
	if h.dstRepo("mirrors", "a").Archived {
		t.Fatal("archived inside the fresh window")
	}
	clk.Advance(time.Minute)
	rec(Options{})
	if !h.dstRepo("mirrors", "a").Archived {
		t.Fatal("not archived after the fresh window elapsed")
	}
}

func TestSyncPairClearsMissing(t *testing.T) {
	h, _, p, _ := reconcileFixture(t, "archive")
	ctx := context.Background()
	if _, err := h.store.MarkMissing(ctx, p.Key()); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, h.sync(p, Options{DryRun: true}), state.RunOK)
	if _, ok := h.missingSince(p.Key()); !ok {
		t.Fatal("dry-run sync cleared the missing record")
	}
	wantStatus(t, h.sync(p, Options{}), state.RunOK)
	if _, ok := h.missingSince(p.Key()); ok {
		t.Fatal("successful sync did not clear the missing record")
	}
}

func TestReconcileIgnoresOtherSourceErrors(t *testing.T) {
	h, clk, p, rec := reconcileFixture(t, "delete")
	k := p.Key()
	h.src.RemoveRepo("alice", "a")

	// A non-not-found error never starts the clock.
	h.src.FailNext("GetRepo", errors.New("503 service unavailable"))
	if reps := rec(Options{}); len(reps) != 0 {
		t.Fatalf("reports on a plain error: %+v", reps)
	}
	if _, ok := h.missingSince(k); ok {
		t.Fatal("plain error started the grace clock")
	}

	t0 := clk.Now()
	rec(Options{})
	clk.Advance(2 * time.Hour)
	runs := h.runCount()
	dstMark := len(h.dst.Calls())
	// Past the grace, but the source answer is an error: nothing happens.
	h.src.FailNext("GetRepo", errors.New("403 forbidden"))
	if reps := rec(Options{}); len(reps) != 0 {
		t.Fatalf("reports on a plain error: %+v", reps)
	}
	if w := writeCalls(h.dst.Calls()[dstMark:]); len(w) != 0 {
		t.Fatalf("dst writes on a plain error: %v", w)
	}
	if h.runCount() != runs {
		t.Fatal("plain error recorded a run")
	}
	if at, ok := h.missingSince(k); !ok || !at.Equal(t0) {
		t.Fatalf("MissingSince = %v, %v; want unchanged %v", at, ok, t0)
	}
	if _, err := h.dst.GetRepo(context.Background(), "mirrors", "a"); err != nil {
		t.Fatalf("destination gone after plain error: %v", err)
	}
}

func TestReconcileKeepRecordsNoRuns(t *testing.T) {
	h, clk, p, rec := reconcileFixture(t, "keep")
	h.src.RemoveRepo("alice", "a")
	runs := h.runCount()
	dstMark := len(h.dst.Calls())
	for range 3 {
		reps := rec(Options{})
		if len(reps) != 1 {
			t.Fatalf("reports = %+v", reps)
		}
		wantStatus(t, reps[0], state.RunOK)
		clk.Advance(time.Hour)
	}
	reps := rec(Options{})
	wantAction(t, reps[0], "source deleted; keeping destination")
	if h.runCount() != runs || len(h.reconcileRuns(p.Key())) != 0 {
		t.Fatal("keep policy recorded runs")
	}
	if w := writeCalls(h.dst.Calls()[dstMark:]); len(w) != 0 {
		t.Fatalf("keep wrote to the destination: %v", w)
	}
}

func TestReconcileDryRunUsesExistingMissingRecord(t *testing.T) {
	h, clk, p, rec := reconcileFixture(t, "archive")
	h.src.RemoveRepo("alice", "a")
	t0 := clk.Now()
	rec(Options{})
	clk.Advance(30 * time.Minute)
	reps := rec(Options{DryRun: true})
	if len(reps) != 1 {
		t.Fatalf("reports = %+v", reps)
	}
	wantAction(t, reps[0], "source missing since "+t0.Format(time.RFC3339)+"; waiting until "+t0.Add(time.Hour).Format(time.RFC3339))
	clk.Advance(time.Hour)
	dstMark := len(h.dst.Calls())
	reps = rec(Options{DryRun: true})
	wantAction(t, reps[0], "source deleted; archive destination")
	if w := writeCalls(h.dst.Calls()[dstMark:]); len(w) != 0 {
		t.Fatalf("dry run wrote: %v", w)
	}
	if at, _ := h.missingSince(p.Key()); !at.Equal(t0) {
		t.Fatalf("dry run changed MissingSince to %v", at)
	}
}
