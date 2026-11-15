package engine

import (
	"context"
	"testing"

	"gitmirror/internal/config"
	"gitmirror/internal/forge"
	"gitmirror/internal/resolve"
	"gitmirror/internal/state"
)

// Options.Providers / ForgeURLs replace the Engine's for one call, so a
// caller can hand every run its own consistent configuration snapshot.
func TestOptionsProvidersOverrideEngine(t *testing.T) {
	h := newHarness(t)
	s := h.addSource(forge.Repo{Name: "proj"})
	s.commit("a.txt")
	s.push("main")
	provs := h.eng.Providers
	h.eng.Providers = nil

	r := h.sync(h.pair(s, settings()), Options{})
	wantStatus(t, r, state.RunFailed) // no providers at all

	r = h.sync(h.pair(s, settings()), Options{Providers: provs, ForgeURLs: map[string]string{"dst": "https://dst.example"}})
	wantStatus(t, r, state.RunOK)
	rec, ok, err := h.store.OwnershipOf(context.Background(), "dst", "mirrors/proj")
	if err != nil || !ok || rec.ForgeURL != "https://dst.example" {
		t.Fatalf("ownership = %+v %v %v, want the run's forge URL", rec, ok, err)
	}

	// Reconcile also uses the run's providers: the source vanished and the
	// policy archives, which needs the destination provider.
	h.src.RemoveRepo("alice", "proj")
	rs := settings()
	rs.OnSourceDeleted = "archive"
	p := h.pair(s, rs)
	opt := Options{Providers: provs}
	none := h.eng.Reconcile(context.Background(), nil, map[string]bool{p.Rule: true}, nil,
		map[string]config.RuleSettings{p.Rule: rs}, Options{})
	if len(none) != 0 {
		t.Fatalf("reconcile without providers = %+v", none)
	}
	_ = h.eng.Reconcile(context.Background(), []resolve.Pair{}, map[string]bool{p.Rule: true}, nil,
		map[string]config.RuleSettings{p.Rule: rs}, opt)
	if _, ok, _ := h.store.MissingSince(context.Background(), p.Key()); !ok {
		t.Fatal("reconcile with Options.Providers did not reach the source forge")
	}
}
