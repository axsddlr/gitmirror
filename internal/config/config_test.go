package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

const secret = "SECRET-123"

func envFrom(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func filesFrom(m map[string]string) func(string) ([]byte, error) {
	return func(p string) ([]byte, error) {
		if v, ok := m[p]; ok {
			return []byte(v), nil
		}
		return nil, fmt.Errorf("open %s: %w", p, os.ErrNotExist)
	}
}

func parse(t *testing.T, doc string, env, files map[string]string) (*Config, error) {
	t.Helper()
	return Parse([]byte(doc), envFrom(env), filesFrom(files))
}

func mustParse(t *testing.T, doc string, env, files map[string]string) *Config {
	t.Helper()
	c, err := parse(t, doc, env, files)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	return c
}

const forgesBlock = `
forges:
  gh: {type: github, token: ` + secret + `}
  home: {type: gitea, url: "https://git.example.com/", token: ` + secret + `}
`

// doc builds a config with valid forges, the given top-level extras and rules.
func doc(top, rules string) string {
	return forgesBlock + top + "\nrules:\n" + rules
}

const okRule = `
  - name: r1
    from: {forge: gh, owner: me}
    to: {forge: home, owner: mirrors}
`

const fullExample = `
state_dir: ./data
concurrency: 8
forges:
  gh:   { type: GitHub, token: "${GITHUB_TOKEN}" }
  home: { type: gitea, url: https://git.example.com, token_file: /run/secrets/gitea, username: bot }
defaults:
  interval: 2h
  include: [git, lfs, settings, releases, wiki]
  visibility: private
  on_source_deleted: keep
  delete_missing_releases: false
server:
  listen: ":9090"
  webhook_secret: "${WEBHOOK_SECRET}"
rules:
  - name: my-repos
    from: { forge: gh, owner: me, filter: { forks: false, archived: true, private: true, include: ["*"], exclude: ["tmp-*"] } }
    to:   { forge: home, owner: mirrors, name: "{{.Name}}" }
  - name: stars
    from: { forge: gh, starred_by: me }
    to:   { forge: home, owner: stars, name: "{{.Owner}}-{{.Name}}" }
  - name: publish
    from: { forge: home, repo: me/project }
    to:   { forge: gh, repo: me/project }
    interval: 15m
    include: [git, releases, git]
    adopt: true
`

func TestParseFullExample(t *testing.T) {
	c := mustParse(t, fullExample,
		map[string]string{"GITHUB_TOKEN": secret, "WEBHOOK_SECRET": "whs"},
		map[string]string{"/run/secrets/gitea": "  gitea-tok\n"})

	if c.StateDir != "./data" || c.Concurrency != 8 {
		t.Errorf("state/concurrency: %q %d", c.StateDir, c.Concurrency)
	}
	if got := c.Forges["gh"]; got.Type != "github" || got.Token != secret || got.URL != "" {
		t.Errorf("gh forge: %#v", got.Type)
	}
	if got := c.Forges["home"]; got.Token != "gitea-tok" || got.URL != "https://git.example.com" || got.Username != "bot" {
		t.Errorf("home forge: url=%q user=%q", got.URL, got.Username)
	}
	if c.Server.Listen != ":9090" || c.Server.WebhookSecret != "whs" {
		t.Errorf("server: %q", c.Server.Listen)
	}
	if len(c.Rules) != 3 {
		t.Fatalf("rules: %d", len(c.Rules))
	}
	e0 := c.Rules[0].Effective(c.Defaults)
	if e0.Interval != 2*time.Hour || e0.Visibility != "private" || e0.OnSourceDeleted != "keep" || e0.Adopt || e0.DeleteMissingReleases {
		t.Errorf("rule0 effective: %+v", e0)
	}
	if len(e0.Include) != 5 {
		t.Errorf("rule0 include: %v", e0.Include)
	}
	f := c.Rules[0].From.Filter
	if f == nil || *f.Forks || !*f.Archived || !*f.Private || len(f.Exclude) != 1 {
		t.Errorf("filter: %+v", f)
	}
	// Filter defaults on a rule without an explicit filter.
	f1 := c.Rules[1].From.Filter
	if f1 == nil || *f1.Forks || !*f1.Archived || !*f1.Private || !reflect.DeepEqual(f1.Include, []string{"*"}) {
		t.Errorf("default filter: %+v", f1)
	}
	if c.Rules[1].To.Name != "{{.Owner}}-{{.Name}}" {
		t.Errorf("to name: %q", c.Rules[1].To.Name)
	}
	e2 := c.Rules[2].Effective(c.Defaults)
	if e2.Interval != 15*time.Minute || !e2.Adopt || !reflect.DeepEqual(e2.Include, []string{"git", "releases"}) {
		t.Errorf("rule2 effective: %+v", e2)
	}
	if !e2.Includes("releases") || e2.Includes("wiki") {
		t.Errorf("Includes broken")
	}
}

func TestDefaultsFilled(t *testing.T) {
	c := mustParse(t, doc("", okRule), nil, nil)
	if c.StateDir != "./data" || c.Concurrency != 4 || c.Server.Listen != ":8080" {
		t.Errorf("top defaults: %q %d %q", c.StateDir, c.Concurrency, c.Server.Listen)
	}
	e := c.Rules[0].Effective(c.Defaults)
	want := RuleSettings{
		Interval:        time.Hour,
		Include:         []string{"git", "lfs", "settings", "releases", "wiki"},
		Visibility:      "private",
		OnSourceDeleted: "keep",
		DeletionGrace:   24 * time.Hour,

		MaxRefDeletions:        5,
		MaxRefDeletionsPercent: 50,
	}
	if c.Defaults.DeletionGrace != "24h" {
		t.Errorf("defaults deletion_grace = %q, want 24h", c.Defaults.DeletionGrace)
	}
	if !reflect.DeepEqual(e, want) {
		t.Errorf("effective = %+v, want %+v", e, want)
	}
	if c.Rules[0].To.Name != "{{.Name}}" {
		t.Errorf("default dest name template: %q", c.Rules[0].To.Name)
	}
	if c.Server.WebhookSecret != "" {
		t.Errorf("webhook secret should be allowed absent")
	}
}

func TestRuleOverridesDefaults(t *testing.T) {
	c := mustParse(t, doc(`
defaults:
  interval: 3h
  visibility: match-source
  on_source_deleted: archive
  delete_missing_releases: true
`, `
  - name: a
    from: {forge: gh, owner: me}
    to: {forge: home, owner: m}
  - name: b
    from: {forge: gh, owner: me}
    to: {forge: home, owner: m2}
    visibility: private
    on_source_deleted: delete
    delete_missing_releases: false
`), nil, nil)
	a := c.Rules[0].Effective(c.Defaults)
	if a.Interval != 3*time.Hour || a.Visibility != "match-source" || a.OnSourceDeleted != "archive" || !a.DeleteMissingReleases {
		t.Errorf("a: %+v", a)
	}
	b := c.Rules[1].Effective(c.Defaults)
	if b.Visibility != "private" || b.OnSourceDeleted != "delete" || b.DeleteMissingReleases {
		t.Errorf("b: %+v", b)
	}
}

func TestDeletionGrace(t *testing.T) {
	c := mustParse(t, doc(`
defaults:
  deletion_grace: 48h
`, `
  - name: a
    from: {forge: gh, owner: me}
    to: {forge: home, owner: m}
  - name: b
    from: {forge: gh, owner: me}
    to: {forge: home, owner: m2}
    deletion_grace: 2h30m
`), nil, nil)
	if g := c.Rules[0].Effective(c.Defaults).DeletionGrace; g != 48*time.Hour {
		t.Errorf("a: deletion grace = %s, want 48h", g)
	}
	if g := c.Rules[1].Effective(c.Defaults).DeletionGrace; g != 150*time.Minute {
		t.Errorf("b: deletion grace = %s, want 2h30m", g)
	}

	tests := []struct {
		name, doc, want string
	}{
		{"defaults below minimum", doc("defaults: {deletion_grace: 59m}", okRule), "defaults: deletion_grace 59m is below the minimum of 1h"},
		{"defaults zero", doc("defaults: {deletion_grace: 0s}", okRule), "defaults: deletion_grace"},
		{"defaults invalid", doc("defaults: {deletion_grace: soon}", okRule), `defaults: deletion_grace "soon" is not a valid duration`},
		{"defaults 1h ok", doc("defaults: {deletion_grace: 1h}", okRule), ""},
		{"rule below minimum", okRuleWith("    deletion_grace: 30m\n"), "rules[0] (r1): deletion_grace 30m is below the minimum of 1h"},
		{"rule invalid", okRuleWith("    deletion_grace: x\n"), `rules[0] (r1): deletion_grace "x" is not a valid duration`},
		{"rule 1h ok", okRuleWith("    deletion_grace: 1h\n"), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parse(t, tt.doc, nil, nil)
			checkErr(t, err, tt.want)
		})
	}
}

func TestInterpolation(t *testing.T) {
	t.Run("default value", func(t *testing.T) {
		c := mustParse(t, doc("state_dir: ${NOPE:-fallback}", okRule), nil, nil)
		if c.StateDir != "fallback" {
			t.Errorf("got %q", c.StateDir)
		}
	})
	t.Run("default unused when set", func(t *testing.T) {
		c := mustParse(t, doc("state_dir: ${A:-b}", okRule), map[string]string{"A": "a"}, nil)
		if c.StateDir != "a" {
			t.Errorf("got %q", c.StateDir)
		}
	})
	t.Run("escape", func(t *testing.T) {
		c := mustParse(t, doc(`state_dir: "$${HOME}/x"`, okRule), nil, nil)
		if c.StateDir != "${HOME}/x" {
			t.Errorf("got %q", c.StateDir)
		}
	})
	t.Run("undefined names var and field not value", func(t *testing.T) {
		d := strings.Replace(doc("", okRule), "gh: {type: github, token: "+secret+"}", "gh: {type: github, token: \"${MISSING_TOK}\"}", 1)
		_, err := parse(t, d, nil, nil)
		if err == nil || !strings.Contains(err.Error(), "MISSING_TOK") || !strings.Contains(err.Error(), "forges.gh.token") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("value cannot inject yaml structure", func(t *testing.T) {
		env := map[string]string{"V": "x\nconcurrency: 99\nbogus: 1"}
		c := mustParse(t, doc("state_dir: ${V}", okRule), env, nil)
		if c.Concurrency != 4 || c.StateDir != env["V"] {
			t.Errorf("injection effect: %d %q", c.Concurrency, c.StateDir)
		}
	})
	t.Run("in rule strings", func(t *testing.T) {
		d := doc("", `
  - name: r1
    from: {forge: gh, owner: "${WHO}"}
    to: {forge: home, owner: mirrors}
`)
		c := mustParse(t, d, map[string]string{"WHO": "alice"}, nil)
		if c.Rules[0].From.Owner != "alice" {
			t.Errorf("owner %q", c.Rules[0].From.Owner)
		}
	})
	t.Run("unterminated", func(t *testing.T) {
		_, err := parse(t, doc(`state_dir: "${OOPS"`, okRule), nil, nil)
		if err == nil || !strings.Contains(err.Error(), "state_dir") {
			t.Errorf("err = %v", err)
		}
	})
}

func TestSecrets(t *testing.T) {
	gitea := func(forgeBody string) string {
		return "forges:\n  home: {type: gitea, url: 'https://g.example', " + forgeBody + "}\nrules:\n" +
			"  - name: r\n    from: {forge: home, owner: a}\n    to: {forge: home, owner: b}\n"
	}
	files := map[string]string{"/s/tok": " \n filetok \n\n", "/s/empty": " \n\t\n", "/s/wh": "whfile\n"}
	tests := []struct {
		name string
		doc  string
		want string // substring of error; empty = success
	}{
		{"token ok", gitea("token: abc"), ""},
		{"token_file ok", gitea("token_file: /s/tok"), ""},
		{"both", gitea("token: abc, token_file: /s/tok"), `forges.home: only one of token and token_file`},
		{"neither", gitea("username: u"), `forges.home: one of token or token_file is required`},
		{"empty file", gitea("token_file: /s/empty"), `forges.home.token_file`},
		{"missing file", gitea("token_file: /s/none"), `forges.home.token_file`},
		{"webhook both", doc("server: {webhook_secret: a, webhook_secret_file: /s/wh}", okRule), `server: only one of webhook_secret and webhook_secret_file`},
		{"webhook file empty", doc("server: {webhook_secret_file: /s/empty}", okRule), `server.webhook_secret_file`},
		{"webhook file ok", doc("server: {webhook_secret_file: /s/wh}", okRule), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parse(t, tt.doc, nil, files)
			checkErr(t, err, tt.want)
		})
	}
	c := mustParse(t, gitea("token_file: /s/tok"), nil, files)
	if c.Forges["home"].Token != "filetok" {
		t.Errorf("token_file not trimmed: %q", c.Forges["home"].Token)
	}
	c = mustParse(t, doc("server: {webhook_secret_file: /s/wh}", okRule), nil, files)
	if c.Server.WebhookSecret != "whfile" {
		t.Errorf("webhook file: %q", c.Server.WebhookSecret)
	}
}

func checkErr(t *testing.T, err error, want ...string) {
	t.Helper()
	if len(want) == 1 && want[0] == "" {
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		return
	}
	if err == nil {
		t.Fatalf("expected error containing %q, got nil", want)
	}
	for _, w := range want {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("error %q does not contain %q", err.Error(), w)
		}
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("error leaks secret: %v", err)
	}
}

func TestForgeValidation(t *testing.T) {
	tail := "rules:\n" + okRule
	tests := []struct {
		name, doc, want string
	}{
		{"missing type", "forges:\n  gh: {token: t}\n  home: {type: gitea, url: 'https://x', token: t}\n" + tail, `forges.gh: type is required`},
		{"unknown type", "forges:\n  gh: {type: gitlab, token: t}\n  home: {type: gitea, url: 'https://x', token: t}\n" + tail, `forges.gh: unknown type "gitlab"`},
		{"gitea no url", "forges:\n  gh: {type: github, token: t}\n  home: {type: gitea, token: t}\n" + tail, `forges.home: url is required for gitea`},
		{"bad url scheme", "forges:\n  gh: {type: github, token: t}\n  home: {type: gitea, url: 'ftp://x', token: t}\n" + tail, `forges.home: url`},
		{"relative url", "forges:\n  gh: {type: github, token: t}\n  home: {type: gitea, url: 'git.example.com', token: t}\n" + tail, `forges.home: url`},
		{"github url ok", "forges:\n  gh: {type: github, url: 'https://ghe.example/api/v3/', token: t}\n  home: {type: gitea, url: 'https://x', token: t}\n" + tail, ""},
		{"bad name", "forges:\n  GH: {type: github, token: t}\n  home: {type: gitea, url: 'https://x', token: t}\n" + tail, `forges.GH: invalid forge name`},
		{"leading dash name", "forges:\n  -gh: {type: github, token: t}\n  home: {type: gitea, url: 'https://x', token: t}\n" + tail, `invalid forge name`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parse(t, tt.doc, nil, nil)
			if tt.name == "leading dash name" && err == nil {
				t.Fatal("expected error")
			}
			checkErr(t, err, tt.want)
		})
	}
}

func TestKnownForgeTypesExtensible(t *testing.T) {
	old := KnownForgeTypes
	defer func() { KnownForgeTypes = old }()
	KnownForgeTypes = append(append([]string{}, old...), "gitlab")
	d := "forges:\n  gl: {type: GitLab, token: t}\n  home: {type: gitea, url: 'https://x', token: t}\nrules:\n  - name: r\n    from: {forge: gl, owner: a}\n    to: {forge: home, owner: b}\n"
	c := mustParse(t, d, nil, nil)
	if c.Forges["gl"].Type != "gitlab" {
		t.Errorf("type %q", c.Forges["gl"].Type)
	}
}

func rule(name, from, to string, extra string) string {
	return fmt.Sprintf("  - name: %s\n    from: {%s}\n    to: {%s}\n%s", name, from, to, extra)
}

func TestRuleValidation(t *testing.T) {
	tests := []struct {
		name  string
		rules string
		want  []string
	}{
		{"missing name", "  - from: {forge: gh, owner: me}\n    to: {forge: home, owner: m}\n", []string{"rules[0]: name is required"}},
		{"bad name", rule("Bad Name", "forge: gh, owner: me", "forge: home, owner: m", ""), []string{`rules[0] (Bad Name): invalid rule name`}},
		{"duplicate name", okRule + okRule, []string{`rules[1] (r1): duplicate rule name`}},
		{"undefined from forge", rule("x", "forge: nope, owner: me", "forge: home, owner: m", ""), []string{`rules[0] (x): from.forge "nope" is not defined`}},
		{"undefined to forge", okRule + rule("stars", "forge: gh, starred_by: me", "forge: nope, owner: s", ""), []string{`rules[1] (stars): to.forge "nope" is not defined`}},
		{"from none", rule("x", "forge: gh", "forge: home, owner: m", ""), []string{`rules[0] (x): from: exactly one of owner, starred_by, repo`}},
		{"from two", rule("x", "forge: gh, owner: me, starred_by: me", "forge: home, owner: m", ""), []string{`rules[0] (x): from: exactly one of owner, starred_by, repo`}},
		{"from repo bad", rule("x", "forge: gh, repo: justname", "forge: home, repo: a/b", ""), []string{`rules[0] (x): from.repo "justname" must be owner/name`}},
		{"from repo three parts", rule("x", "forge: gh, repo: a/b/c", "forge: home, repo: a/b", ""), []string{`from.repo "a/b/c" must be owner/name`}},
		{"filter with repo", rule("x", "forge: gh, repo: a/b, filter: {forks: true}", "forge: home, repo: a/b", ""), []string{`rules[0] (x): from.filter is only allowed with owner or starred_by`}},
		{"filter bad glob", rule("x", "forge: gh, owner: me, filter: {include: ['[']}", "forge: home, owner: m", ""), []string{`rules[0] (x): from.filter.include[0]`}},
		{"filter bad exclude glob", rule("x", "forge: gh, owner: me, filter: {exclude: ['[a']}", "forge: home, owner: m", ""), []string{`from.filter.exclude[0]`}},
		{"to none", rule("x", "forge: gh, owner: me", "forge: home", ""), []string{`rules[0] (x): to: exactly one of owner or repo`}},
		{"to both", rule("x", "forge: gh, owner: me", "forge: home, owner: m, repo: a/b", ""), []string{`rules[0] (x): to: exactly one of owner or repo`}},
		{"to repo with name", rule("x", "forge: gh, owner: me", "forge: home, repo: a/b, name: z", ""), []string{`rules[0] (x): to.name is not allowed with repo`}},
		{"to repo bad", rule("x", "forge: gh, owner: me", "forge: home, repo: ab", ""), []string{`to.repo "ab" must be owner/name`}},
		{"bad template", rule("x", "forge: gh, owner: me", `forge: home, owner: m, name: "{{.Name"`, ""), []string{`rules[0] (x): to.name`}},
		{"bad owner template", rule("x", "forge: gh, owner: me", `forge: home, owner: "{{.Nope"`, ""), []string{`rules[0] (x): to.owner`}},
		{"unknown template field", rule("x", "forge: gh, owner: me", `forge: home, owner: m, name: "{{.Bogus}}"`, ""), []string{`rules[0] (x): to.name`}},
		{"bad interval", okRule + "    interval: 30s\n", []string{`rules[0] (r1): interval`}},
		{"unparsable interval", okRule + "    interval: soon\n", []string{`rules[0] (r1): interval`}},
		{"bad visibility", okRule + "    visibility: public\n", []string{`rules[0] (r1): visibility`}},
		{"bad on_source_deleted", okRule + "    on_source_deleted: nuke\n", []string{`rules[0] (r1): on_source_deleted`}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parse(t, doc("", tt.rules), nil, nil)
			checkErr(t, err, tt.want...)
		})
	}
}

func TestSafetyChecks(t *testing.T) {
	tests := []struct {
		name  string
		rules string
		want  string // empty = valid
	}{
		{"same repo", rule("x", "forge: gh, repo: a/b", "forge: gh, repo: a/b", ""), "source and destination are the same"},
		{"same repo case-insensitive", rule("x", "forge: gh, repo: a/b", "forge: gh, repo: A/B", ""), "source and destination are the same"},
		{"same names different forges ok", rule("x", "forge: gh, repo: a/b", "forge: home, repo: a/b", ""), ""},
		{"duplicate destination",
			rule("x", "forge: gh, repo: a/b", "forge: home, repo: d/e", "") + rule("y", "forge: gh, repo: c/d", "forge: home, repo: d/e", ""),
			"rules[1] (y): destination home:d/e is already the destination of rule \"x\""},
		{"same dest different forge ok",
			rule("x", "forge: gh, repo: a/b", "forge: home, repo: d/e", "") + rule("y", "forge: home, repo: c/d", "forge: gh, repo: d/e", ""),
			""},
		{"mirror loop",
			rule("x", "forge: gh, repo: a/b", "forge: home, repo: d/e", "") + rule("y", "forge: home, repo: d/e", "forge: gh, repo: a/b", ""),
			"mirror loop"},
		{"no loop one direction",
			rule("x", "forge: gh, repo: a/b", "forge: home, repo: d/e", "") + rule("y", "forge: home, repo: d/e", "forge: gh, repo: a/other", ""),
			""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parse(t, doc("", tt.rules), nil, nil)
			checkErr(t, err, tt.want)
		})
	}
}

func TestIncludeValidation(t *testing.T) {
	tests := []struct {
		name, extra, want string
	}{
		{"unknown", "    include: [git, nope]\n", `rules[0] (r1): include: unknown entry "nope"`},
		{"lfs without git", "    include: [lfs]\n", `rules[0] (r1): include: "lfs" requires git`},
		{"releases without git", "    include: [settings, releases]\n", `include: "releases" requires git`},
		{"wiki without git", "    include: [wiki]\n", `include: "wiki" requires git`},
		{"empty", "    include: []\n", `rules[0] (r1): include: must not be empty`},
		{"settings alone ok", "    include: [settings]\n", ""},
		{"ok", "    include: [git, wiki]\n", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parse(t, doc("", okRule+tt.extra), nil, nil)
			checkErr(t, err, tt.want)
		})
	}
	t.Run("defaults include", func(t *testing.T) {
		_, err := parse(t, doc("defaults: {include: [lfs]}", okRule), nil, nil)
		checkErr(t, err, `defaults: include: "lfs" requires git`)
	})
	t.Run("defaults empty include", func(t *testing.T) {
		_, err := parse(t, doc("defaults: {include: []}", okRule), nil, nil)
		checkErr(t, err, `defaults: include: must not be empty`)
	})
	t.Run("dedupe", func(t *testing.T) {
		c := mustParse(t, doc("defaults: {include: [git, git, wiki, git]}", okRule), nil, nil)
		if !reflect.DeepEqual(c.Defaults.Include, []string{"git", "wiki"}) {
			t.Errorf("got %v", c.Defaults.Include)
		}
	})
}

func TestTopLevelValidation(t *testing.T) {
	tests := []struct {
		name, doc, want string
	}{
		{"concurrency zero-negative", doc("concurrency: -1", okRule), "concurrency"},
		{"concurrency too big", doc("concurrency: 65", okRule), "concurrency"},
		{"concurrency 64 ok", doc("concurrency: 64", okRule), ""},
		{"defaults interval low", doc("defaults: {interval: 59s}", okRule), "defaults: interval"},
		{"defaults interval 1m ok", doc("defaults: {interval: 1m}", okRule), ""},
		{"defaults visibility", doc("defaults: {visibility: x}", okRule), "defaults: visibility"},
		{"defaults on_source_deleted", doc("defaults: {on_source_deleted: x}", okRule), "defaults: on_source_deleted"},
		// Zero rules is valid (rules can be added in the web UI); sync and
		// plan refuse to run without any.
		{"no rules key", forgesBlock, ""},
		{"empty rules", forgesBlock + "rules: []\n", ""},
		{"empty doc", "", ""},
		{"unknown top key", doc("bogus: 1", okRule), "bogus"},
		{"unknown forge key", strings.Replace(doc("", okRule), "type: github,", "type: github, tokn: x,", 1), "tokn"},
		{"unknown rule key", okRuleWith("    wat: 1\n"), "wat"},
		{"unknown filter key", rulesDoc(rule("x", "forge: gh, owner: me, filter: {forkz: true}", "forge: home, owner: m", "")), "forkz"},
		{"bad yaml", "forges: [", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parse(t, tt.doc, nil, nil)
			if tt.name == "bad yaml" {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			checkErr(t, err, tt.want)
		})
	}
}

func okRuleWith(extra string) string { return rulesDoc(okRule + extra) }
func rulesDoc(r string) string       { return doc("", r) }

func TestMultipleErrorsReportedTogether(t *testing.T) {
	d := `
concurrency: 100
forges:
  gh: {type: github, token: t}
rules:
  - name: a
    from: {forge: gh, owner: me}
    to: {forge: nope, owner: m}
  - name: b
    from: {forge: gh}
    to: {forge: gh, owner: m}
    interval: 1s
`
	_, err := parse(t, d, nil, nil)
	checkErr(t, err, "concurrency", `rules[0] (a): to.forge "nope" is not defined`, "rules[1] (b): from: exactly one", "rules[1] (b): interval")
	var j interface{ Unwrap() []error }
	if !errors.As(err, &j) {
		t.Errorf("expected errors.Join-style error, got %T", err)
	}
}

func TestValidateDirect(t *testing.T) {
	c := mustParse(t, doc("", okRule), nil, nil)
	if err := c.Validate(); err != nil {
		t.Fatalf("validate on parsed config: %v", err)
	}
	c.Rules[0].To.Forge = "zzz"
	checkErr(t, c.Validate(), `rules[0] (r1): to.forge "zzz" is not defined`)
}

func TestNoSecretLeaks(t *testing.T) {
	d := `
forges:
  gh: {type: github, token: ` + secret + `}
  home: {type: gitea, url: 'https://x', token_file: /s/t}
server: {webhook_secret: ` + secret + `}
rules:
` + okRule
	c := mustParse(t, d, nil, map[string]string{"/s/t": secret})

	outputs := map[string]string{
		"%v cfg":       fmt.Sprintf("%v", c),
		"%v cfg value": fmt.Sprintf("%v", *c),
		"%+v cfg":      fmt.Sprintf("%+v", *c),
		"%#v forge":    fmt.Sprintf("%#v", c.Forges["gh"]),
		"%v forge":     fmt.Sprintf("%v", c.Forges["gh"]),
		"%+v forge":    fmt.Sprintf("%+v", c.Forges["gh"]),
		"%s forge":     c.Forges["home"].String(),
		"%v *forge":    fmt.Sprintf("%v", func() *Forge { f := c.Forges["gh"]; return &f }()),
		"%+v server":   fmt.Sprintf("%+v", c.Server),
		"redacted":     fmt.Sprintf("%+v", c.Redacted()),
		"redacted %v":  fmt.Sprintf("%v", c.Redacted()),
	}
	var sb strings.Builder
	h := slog.NewTextHandler(&sb, nil)
	slog.New(h).Info("x", "forge", c.Forges["gh"], "cfg", c, "server", c.Server)
	outputs["slog text"] = sb.String()
	sb.Reset()
	slog.New(slog.NewJSONHandler(&sb, nil)).Info("x", "forge", c.Forges["gh"], "server", c.Server)
	outputs["slog json"] = sb.String()

	for name, out := range outputs {
		if strings.Contains(out, secret) {
			t.Errorf("%s leaks secret: %s", name, out)
		}
	}
	if !strings.Contains(outputs["%+v forge"], "type") && !strings.Contains(outputs["%+v forge"], "github") {
		t.Errorf("forge string unhelpful: %s", outputs["%+v forge"])
	}
	// Redacted must not mutate the original.
	if c.Forges["gh"].Token != secret {
		t.Errorf("Redacted mutated original")
	}
	if r := c.Redacted(); r.Forges["gh"].Token == secret || r.Server.WebhookSecret == secret {
		t.Errorf("Redacted still holds secret")
	}
}

func TestErrorsNeverContainSecretValues(t *testing.T) {
	// Various failure modes with the secret as literal values, env values, and file contents.
	docs := []string{
		"forges:\n  gh: {type: bogus, token: " + secret + "}\nrules:\n" + okRule,
		"forges:\n  gh: {type: github, token: " + secret + ", token_file: /s/t}\nrules: []\n",
		"forges:\n  gh: {type: github, token: " + secret + ", wat: 1}\nrules:\n" + okRule,
		"forges:\n  gh: {type: github, token: ${S}}\nserver: {webhook_secret: ${S}, webhook_secret_file: /s/t}\nrules:\n" + okRule,
		"forges:\n  gh: {type: github, token: " + secret + "\nrules: [",
		"forges:\n  gh: {type: github, token: " + secret + "}\nconcurrency: " + secret + "\nrules:\n" + okRule,
	}
	for i, d := range docs {
		_, err := parse(t, d, map[string]string{"S": secret}, map[string]string{"/s/t": secret})
		if err == nil {
			t.Errorf("doc %d: expected error", i)
			continue
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("doc %d leaks secret: %v", i, err)
		}
	}
}

func TestRender(t *testing.T) {
	dest := func(owner, name, repo string) Dest { return Dest{Forge: "home", Owner: owner, Name: name, Repo: repo} }
	tests := []struct {
		name         string
		d            Dest
		owner, repo  string
		wantO, wantN string
		wantErr      string
	}{
		{"default name", dest("mirrors", "", ""), "me", "proj", "mirrors", "proj", ""},
		{"name template", dest("stars", "{{.Owner}}-{{.Name}}", ""), "me", "proj", "stars", "me-proj", ""},
		{"owner template", dest("{{.Owner}}", "{{.Name}}", ""), "me", "proj", "me", "proj", ""},
		{"literal repo", dest("", "", "a/b"), "x", "y", "a", "b", ""},
		{"dots ok", dest("m", "{{.Name}}", ""), "me", "a.b_c-d", "m", "a.b_c-d", ""},
		{"traversal", dest("m", "../{{.Name}}", ""), "me", "p", "", "", "invalid"},
		{"traversal in value", dest("m", "{{.Name}}", ""), "me", "../etc", "", "", "invalid"},
		{"slash in value", dest("m", "{{.Name}}", ""), "me", "a/b", "", "", "invalid"},
		{"dot", dest("m", "{{.Name}}", ""), "me", ".", "", "", "invalid"},
		{"dotdot", dest("m", "{{.Name}}", ""), "me", "..", "", "", "invalid"},
		{"git suffix", dest("m", "{{.Name}}.git", ""), "me", "p", "", "", ".git"},
		{"git suffix from source", dest("m", "{{.Name}}", ""), "me", "p.git", "", "", ".git"},
		{"empty", dest("m", "{{.Owner}}{{.Name}}", ""), "", "", "", "", "invalid"},
		{"space", dest("m", "a b", ""), "me", "p", "", "", "invalid"},
		{"bad owner", dest("../x", "{{.Name}}", ""), "me", "p", "", "", "invalid"},
		{"literal repo bad", dest("", "", "a/b.git"), "x", "y", "", "", ".git"},
		{"exec error", dest("m", "{{.Bogus}}", ""), "me", "p", "", "", "name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o, n, err := tt.d.Render(tt.owner, tt.repo)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if o != tt.wantO || n != tt.wantN {
				t.Errorf("got %q/%q want %q/%q", o, n, tt.wantO, tt.wantN)
			}
		})
	}
}

func TestLoad(t *testing.T) {
	p := t.TempDir() + "/c.yaml"
	if err := os.WriteFile(p, []byte(doc("", okRule)), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil || len(c.Rules) != 1 {
		t.Fatalf("Load: %v", err)
	}
	if _, err := Load(p + ".missing"); err == nil {
		t.Error("expected error for missing file")
	}
}
