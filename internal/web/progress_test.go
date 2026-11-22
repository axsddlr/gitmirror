package web

import (
	"context"
	"strings"
	"testing"

	"gitmirror/internal/config"
	"gitmirror/internal/state"
)

func TestRunProgress(t *testing.T) {
	steps := func(names ...string) []state.StepResult {
		var out []state.StepResult
		for _, n := range names {
			out = append(out, state.StepResult{Step: n, Status: state.StepOK})
		}
		return out
	}
	cases := []struct {
		steps   []state.StepResult
		lfs     bool
		percent int
		label   string
	}{
		{nil, false, 0, "Step 1 of 5: Checks before copying"},
		{steps("prepare"), false, 20, "Step 2 of 5: Code (branches and tags)"},
		{steps("prepare", "git"), true, 33, "Step 3 of 6: Large files (LFS)"},
		{steps("prepare", "git", "settings", "releases", "wiki", "finalize"), false, 100, "Finishing up"},
	}
	for _, c := range cases {
		got := runProgress(c.steps, c.lfs)
		if got.Percent != c.percent || got.Label != c.label {
			t.Errorf("runProgress(%v, %v) = %+v, want %d%% %q", c.steps, c.lfs, got, c.percent, c.label)
		}
	}
}

func TestProgressBarWhileCopying(t *testing.T) {
	w := newWorld(t, "")
	w.addSites()
	w.putRule(config.Rule{Name: "m1", From: config.Source{Forge: "github", Owner: "alice"}, To: config.Dest{Forge: "git-example-com", Owner: "carol"}})
	c := w.admin()
	ctx := context.Background()
	k := state.PairKey{Rule: "m1", SrcForge: "github", SrcRepo: "alice/a", DstForge: "git-example-com", DstRepo: "carol/a"}
	id, err := w.app.Store.StartRun(ctx, k, "schedule")
	if err != nil {
		t.Fatal(err)
	}
	if err := w.app.Store.UpdateRunSteps(ctx, id, []state.StepResult{{Step: "prepare", Status: state.StepOK}}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/", "/mirrors", "/pair?k=" + pairID(k)} {
		mustContain(t, c.get(p), "<progress", "Code (branches and tags)")
	}
	if err := w.app.Store.FinishRun(ctx, id, state.RunOK, nil, "", "", 1); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/", "/mirrors", "/pair?k=" + pairID(k)} {
		if strings.Contains(c.get(p).body, "<progress") {
			t.Fatalf("%s shows a progress bar after the copy finished", p)
		}
	}
}
