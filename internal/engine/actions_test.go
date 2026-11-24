package engine

import (
	"errors"
	"strings"
	"testing"

	"gitmirror/internal/forge"
	"gitmirror/internal/state"
)

// DisableActions turns Actions off in the copy. The setting is applied in the
// prepare step, so it also holds when the settings step is not included, and a
// dry run only plans it.
func TestDisableActions(t *testing.T) {
	h := newHarness(t)
	s := h.addSource(forge.Repo{Name: "proj"})
	s.commit("a.txt")
	s.push("main")
	p := h.pair(s, settings("git", "releases")) // no settings step

	// A first run without the option leaves Actions alone.
	wantStatus(t, h.sync(p, Options{}), state.RunOK)
	if h.dst.ActionsOff("mirrors", "proj") {
		t.Fatal("Actions off without disable_actions")
	}

	// A dry run plans the change and touches nothing.
	p.Settings.DisableActions = true
	r := h.sync(p, Options{DryRun: true})
	wantStatus(t, r, state.RunOK)
	wantAction(t, r, "turn Actions off for dst:mirrors/proj")
	if h.dst.ActionsOff("mirrors", "proj") {
		t.Fatal("dry run turned Actions off")
	}

	r = h.sync(p, Options{})
	wantStatus(t, r, state.RunOK)
	if !h.dst.ActionsOff("mirrors", "proj") {
		t.Fatal("Actions still on at the destination")
	}
	if d := wantStep(t, r, StepPrepare, state.StepOK).Detail; !strings.Contains(d, "Actions off") {
		t.Fatalf("prepare detail = %q", d)
	}

	// Running again is harmless.
	wantStatus(t, h.sync(p, Options{}), state.RunOK)
}

// A destination that refuses the change fails the run instead of leaving
// Actions running silently.
func TestDisableActionsFailure(t *testing.T) {
	h := newHarness(t)
	s := h.addSource(forge.Repo{Name: "proj"})
	p := h.pair(s, settings("git"))
	p.Settings.DisableActions = true

	h.dst.FailNext("UpdateRepoSettings", errors.New("403 must have admin rights"))
	r := h.sync(p, Options{})
	wantStatus(t, r, state.RunFailed)
	if r.Err == nil || !strings.Contains(r.Err.Error(), "turn Actions off for dst:mirrors/proj") {
		t.Fatalf("err = %v", r.Err)
	}
}
