package resolve

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"gitmirror/internal/config"
	"gitmirror/internal/forge"
	"gitmirror/internal/forge/fake"
)

const forgesYAML = `
forges:
  gh: {type: github, token: t}
  home: {type: gitea, url: "https://git.example.com", token: t}
`

func parse(t *testing.T, rules string) *config.Config {
	t.Helper()
	env := func(string) (string, bool) { return "", false }
	rf := func(string) ([]byte, error) { return nil, errors.New("no files") }
	cfg, err := config.Parse([]byte(forgesYAML+"rules:\n"+rules), env, rf)
	if err != nil {
		t.Fatalf("config.Parse: %v", err)
	}
	return cfg
}

type env struct {
	gh, home *fake.Provider
	provs    map[string]forge.Provider
}

func newEnv() *env {
	e := &env{gh: fake.New(fake.Options{Type: "github"}), home: fake.New(fake.Options{Type: "gitea"})}
	e.provs = map[string]forge.Provider{"gh": e.gh, "home": e.home}
	return e
}

func add(t *testing.T, p *fake.Provider, owner, name string, mod func(*forge.Repo)) forge.Repo {
	t.Helper()
	r := forge.Repo{Owner: owner, Name: name, DefaultBranch: "main"}
	if mod != nil {
		mod(&r)
	}
	if err := p.AddRepo(r); err != nil {
		t.Fatal(err)
	}
	return r
}

func srcNames(r Result) []string {
	var out []string
	for _, p := range r.Pairs {
		out = append(out, p.Rule+":"+p.Src.FullName()+"->"+p.DstForge+":"+p.DstFullName())
	}
	return out
}

func errText(r Result) string {
	var sb strings.Builder
	for _, e := range r.Errors {
		sb.WriteString(e.Error() + "\n")
	}
	return sb.String()
}

func eq(t *testing.T, got, want []string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
}

const ownerRule = `
  - name: mine
    from: {forge: gh, owner: me%s}
    to: {forge: home, owner: mirrors}
`

func ownerCfg(t *testing.T, filter string) *config.Config {
	return parse(t, fmt.Sprintf(ownerRule, filter))
}

func TestRuleErrorFormat(t *testing.T) {
	inner := errors.New("boom")
	e := RuleError{Rule: "r", Err: inner}
	if e.Error() != "rule r: boom" {
		t.Fatal(e.Error())
	}
	if !errors.Is(e, inner) {
		t.Fatal("Unwrap")
	}
}

func TestRepoFound(t *testing.T) {
	e := newEnv()
	add(t, e.gh, "me", "proj", nil)
	cfg := parse(t, `
  - name: pub
    from: {forge: gh, repo: me/proj}
    to: {forge: home, repo: pub/proj}
    interval: 15m
`)
	res := Resolve(context.Background(), cfg, e.provs)
	eq(t, srcNames(res), []string{"pub:me/proj->home:pub/proj"})
	p := res.Pairs[0]
	if p.SrcForge != "gh" || p.DstForge != "home" || p.Settings.Interval.Minutes() != 15 {
		t.Fatalf("bad pair %+v", p)
	}
	if !res.Complete["pub"] || len(res.Errors) != 0 {
		t.Fatalf("complete=%v errs=%s", res.Complete, errText(res))
	}
}

func TestRepoNotFound(t *testing.T) {
	e := newEnv()
	cfg := parse(t, `
  - name: pub
    from: {forge: gh, repo: me/gone}
    to: {forge: home, repo: pub/gone}
`)
	res := Resolve(context.Background(), cfg, e.provs)
	if len(res.Pairs) != 0 || !res.Complete["pub"] {
		t.Fatalf("pairs=%v complete=%v", res.Pairs, res.Complete)
	}
	if len(res.Errors) != 1 || !strings.Contains(res.Errors[0].Error(), "source repository not found") {
		t.Fatalf("errs: %s", errText(res))
	}
}

func TestRepoOtherError(t *testing.T) {
	e := newEnv()
	add(t, e.gh, "me", "proj", nil)
	e.gh.FailNext("GetRepo", errors.New("500"))
	cfg := parse(t, `
  - name: pub
    from: {forge: gh, repo: me/proj}
    to: {forge: home, repo: pub/proj}
`)
	res := Resolve(context.Background(), cfg, e.provs)
	if len(res.Pairs) != 0 || res.Complete["pub"] || len(res.Errors) != 1 {
		t.Fatalf("pairs=%v complete=%v errs=%s", res.Pairs, res.Complete, errText(res))
	}
}

func seedMixed(t *testing.T, e *env) {
	add(t, e.gh, "me", "alpha", nil)
	add(t, e.gh, "me", "Beta", nil)
	add(t, e.gh, "me", "forky", func(r *forge.Repo) { r.Fork = true })
	add(t, e.gh, "me", "old", func(r *forge.Repo) { r.Archived = true })
	add(t, e.gh, "me", "secret", func(r *forge.Repo) { r.Private = true })
	add(t, e.gh, "me", "tmp-x", nil)
}

func names(r Result) []string {
	var out []string
	for _, p := range r.Pairs {
		out = append(out, p.Src.Name)
	}
	return out
}

func TestOwnerDefaultFilter(t *testing.T) {
	e := newEnv()
	seedMixed(t, e)
	res := Resolve(context.Background(), ownerCfg(t, ""), e.provs)
	// forks dropped; archived and private kept; sorted case-insensitively
	eq(t, names(res), []string{"alpha", "Beta", "old", "secret", "tmp-x"})
	if !res.Complete["mine"] {
		t.Fatal("not complete")
	}
	if res.Pairs[0].DstOwner != "mirrors" || res.Pairs[0].DstName != "alpha" {
		t.Fatalf("%+v", res.Pairs[0])
	}
}

func TestOwnerNilFilterBehavesAsDefaults(t *testing.T) {
	e := newEnv()
	seedMixed(t, e)
	cfg := ownerCfg(t, "")
	cfg.Rules[0].From.Filter = nil
	res := Resolve(context.Background(), cfg, e.provs)
	eq(t, names(res), []string{"alpha", "Beta", "old", "secret", "tmp-x"})
}

func TestOwnerFilterFlags(t *testing.T) {
	e := newEnv()
	seedMixed(t, e)
	res := Resolve(context.Background(), ownerCfg(t, ", filter: {forks: true, archived: false, private: false}"), e.provs)
	eq(t, names(res), []string{"alpha", "Beta", "forky", "tmp-x"})
}

func TestOwnerFilterPatterns(t *testing.T) {
	e := newEnv()
	seedMixed(t, e)
	res := Resolve(context.Background(), ownerCfg(t, `, filter: {include: ["ALPHA", "b*", "tmp-*"], exclude: ["tmp-*"]}`), e.provs)
	eq(t, names(res), []string{"alpha", "Beta"})

	res = Resolve(context.Background(), ownerCfg(t, `, filter: {include: ["me/a*"], exclude: ["ME/*ta"]}`), e.provs)
	eq(t, names(res), []string{"alpha"})
}

func TestStarredSlashPatterns(t *testing.T) {
	e := newEnv()
	for _, r := range []forge.Repo{
		{Owner: "x", Name: "one"}, {Owner: "y", Name: "one"}, {Owner: "x", Name: "two"}, {Owner: "Z", Name: "three"},
	} {
		e.gh.Star("me", r)
	}
	cfg := parse(t, `
  - name: stars
    from: {forge: gh, starred_by: me, filter: {include: ["x/*", "z/*"], exclude: ["*/two"]}}
    to: {forge: home, owner: stars, name: "{{.Owner}}-{{.Name}}"}
`)
	res := Resolve(context.Background(), cfg, e.provs)
	eq(t, srcNames(res), []string{
		"stars:x/one->home:stars/x-one",
		"stars:Z/three->home:stars/Z-three",
	})
	// name-only pattern matches the bare name across owners
	cfg.Rules[0].From.Filter.Include = []string{"one"}
	cfg.Rules[0].From.Filter.Exclude = nil
	res = Resolve(context.Background(), cfg, e.provs)
	eq(t, srcNames(res), []string{
		"stars:x/one->home:stars/x-one",
		"stars:y/one->home:stars/y-one",
	})
}

func TestBadPattern(t *testing.T) {
	e := newEnv()
	seedMixed(t, e)
	cfg := ownerCfg(t, "")
	cfg.Rules[0].From.Filter.Include = []string{"[a"}
	res := Resolve(context.Background(), cfg, e.provs)
	if len(res.Pairs) != 0 || len(res.Errors) != 1 || res.Complete["mine"] {
		t.Fatalf("pairs=%v errs=%s complete=%v", res.Pairs, errText(res), res.Complete)
	}
}

type noStars struct{ forge.Provider }

func TestStarredNeedsStarProvider(t *testing.T) {
	e := newEnv()
	e.provs["gh"] = noStars{e.gh}
	cfg := parse(t, `
  - name: stars
    from: {forge: gh, starred_by: me}
    to: {forge: home, owner: stars}
`)
	res := Resolve(context.Background(), cfg, e.provs)
	if res.Complete["stars"] || len(res.Errors) != 1 || !strings.Contains(res.Errors[0].Error(), "github") {
		t.Fatalf("complete=%v errs=%s", res.Complete, errText(res))
	}
}

func TestListErrorIncomplete(t *testing.T) {
	e := newEnv()
	seedMixed(t, e)
	e.gh.FailNext("ListRepos", errors.New("rate limited"))
	res := Resolve(context.Background(), ownerCfg(t, ""), e.provs)
	if len(res.Pairs) != 0 || len(res.Errors) != 1 {
		t.Fatalf("pairs=%v errs=%s", res.Pairs, errText(res))
	}
	if v, ok := res.Complete["mine"]; !ok || v {
		t.Fatalf("Complete entry should be present and false: %v", res.Complete)
	}

	e.gh.FailNext("ListStarred", errors.New("nope"))
	cfg := parse(t, `
  - name: stars
    from: {forge: gh, starred_by: me}
    to: {forge: home, owner: stars}
`)
	res = Resolve(context.Background(), cfg, e.provs)
	if v, ok := res.Complete["stars"]; !ok || v || len(res.Errors) != 1 {
		t.Fatalf("complete=%v errs=%s", res.Complete, errText(res))
	}
}

func TestEmptyListingIsComplete(t *testing.T) {
	e := newEnv()
	e.gh.AddOwner("me", false)
	res := Resolve(context.Background(), ownerCfg(t, ""), e.provs)
	if !res.Complete["mine"] || len(res.Pairs) != 0 || len(res.Errors) != 0 {
		t.Fatalf("%+v", res)
	}
}

type dupStars struct {
	forge.Provider
	list []forge.Repo
}

func (d dupStars) ListStarred(context.Context, string) ([]forge.Repo, error) { return d.list, nil }

func TestStarredDedup(t *testing.T) {
	e := newEnv()
	rep := forge.Repo{Owner: "x", Name: "one"}
	e.provs["gh"] = dupStars{e.gh, []forge.Repo{rep, {Owner: "X", Name: "ONE"}, {Owner: "x", Name: "two"}}}
	cfg := parse(t, `
  - name: stars
    from: {forge: gh, starred_by: me}
    to: {forge: home, owner: stars, name: "{{.Owner}}-{{.Name}}"}
`)
	res := Resolve(context.Background(), cfg, e.provs)
	eq(t, srcNames(res), []string{
		"stars:x/one->home:stars/x-one",
		"stars:x/two->home:stars/x-two",
	})
	if len(res.Errors) != 0 {
		t.Fatal(errText(res))
	}
}

func TestPartialRenderFailure(t *testing.T) {
	e := newEnv()
	add(t, e.gh, "me", "good", nil)
	add(t, e.gh, "me", "bad name", nil) // space is not valid in a rendered component
	res := Resolve(context.Background(), ownerCfg(t, ""), e.provs)
	eq(t, names(res), []string{"good"})
	if !res.Complete["mine"] || len(res.Errors) != 1 || !strings.Contains(res.Errors[0].Error(), "bad name") {
		t.Fatalf("complete=%v errs=%s", res.Complete, errText(res))
	}
}

func TestMissingProvider(t *testing.T) {
	e := newEnv()
	add(t, e.gh, "me", "a", nil)
	for _, missing := range []string{"gh", "home"} {
		provs := map[string]forge.Provider{"gh": e.gh, "home": e.home}
		delete(provs, missing)
		res := Resolve(context.Background(), ownerCfg(t, ""), provs)
		if len(res.Pairs) != 0 || len(res.Errors) != 1 {
			t.Fatalf("missing %s: pairs=%v errs=%s", missing, res.Pairs, errText(res))
		}
		if v, ok := res.Complete["mine"]; !ok || v {
			t.Fatalf("missing %s: Complete entry should be present and false", missing)
		}
	}
}

func TestSettingsEffective(t *testing.T) {
	e := newEnv()
	add(t, e.gh, "me", "a", nil)
	cfg := ownerCfg(t, "")
	cfg.Rules[0].Visibility = "match-source"
	res := Resolve(context.Background(), cfg, e.provs)
	want := cfg.Rules[0].Effective(cfg.Defaults)
	if !reflect.DeepEqual(res.Pairs[0].Settings, want) {
		t.Fatalf("settings %+v want %+v", res.Pairs[0].Settings, want)
	}
}

// forceCfg parses a harmless config then replaces its rules, so runtime
// collisions that config.Validate would catch can be exercised.
func forceCfg(t *testing.T, rules ...config.Rule) *config.Config {
	cfg := parse(t, fmt.Sprintf(ownerRule, ""))
	cfg.Rules = rules
	return cfg
}

func repoRule(name, sf, srepo, df, drepo string) config.Rule {
	return config.Rule{Name: name, From: config.Source{Forge: sf, Repo: srepo}, To: config.Dest{Forge: df, Repo: drepo}}
}

func TestSelfMirror(t *testing.T) {
	e := newEnv()
	add(t, e.gh, "me", "a", nil)
	add(t, e.gh, "me", "b", nil)
	cfg := forceCfg(t,
		repoRule("self", "gh", "me/a", "gh", "ME/A"),
		repoRule("ok", "gh", "me/b", "home", "me/b"),
	)
	res := Resolve(context.Background(), cfg, e.provs)
	eq(t, srcNames(res), []string{"ok:me/b->home:me/b"})
	if len(res.Errors) != 1 || res.Errors[0].Rule != "self" || !strings.Contains(res.Errors[0].Error(), "source and destination are the same") {
		t.Fatalf("errs: %s", errText(res))
	}
	if !res.Complete["self"] || len(res.Dropped) != 1 || res.Dropped[0].Rule != "self" {
		t.Fatalf("complete=%v dropped=%v", res.Complete, res.Dropped)
	}
	// same repo name on different forges is fine
	cfg = forceCfg(t, repoRule("x", "gh", "me/a", "home", "me/a"))
	res = Resolve(context.Background(), cfg, e.provs)
	if len(res.Pairs) != 1 || len(res.Errors) != 0 {
		t.Fatal(errText(res))
	}
}

func TestDuplicateDestinationDropsAll(t *testing.T) {
	e := newEnv()
	add(t, e.gh, "me", "a", nil)
	add(t, e.gh, "me", "b", nil)
	add(t, e.gh, "me", "c", nil)
	cfg := forceCfg(t,
		repoRule("r1", "gh", "me/a", "home", "m/dst"),
		repoRule("r2", "gh", "me/b", "home", "M/Dst"),
		repoRule("r3", "gh", "me/c", "home", "m/other"),
	)
	res := Resolve(context.Background(), cfg, e.provs)
	eq(t, srcNames(res), []string{"r3:me/c->home:m/other"})
	if len(res.Errors) != 2 || len(res.Dropped) != 2 {
		t.Fatalf("errs=%s dropped=%v", errText(res), res.Dropped)
	}
	if res.Errors[0].Rule != "r1" || res.Errors[1].Rule != "r2" {
		t.Fatalf("errs=%s", errText(res))
	}
	m0 := res.Errors[0].Error()
	if !strings.Contains(m0, "targeted by multiple sources") || !strings.Contains(m0, "r2") || !strings.Contains(m0, "me/b") {
		t.Fatalf("msg: %s", m0)
	}
	if !res.Complete["r1"] || !res.Complete["r2"] || !res.Complete["r3"] {
		t.Fatalf("%v", res.Complete)
	}
}

func TestDuplicateDestinationTemplated(t *testing.T) {
	e := newEnv()
	add(t, e.gh, "me", "a", nil)
	add(t, e.gh, "me", "b", nil)
	cfg := parse(t, `
  - name: fixed
    from: {forge: gh, owner: me}
    to: {forge: home, owner: mirrors, name: same}
`)
	res := Resolve(context.Background(), cfg, e.provs)
	if len(res.Pairs) != 0 || len(res.Errors) != 2 || len(res.Dropped) != 2 || !res.Complete["fixed"] {
		t.Fatalf("pairs=%v errs=%s dropped=%v", res.Pairs, errText(res), res.Dropped)
	}
}

func TestLoop(t *testing.T) {
	e := newEnv()
	add(t, e.gh, "me", "a", nil)
	add(t, e.home, "me", "a", nil)
	cfg := forceCfg(t,
		repoRule("fwd", "gh", "me/a", "home", "me/a"),
		repoRule("back", "home", "me/a", "gh", "me/a"),
	)
	res := Resolve(context.Background(), cfg, e.provs)
	if len(res.Pairs) != 0 || len(res.Dropped) != 2 || len(res.Errors) != 2 {
		t.Fatalf("pairs=%v errs=%s", res.Pairs, errText(res))
	}
	for _, re := range res.Errors {
		if !strings.Contains(re.Error(), "mirror loop") {
			t.Fatal(errText(res))
		}
	}
	if !res.Complete["fwd"] || !res.Complete["back"] {
		t.Fatal(res.Complete)
	}
}

func TestChainAllowed(t *testing.T) {
	e := newEnv()
	add(t, e.gh, "me", "a", nil)
	add(t, e.home, "me", "a", nil)
	cfg := forceCfg(t,
		repoRule("one", "gh", "me/a", "home", "me/a"),
		repoRule("two", "home", "me/a", "home", "me/b"),
	)
	res := Resolve(context.Background(), cfg, e.provs)
	if len(res.Pairs) != 2 || len(res.Errors) != 0 || len(res.Dropped) != 0 {
		t.Fatalf("pairs=%v errs=%s", res.Pairs, errText(res))
	}
}

func TestOnly(t *testing.T) {
	e := newEnv()
	add(t, e.gh, "me", "a", nil)
	cfg := parse(t, `
  - name: one
    from: {forge: gh, owner: me}
    to: {forge: home, owner: m1}
  - name: two
    from: {forge: gh, owner: me}
    to: {forge: home, owner: m2}
`)
	res := Resolve(context.Background(), cfg, e.provs, "two", "nope")
	eq(t, srcNames(res), []string{"two:me/a->home:m2/a"})
	if _, ok := res.Complete["one"]; ok {
		t.Fatal("one must be absent from Complete")
	}
	if !res.Complete["two"] {
		t.Fatal("two should be complete")
	}
	if len(res.Errors) != 1 || res.Errors[0].Rule != "nope" {
		t.Fatalf("errs: %s", errText(res))
	}
	if n := len(e.gh.Calls()); n != 1 {
		t.Fatalf("expected one listing call, got %v", e.gh.Calls())
	}
}

func TestDeterminism(t *testing.T) {
	e := newEnv()
	for i := 0; i < 15; i++ {
		add(t, e.gh, "me", fmt.Sprintf("r%02d", 14-i), nil)
		add(t, e.gh, "org", fmt.Sprintf("q%02d", i), nil)
	}
	add(t, e.gh, "me", "bad name", nil)
	cfg := parse(t, `
  - name: zeta
    from: {forge: gh, owner: me}
    to: {forge: home, owner: z}
  - name: alpha
    from: {forge: gh, owner: org}
    to: {forge: home, owner: a}
  - name: mid
    from: {forge: gh, owner: org}
    to: {forge: home, owner: z}
`)
	cfg.Concurrency = 8
	first := Resolve(context.Background(), cfg, e.provs)
	if len(first.Pairs) == 0 || len(first.Errors) == 0 {
		t.Fatalf("setup: %d pairs, errs %s", len(first.Pairs), errText(first))
	}
	for i := 1; i < len(first.Pairs); i++ {
		a, b := first.Pairs[i-1], first.Pairs[i]
		if a.Rule > b.Rule || (a.Rule == b.Rule && strings.ToLower(a.Src.FullName()) > strings.ToLower(b.Src.FullName())) {
			t.Fatalf("unsorted at %d", i)
		}
	}
	for i := 0; i < 20; i++ {
		got := Resolve(context.Background(), cfg, e.provs)
		if !reflect.DeepEqual(got, first) {
			t.Fatalf("run %d differs", i)
		}
	}
	cfg.Concurrency = 0 // min 1
	if got := Resolve(context.Background(), cfg, e.provs); !reflect.DeepEqual(got, first) {
		t.Fatal("serial run differs")
	}
}

func TestCancelledContext(t *testing.T) {
	e := newEnv()
	add(t, e.gh, "me", "a", nil)
	cfg := parse(t, fmt.Sprintf(ownerRule, "")+`
  - name: other
    from: {forge: gh, owner: me}
    to: {forge: home, owner: o2}
`)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res := Resolve(ctx, cfg, e.provs)
	if len(res.Pairs) != 0 || len(res.Errors) != 2 {
		t.Fatalf("pairs=%v errs=%s", res.Pairs, errText(res))
	}
	for _, re := range res.Errors {
		if !errors.Is(re, context.Canceled) {
			t.Fatalf("err %v", re)
		}
	}
	for _, name := range []string{"mine", "other"} {
		if v, ok := res.Complete[name]; !ok || v {
			t.Fatalf("complete[%s]=%v,%v", name, v, ok)
		}
	}
}
