package config

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func uiRule(name, from, to string) Rule {
	return Rule{Name: name, From: Source{Forge: "gh", Repo: from}, To: Dest{Forge: "home", Repo: to}}
}

func TestMergeAddsUIEntriesWithDefaultsAndOrigin(t *testing.T) {
	file := mustParse(t, rulesDoc(okRule), nil, nil)
	before := file.String()
	forges := map[string]Forge{"ui-gt": {Type: " Gitea ", URL: "https://ui.example/", Token: "ui-token-1"}}
	rules := []Rule{
		{Name: "ui-b", From: Source{Forge: "ui-gt", Owner: "x"}, To: Dest{Forge: "home", Owner: "y"}},
		{Name: "ui-a", From: Source{Forge: "gh", Repo: "a/b"}, To: Dest{Forge: "ui-gt", Repo: "c/d"}},
	}
	eff, err := Merge(file, forges, rules)
	if err != nil {
		t.Fatal(err)
	}
	if file.String() != before || len(file.Rules) != 1 || len(file.Forges) != 2 {
		t.Fatal("Merge modified the file config")
	}
	if rules[0].From.Filter != nil || rules[0].To.Name != "" {
		t.Fatal("Merge modified the caller's rules")
	}
	if len(eff.Forges) != 3 || len(eff.Rules) != 3 {
		t.Fatalf("effective forges %d rules %d", len(eff.Forges), len(eff.Rules))
	}
	if eff.Rules[0].Name != "r1" || eff.Rules[1].Name != "ui-a" || eff.Rules[2].Name != "ui-b" {
		t.Fatalf("rule order = %s, %s, %s; want file rules first, then UI rules by name", eff.Rules[0].Name, eff.Rules[1].Name, eff.Rules[2].Name)
	}
	g := eff.Forges["ui-gt"]
	if g.Type != "gitea" || g.URL != "https://ui.example" || g.Token != "ui-token-1" {
		t.Fatalf("UI forge not normalised: %v", g)
	}
	b := eff.Rules[2]
	if b.To.Name != "{{.Name}}" || b.From.Filter == nil || *b.From.Filter.Forks || !*b.From.Filter.Archived {
		t.Fatalf("UI rule defaults not applied: %+v", b)
	}
	for name, want := range map[string]Origin{"gh": OriginFile, "home": OriginFile, "ui-gt": OriginUI, "nope": ""} {
		if got := eff.ForgeOrigin(name); got != want {
			t.Errorf("ForgeOrigin(%s) = %q, want %q", name, got, want)
		}
	}
	for name, want := range map[string]Origin{"r1": OriginFile, "ui-a": OriginUI, "ui-b": OriginUI, "nope": ""} {
		if got := eff.RuleOrigin(name); got != want {
			t.Errorf("RuleOrigin(%s) = %q, want %q", name, got, want)
		}
	}
	if file.RuleOrigin("r1") != OriginFile || file.ForgeOrigin("gh") != OriginFile {
		t.Fatal("a parsed file config does not report file origin")
	}
	if strings.Contains(eff.String(), "ui-token-1") {
		t.Fatal("String leaks a UI token")
	}
}

func TestMergeRejectsNameClashWithFile(t *testing.T) {
	file := mustParse(t, rulesDoc(okRule), nil, nil)
	_, err := Merge(file, map[string]Forge{"gh": {Type: "github", Token: "t"}}, nil)
	if err == nil || !strings.Contains(err.Error(), `"gh"`) || !errors.Is(err, ErrFileDefined) {
		t.Fatalf("forge clash err = %v", err)
	}
	_, err = Merge(file, nil, []Rule{uiRule("r1", "a/b", "c/d")})
	if err == nil || !strings.Contains(err.Error(), `"r1"`) || !errors.Is(err, ErrFileDefined) {
		t.Fatalf("rule clash err = %v", err)
	}
}

func TestMergeValidatesAcrossFileAndUIRules(t *testing.T) {
	file := mustParse(t, rulesDoc(`
  - name: lit
    from: {forge: gh, repo: me/x}
    to: {forge: home, repo: mirrors/x}
`), nil, nil)
	tests := []struct {
		name string
		rule Rule
		want string
	}{
		{"loop", uiRule("back", "", ""), "mirror loop"},
		{"collision", uiRule("dup", "me/other", "mirrors/x"), "already the destination"},
		{"self", uiRule("self", "", ""), "same repo"},
		{"unknown forge", Rule{Name: "u", From: Source{Forge: "ghost", Owner: "o"}, To: Dest{Forge: "home", Owner: "m"}}, `from.forge "ghost" is not defined`},
		{"bad name", uiRule("Bad Name", "a/b", "c/d"), "invalid rule name"},
	}
	tests[0].rule = Rule{Name: "back", From: Source{Forge: "home", Repo: "mirrors/x"}, To: Dest{Forge: "gh", Repo: "me/x"}}
	tests[2].rule = Rule{Name: "self", From: Source{Forge: "gh", Repo: "a/b"}, To: Dest{Forge: "gh", Repo: "a/b"}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Merge(file, nil, []Rule{tt.rule})
			checkErr(t, err, tt.want)
		})
	}
	t.Run("duplicate UI rules", func(t *testing.T) {
		_, err := Merge(file, nil, []Rule{uiRule("d", "a/b", "c/d"), uiRule("d", "a/c", "c/e")})
		checkErr(t, err, "duplicate rule name")
	})
}

func TestMergeUIForgeRules(t *testing.T) {
	file := mustParse(t, rulesDoc(okRule), nil, nil)
	_, err := Merge(file, map[string]Forge{"x": {Type: "github"}}, nil)
	checkErr(t, err, "forges.x: one of token or token_file is required")
	_, err = Merge(file, map[string]Forge{"x": {Type: "github", TokenFile: "/run/t"}}, nil)
	checkErr(t, err, "forges.x: token_file")
	_, err = Merge(file, map[string]Forge{"x": {Type: "nope", Token: "t"}}, nil)
	checkErr(t, err, `unknown type "nope"`)
}

func TestZeroRulesValidates(t *testing.T) {
	c := mustParse(t, "", nil, nil)
	if len(c.Rules) != 0 || c.StateDir != defaultStateDir {
		t.Fatalf("empty config = %+v", c)
	}
	eff, err := Merge(c, nil, nil)
	if err != nil || len(eff.Rules) != 0 {
		t.Fatalf("Merge of nothing = %v, %v", eff, err)
	}
	if !errors.Is(ErrNoRules, ErrNoRules) || ErrNoRules.Error() != "no mirror rules configured: add one in the web UI or in the config file" {
		t.Fatalf("ErrNoRules = %q", ErrNoRules)
	}
}

func TestRuleJSONUsesSnakeCaseTags(t *testing.T) {
	yes, n := true, 3
	r := Rule{
		Name: "r", From: Source{Forge: "gh", StarredBy: "me", Filter: &Filter{Forks: &yes, Include: []string{"a*"}}},
		To: Dest{Forge: "home", Owner: "m", Name: "{{.Name}}"}, OnSourceDeleted: "archive", DeletionGrace: "48h",
		DeleteMissingReleases: &yes, MaxRefDeletions: &n, MaxRefDeletionsPercent: &n, AllowPublish: true,
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{`"starred_by"`, `"on_source_deleted"`, `"deletion_grace"`, `"delete_missing_releases"`,
		`"max_ref_deletions"`, `"max_ref_deletions_percent"`, `"allow_publish"`, `"filter"`, `"forks"`} {
		if !strings.Contains(string(b), k) {
			t.Errorf("JSON %s lacks %s", b, k)
		}
	}
	var back Rule
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, r) {
		t.Fatalf("round trip = %+v, want %+v", back, r)
	}
}
