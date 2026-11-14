package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"gitmirror/internal/forge"
	"gitmirror/internal/state"
)

func (h *harness) auditLog() []state.Event {
	h.t.Helper()
	evs, err := h.store.AuditLog(context.Background(), 0)
	if err != nil {
		h.t.Fatal(err)
	}
	return evs
}

// auditActions lists the audit actions recorded for k, oldest first.
func (h *harness) auditActions(k state.PairKey) []string {
	h.t.Helper()
	var out []string
	evs := h.auditLog()
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Key == k {
			out = append(out, string(evs[i].Action))
		}
	}
	return out
}

func wantAudit(t *testing.T, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("audit actions = %v, want %v", got, want)
	}
}

// assertNoTokenInAudit fails if any audit field carries a forge token.
func (h *harness) assertNoTokenInAudit() {
	h.t.Helper()
	for _, ev := range h.auditLog() {
		for _, tok := range []string{"srctoken-1234", "dsttoken-5678"} {
			if strings.Contains(ev.Detail+ev.Key.String(), tok) {
				h.t.Fatalf("token in audit event %+v", ev)
			}
		}
	}
}

func TestAuditCreateAndSourceArchive(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	s := h.addSource(forge.Repo{Name: "a"})
	s.commit("f")
	s.push("main")
	p := h.pair(s, settings())

	// A dry run creates nothing and audits nothing.
	wantStatus(t, h.sync(p, Options{DryRun: true}), state.RunOK)
	if evs := h.auditLog(); len(evs) != 0 {
		t.Fatalf("dry run audited: %+v", evs)
	}

	wantStatus(t, h.sync(p, Options{}), state.RunOK)
	wantAudit(t, h.auditActions(p.Key()), "create")
	ev := h.auditLog()[0]
	if ev.Key != p.Key() || ev.Detail == "" {
		t.Fatalf("create event = %+v", ev)
	}

	// An unchanged second sync audits nothing new.
	wantStatus(t, h.sync(p, Options{}), state.RunOK)
	wantAudit(t, h.auditActions(p.Key()), "create")

	// The source is archived: the destination is archived (audited once).
	tr := true
	if err := h.src.UpdateRepoSettings(ctx, "alice", "a", forge.Settings{Archived: &tr}); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, h.sync(p, Options{}), state.RunOK)
	if !h.dstRepo("mirrors", "a").Archived {
		t.Fatal("destination not archived")
	}
	wantAudit(t, h.auditActions(p.Key()), "create", "archive")
	// Later syncs unarchive and re-archive for the sync: not a new archive.
	wantStatus(t, h.sync(p, Options{}), state.RunOK)
	wantAudit(t, h.auditActions(p.Key()), "create", "archive")
	h.assertNoTokenInAudit()
}

func TestAuditAdopt(t *testing.T) {
	h := newHarness(t)
	s := h.addSource(forge.Repo{Name: "a"})
	s.commit("f")
	s.push("main")
	if err := h.dst.AddRepo(forge.Repo{Owner: "mirrors", Name: "a", DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	p := h.pair(s, settings())
	p.Settings.Adopt = true
	wantStatus(t, h.sync(p, Options{DryRun: true}), state.RunOK)
	if evs := h.auditLog(); len(evs) != 0 {
		t.Fatalf("dry run audited: %+v", evs)
	}
	wantStatus(t, h.sync(p, Options{}), state.RunOK)
	wantStatus(t, h.sync(p, Options{}), state.RunOK)
	wantAudit(t, h.auditActions(p.Key()), "adopt")
	h.assertNoTokenInAudit()
}

func TestAuditReconcileDeleteSurvivesDeletePair(t *testing.T) {
	h, clk, p, rec := reconcileFixture(t, "delete")
	k := p.Key()
	h.src.RemoveRepo("alice", "a")
	rec(Options{})
	clk.Advance(2 * time.Hour)
	// The dry run audits nothing.
	if reps := rec(Options{DryRun: true}); len(reps) != 1 {
		t.Fatalf("dry-run reports = %+v", reps)
	}
	wantAudit(t, h.auditActions(k), "create")
	reps := rec(Options{})
	if len(reps) != 1 || reps[0].Err != nil {
		t.Fatalf("reports = %+v", reps)
	}
	if _, err := h.dst.GetRepo(context.Background(), "mirrors", "a"); err == nil {
		t.Fatal("destination not deleted")
	}
	// The pair (with the run that recorded the deletion) is gone...
	if runs, _ := h.store.Runs(context.Background(), k, 0); len(runs) != 0 {
		t.Fatalf("runs of deleted pair = %+v", runs)
	}
	// ...but the audit trail keeps the deletion.
	wantAudit(t, h.auditActions(k), "create", "delete")
	h.assertNoTokenInAudit()
}

func TestAuditReconcileArchive(t *testing.T) {
	h, clk, p, rec := reconcileFixture(t, "archive")
	k := p.Key()
	h.src.RemoveRepo("alice", "a")
	rec(Options{})
	clk.Advance(2 * time.Hour)
	rec(Options{})
	if !h.dstRepo("mirrors", "a").Archived {
		t.Fatal("not archived")
	}
	rec(Options{})
	wantAudit(t, h.auditActions(k), "create", "archive")
}

// After the grace, outcomes that change nothing (destination not owned or
// already gone) are recorded once, not every cycle.
func TestReconcileNoOpOutcomeRecordedOnce(t *testing.T) {
	cases := []struct {
		name  string
		setup func(h *harness)
		want  string
	}{
		{"not owned", func(h *harness) {
			if err := h.store.ReleaseOwnership(context.Background(), "dst", "mirrors/a"); err != nil {
				t.Fatal(err)
			}
		}, "not owned"},
		{"gone", func(h *harness) { h.dst.RemoveRepo("mirrors", "a") }, "already gone"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, clk, p, rec := reconcileFixture(t, "archive")
			k := p.Key()
			h.src.RemoveRepo("alice", "a")
			tc.setup(h)
			rec(Options{})
			clk.Advance(2 * time.Hour)
			before := len(h.reconcileRuns(k))
			for i := range 3 {
				reps := rec(Options{})
				switch {
				case i == 0 && len(reps) != 1:
					t.Fatalf("cycle 1 reports = %+v, want one", reps)
				case i == 0:
					wantAction(t, reps[0], tc.want)
				case len(reps) != 0:
					t.Fatalf("cycle %d reports = %+v, want none", i+1, reps)
				}
				clk.Advance(time.Hour)
			}
			if n := len(h.reconcileRuns(k)) - before; n != 1 {
				t.Fatalf("reconcile runs over three cycles = %d, want exactly 1", n)
			}
			// A dry run afterwards reports nothing either.
			if reps := rec(Options{DryRun: true}); len(reps) != 0 {
				t.Fatalf("dry-run reports after the outcome was recorded = %+v", reps)
			}
			// No archive/delete was audited for a no-op outcome.
			wantAudit(t, h.auditActions(k), "create")
		})
	}
}
