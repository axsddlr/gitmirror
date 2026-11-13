package resolve

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"

	"gitmirror/internal/config"
	"gitmirror/internal/forge"
)

// RuleError is a problem with a rule or with one pair of a rule.
type RuleError struct {
	Rule string
	Err  error
}

// Error implements error.
func (e RuleError) Error() string { return "rule " + e.Rule + ": " + e.Err.Error() }

// Unwrap returns the underlying error.
func (e RuleError) Unwrap() error { return e.Err }

// Result is the outcome of Resolve.
type Result struct {
	// Pairs are the concrete mirrors, sorted by Rule, then Src.FullName()
	// (case-insensitive).
	Pairs []Pair
	// Dropped are pairs removed by the runtime safety checks (self-mirror,
	// duplicate destination, mirror loop), in the same order as Pairs. Their
	// source exists, so the engine's reconcile step MUST treat them as not
	// deleted (it must exclude them when deciding that a missing pair means
	// the source was removed), even though their rule is Complete.
	Dropped []Pair
	// Errors are per-rule or per-pair problems; they never abort other rules.
	Errors []RuleError
	// Complete maps a rule name to whether its listing fully succeeded (even
	// if zero repositories matched). Only rules that were attempted appear.
	// A rule is never Complete after a partial or failed listing.
	Complete map[string]bool
}

type ruleOutcome struct {
	pairs    []Pair
	errs     []RuleError
	complete bool
}

// Resolve expands the rules of cfg into pairs. If only is non-empty, just
// those rules are resolved; others are neither listed nor reported in
// Complete, and an unknown name yields a RuleError.
func Resolve(ctx context.Context, cfg *config.Config, providers map[string]forge.Provider, only ...string) Result {
	res := Result{Complete: map[string]bool{}}

	var rules []config.Rule
	if len(only) == 0 {
		rules = cfg.Rules
	} else {
		want := map[string]bool{}
		for _, n := range only {
			want[n] = true
		}
		known := map[string]bool{}
		for _, r := range cfg.Rules {
			known[r.Name] = true
			if want[r.Name] {
				rules = append(rules, r)
			}
		}
		seen := map[string]bool{}
		for _, n := range only {
			if !known[n] && !seen[n] {
				seen[n] = true
				res.Errors = append(res.Errors, RuleError{Rule: n, Err: errors.New("unknown rule")})
			}
		}
	}

	conc := cfg.Concurrency
	if conc < 1 {
		conc = 1
	}
	outs := make([]ruleOutcome, len(rules))
	sem := make(chan struct{}, conc)
	var wg sync.WaitGroup
	for i := range rules {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
			}
			if err := ctx.Err(); err != nil {
				outs[i] = ruleOutcome{errs: []RuleError{{Rule: rules[i].Name, Err: err}}}
				return
			}
			outs[i] = resolveRule(ctx, cfg, providers, rules[i])
		}(i)
	}
	wg.Wait()

	var all []Pair
	for i, o := range outs {
		res.Complete[rules[i].Name] = o.complete
		res.Errors = append(res.Errors, o.errs...)
		all = append(all, o.pairs...)
	}
	sortPairs(all)

	kept, dropped, errs := safety(all)
	res.Pairs = kept
	res.Dropped = dropped
	res.Errors = append(res.Errors, errs...)
	sort.SliceStable(res.Errors, func(i, j int) bool {
		a, b := res.Errors[i], res.Errors[j]
		if a.Rule != b.Rule {
			return a.Rule < b.Rule
		}
		return a.Err.Error() < b.Err.Error()
	})
	return res
}

func sortPairs(ps []Pair) {
	sort.SliceStable(ps, func(i, j int) bool {
		if ps[i].Rule != ps[j].Rule {
			return ps[i].Rule < ps[j].Rule
		}
		return strings.ToLower(ps[i].Src.FullName()) < strings.ToLower(ps[j].Src.FullName())
	})
}

func resolveRule(ctx context.Context, cfg *config.Config, providers map[string]forge.Provider, rule config.Rule) ruleOutcome {
	fail := func(err error) ruleOutcome {
		return ruleOutcome{errs: []RuleError{{Rule: rule.Name, Err: err}}}
	}
	src, ok := providers[rule.From.Forge]
	if !ok || src == nil {
		return fail(fmt.Errorf("no provider for source forge %q", rule.From.Forge))
	}
	if dst, ok := providers[rule.To.Forge]; !ok || dst == nil {
		return fail(fmt.Errorf("no provider for destination forge %q", rule.To.Forge))
	}

	var repos []forge.Repo
	switch {
	case rule.From.Repo != "":
		owner, name, ok := strings.Cut(rule.From.Repo, "/")
		if !ok {
			return fail(fmt.Errorf("source repo %q must be owner/name", rule.From.Repo))
		}
		r, err := src.GetRepo(ctx, owner, name)
		if errors.Is(err, forge.ErrNotFound) {
			return ruleOutcome{complete: true, errs: []RuleError{{Rule: rule.Name, Err: fmt.Errorf("source repository not found: %s: %w", rule.From.Repo, err)}}}
		}
		if err != nil {
			return fail(fmt.Errorf("get %s: %w", rule.From.Repo, err))
		}
		repos = []forge.Repo{*r}
	case rule.From.StarredBy != "":
		sp, ok := src.(forge.StarProvider)
		if !ok {
			return fail(fmt.Errorf("forge %q (type %s) does not support starred_by", rule.From.Forge, src.Type()))
		}
		list, err := sp.ListStarred(ctx, rule.From.StarredBy)
		if err != nil {
			return fail(fmt.Errorf("list starred by %s: %w", rule.From.StarredBy, err))
		}
		repos, err = applyFilter(list, rule.From.Filter)
		if err != nil {
			return fail(err)
		}
	default:
		list, err := src.ListRepos(ctx, rule.From.Owner)
		if err != nil {
			return fail(fmt.Errorf("list repos of %s: %w", rule.From.Owner, err))
		}
		repos, err = applyFilter(list, rule.From.Filter)
		if err != nil {
			return fail(err)
		}
	}

	out := ruleOutcome{complete: true}
	settings := rule.Effective(cfg.Defaults)
	seen := map[string]bool{}
	for _, r := range repos {
		k := strings.ToLower(r.FullName())
		if seen[k] {
			continue
		}
		seen[k] = true
		do, dn, err := rule.To.Render(r.Owner, r.Name)
		if err != nil {
			out.errs = append(out.errs, RuleError{Rule: rule.Name, Err: fmt.Errorf("destination for %s: %w", r.FullName(), err)})
			continue
		}
		out.pairs = append(out.pairs, Pair{
			Rule:     rule.Name,
			Settings: settings,
			SrcForge: rule.From.Forge,
			Src:      r,
			DstForge: rule.To.Forge,
			DstOwner: do,
			DstName:  dn,
		})
	}
	return out
}

func applyFilter(repos []forge.Repo, f *config.Filter) ([]forge.Repo, error) {
	var forks, archived, private bool
	archived, private = true, true
	include := []string{"*"}
	var exclude []string
	if f != nil {
		if f.Forks != nil {
			forks = *f.Forks
		}
		if f.Archived != nil {
			archived = *f.Archived
		}
		if f.Private != nil {
			private = *f.Private
		}
		if len(f.Include) > 0 {
			include = f.Include
		}
		exclude = f.Exclude
	}
	var out []forge.Repo
	for _, r := range repos {
		if (r.Fork && !forks) || (r.Archived && !archived) || (r.Private && !private) {
			continue
		}
		inc, err := matchAny(include, r)
		if err != nil {
			return nil, err
		}
		if !inc {
			continue
		}
		exc, err := matchAny(exclude, r)
		if err != nil {
			return nil, err
		}
		if exc {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

func matchAny(patterns []string, r forge.Repo) (bool, error) {
	full := strings.ToLower(r.FullName())
	name := strings.ToLower(r.Name)
	for _, p := range patterns {
		target := name
		if strings.Contains(p, "/") {
			target = full
		}
		ok, err := path.Match(strings.ToLower(p), target)
		if err != nil {
			return false, fmt.Errorf("filter pattern %q: %w", p, err)
		}
		if ok {
			return true, nil
		}
	}
	return false, nil
}

func srcKey(p Pair) string { return p.SrcForge + "|" + strings.ToLower(p.Src.FullName()) }
func dstKey(p Pair) string { return p.DstForge + "|" + strings.ToLower(p.DstFullName()) }

// safety applies the runtime checks that config.Validate cannot do for
// templated destinations. Input must be sorted; kept and dropped stay sorted.
func safety(in []Pair) (kept, dropped []Pair, errs []RuleError) {
	drop := func(p Pair, msg string) {
		dropped = append(dropped, p)
		errs = append(errs, RuleError{Rule: p.Rule, Err: errors.New(msg)})
	}

	// (a) self-mirror
	var stage []Pair
	for _, p := range in {
		if srcKey(p) == dstKey(p) {
			drop(p, fmt.Sprintf("source and destination are the same (%s:%s)", p.SrcForge, p.Src.FullName()))
			continue
		}
		stage = append(stage, p)
	}

	// (b) duplicate destinations: drop every collider
	byDst := map[string][]int{}
	for i, p := range stage {
		byDst[dstKey(p)] = append(byDst[dstKey(p)], i)
	}
	gone := make([]bool, len(stage))
	for i, p := range stage {
		idx := byDst[dstKey(p)]
		if len(idx) < 2 {
			continue
		}
		gone[i] = true
		var others []string
		for _, j := range idx {
			if j != i {
				q := stage[j]
				others = append(others, fmt.Sprintf("%s (%s:%s)", q.Rule, q.SrcForge, q.Src.FullName()))
			}
		}
		drop(p, fmt.Sprintf("destination %s:%s is targeted by multiple sources: %s",
			p.DstForge, p.DstFullName(), strings.Join(others, ", ")))
	}
	var stage2 []Pair
	for i, p := range stage {
		if !gone[i] {
			stage2 = append(stage2, p)
		}
	}

	// (c) mirror loops: P.dst == Q.src and Q.dst == P.src
	bySrc := map[string][]int{}
	for i, p := range stage2 {
		bySrc[srcKey(p)] = append(bySrc[srcKey(p)], i)
	}
	loop := make([]bool, len(stage2))
	for i, p := range stage2 {
		for _, j := range bySrc[dstKey(p)] {
			if dstKey(stage2[j]) == srcKey(p) {
				loop[i] = true
			}
		}
	}
	for i, p := range stage2 {
		if loop[i] {
			drop(p, fmt.Sprintf("mirror loop: %s:%s and %s:%s mirror into each other",
				p.SrcForge, p.Src.FullName(), p.DstForge, p.DstFullName()))
			continue
		}
		kept = append(kept, p)
	}
	sortPairs(dropped)
	return kept, dropped, errs
}
