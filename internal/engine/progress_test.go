package engine

import (
	"context"
	"testing"

	"gitmirror/internal/forge"
	"gitmirror/internal/forge/fake"
	"gitmirror/internal/state"
)

// peekSrc records the run's stored steps when the releases step lists the
// source's releases.
type peekSrc struct {
	*fake.Provider
	store *state.Store
	key   state.PairKey
	seen  []state.StepResult
	got   bool
}

func (p *peekSrc) ListReleases(ctx context.Context, owner, name string) ([]forge.Release, error) {
	if runs, err := p.store.Runs(ctx, p.key, 1); err == nil && len(runs) == 1 && runs[0].Status == state.RunRunning {
		p.seen, p.got = runs[0].Steps, true
	}
	return p.Provider.ListReleases(ctx, owner, name)
}

// Finished steps are stored while the run is still running, so the web UI
// can show how far a copy has got.
func TestStepsStoredWhileRunning(t *testing.T) {
	h := newHarness(t)
	p := h.pair(h.addSource(forge.Repo{Name: "x"}), settings("git", "settings", "releases"))
	peek := &peekSrc{Provider: h.src, store: h.store, key: p.Key()}
	h.eng.Providers["src"] = peek
	wantStatus(t, h.sync(p, Options{}), state.RunOK)
	if !peek.got {
		t.Fatal("releases step did not run while the run was running")
	}
	var names []string
	for _, s := range peek.seen {
		names = append(names, s.Step)
	}
	if len(names) < 2 || names[0] != StepPrepare || names[1] != StepGit {
		t.Fatalf("steps stored mid-run = %v, want prepare, git, ...", names)
	}
	// A dry run stores no run at all.
	h2 := newHarness(t)
	p2 := h2.pair(h2.addSource(forge.Repo{Name: "y"}), settings("settings"))
	wantStatus(t, h2.sync(p2, Options{DryRun: true}), state.RunOK)
	if runs, _ := h2.store.Runs(context.Background(), p2.Key(), 0); len(runs) != 0 {
		t.Fatalf("dry run stored runs %+v", runs)
	}
}
