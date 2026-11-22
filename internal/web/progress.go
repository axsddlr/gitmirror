package web

import (
	"fmt"

	"gitmirror/internal/app"
	"gitmirror/internal/state"
)

// progressView is how far a running copy has got, counted in steps.
type progressView struct {
	Done, Total int
	Percent     int    // 0-100
	Label       string // e.g. "Step 2 of 6: Code (branches and tags)"
}

// runProgress returns the progress of a running run from the steps it has
// finished so far. lfs tells whether the run has an LFS step (recorded only
// when the rule includes lfs). Steps outside the usual order (finalize) do
// not count.
func runProgress(steps []state.StepResult, lfs bool) progressView {
	order := []string{"prepare", "git", "lfs", "settings", "releases", "wiki"}
	if !lfs {
		order = []string{"prepare", "git", "settings", "releases", "wiki"}
	}
	seen := map[string]bool{}
	for _, s := range steps {
		seen[s.Step] = true
	}
	done := 0
	for _, name := range order {
		if seen[name] {
			done++
		}
	}
	pv := progressView{Done: done, Total: len(order), Percent: done * 100 / len(order)}
	if done < len(order) {
		next := ""
		for _, name := range order {
			if !seen[name] {
				next = name
				break
			}
		}
		pv.Label = fmt.Sprintf("Step %d of %d: %s", done+1, len(order), stepLabel(next))
	} else {
		pv.Label = "Finishing up"
	}
	return pv
}

// mirrorProgress averages the progress of a mirror's running copies; nil
// when none is running.
func mirrorProgress(pairs []pairView) *progressView {
	n, sum := 0, 0
	for _, p := range pairs {
		if p.Progress != nil {
			n++
			sum += p.Progress.Percent
		}
	}
	if n == 0 {
		return nil
	}
	pv := progressView{Percent: sum / n, Done: n, Total: len(pairs)}
	if n == 1 {
		for _, p := range pairs {
			if p.Progress != nil {
				pv.Label = p.Src + ": " + p.Progress.Label
			}
		}
	} else {
		pv.Label = fmt.Sprintf("Copying %d of %d repositories", n, len(pairs))
	}
	return &pv
}

// ruleIncludes reports whether the rule named rule includes feature in the
// current configuration; false when the rule is gone.
func ruleIncludes(snap *app.Snapshot, rule, feature string) bool {
	cfg := snap.Config
	for _, r := range cfg.Rules {
		if r.Name == rule {
			return r.Effective(cfg.Defaults).Includes(feature)
		}
	}
	return false
}
