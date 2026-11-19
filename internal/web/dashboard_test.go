package web

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"gitmirror/internal/config"
)

func TestDashboardEmptyStates(t *testing.T) {
	w := newWorld(t, "")
	c := w.admin()
	mustContain(t, c.get("/"), "Start by adding a site", `href="/sites/new"`)
	w.addSites()
	mustContain(t, c.get("/"), "Now set up your first mirror", `href="/mirrors/new"`)
}

var pairLinkRe = regexp.MustCompile(`href="(/pair\?k=[^"]+)"`)

func TestMirrorsListExplainsFailures(t *testing.T) {
	w := newWorld(t, "")
	w.addSites()
	w.addRepo(w.gh.Provider, "alice", "two")
	w.addRepo(w.gt.Provider, "carol", "two") // not made by gitmirror: refused
	w.putRule(config.Rule{Name: "bad", From: config.Source{Forge: "github", Repo: "alice/two"}, To: config.Dest{Forge: "git-example-com", Repo: "carol/two"}})
	if _, err := w.app.Sync(context.Background(), []string{"bad"}, nil, false, "cli"); err != nil {
		t.Fatal(err)
	}
	c := w.admin()
	m := c.get("/mirrors")
	mustContain(t, m, "Failed", "github: alice/two", "already exists at the destination", `href="/pair?k=`)
	w.noSecrets(m.body)
}

func TestDashboardStatuses(t *testing.T) {
	w := newWorld(t, "")
	w.addSites()
	w.addRepo(w.gh.Provider, "alice", "one")
	w.addRepo(w.gh.Provider, "alice", "two")
	w.addRepo(w.gt.Provider, "carol", "two") // not made by gitmirror: refused
	w.putRule(config.Rule{Name: "good", From: config.Source{Forge: "github", Repo: "alice/one"}, To: config.Dest{Forge: "git-example-com", Repo: "carol/one"}})
	w.putRule(config.Rule{Name: "bad", From: config.Source{Forge: "github", Repo: "alice/two"}, To: config.Dest{Forge: "git-example-com", Repo: "carol/two"}})
	w.putRule(config.Rule{Name: "later", From: config.Source{Forge: "github", Owner: "nobody"}, To: config.Dest{Forge: "git-example-com", Owner: "carol"}})
	if _, err := w.app.Sync(context.Background(), []string{"good", "bad"}, nil, false, "cli"); err != nil {
		t.Fatal(err)
	}
	w.clock.now = time.Now().Add(5 * time.Minute)
	c := w.admin()
	d := c.get("/")
	mustContain(t, d,
		"github: alice/one", "git-example-com: carol/one", "OK", "5 minutes ago",
		"Failed", "already exists at the destination", "Allow copying into repositories that already exist",
		"Not copied yet", `action="/sync"`, `action="/mirrors/good/sync"`,
		`href="https://git.example.com/carol/one"`, "<strong>3</strong> mirrors")
	w.noSecrets(d.body)
	if !regexp.MustCompile(`<time[^>]+title="[^"]+"`).MatchString(d.body) {
		t.Fatal("no absolute time in a title attribute")
	}

	links := pairLinkRe.FindAllStringSubmatch(d.body, -1)
	if len(links) < 2 {
		t.Fatalf("pair links = %v", links)
	}
	for _, l := range links {
		p := c.get(strings.ReplaceAll(l[1], "&amp;", "&"))
		if p.code != http.StatusOK {
			t.Fatalf("GET %s = %d", l[1], p.code)
		}
		mustContain(t, p, "Recent copies")
		if strings.Contains(p.body, "carol/one") {
			mustContain(t, p, "Code (branches and tags)", "Wiki", "OK")
		}
	}
	if r := c.get("/pair?k=bm9wZQ"); r.code != http.StatusNotFound {
		t.Fatalf("unknown pair = %d", r.code)
	}

	mustContain(t, c.get("/mirrors"), "good", "From github: alice/one", "To git-example-com: carol/one", "every hour", "Failed")
}

func TestExplainKnownProblems(t *testing.T) {
	for msg, want := range map[string]string{
		"git: source has no refs but previously had 3; refusing to prune destination":                                          "suddenly empty",
		"git: sync would delete 9 of 10 destination refs (guard: more than 5 refs and more than 50%); refusing to push. If...": "gitmirror sync --allow-prune",
		"destination dst:carol/x already exists and was not created by gitmirror; refusing to write to it. Set adopt: true":    "Allow copying into repositories that already exist",
		"create destination dst:carol/x: gitea: POST /orgs: status 403: forbidden":                                             "token is not allowed",
		"github: get authenticated user: 401 Bad credentials":                                                                  "refused the token",
		"interrupted": "interrupted",
	} {
		if got := explain(msg).Text; !strings.Contains(got, want) {
			t.Errorf("explain(%q) = %q, want it to mention %q", msg, got, want)
		}
	}
}

func TestEveryLabel(t *testing.T) {
	for d, want := range map[time.Duration]string{
		15 * time.Minute: "every 15 minutes", time.Hour: "every hour", 24 * time.Hour: "every day",
		30 * time.Minute: "every 30m", 2 * time.Hour: "every 2h", 90 * time.Minute: "every 1h30m",
	} {
		if got := everyLabel(d); got != want {
			t.Errorf("everyLabel(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestRelativeTime(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	for d, want := range map[time.Duration]string{
		10 * time.Second: "just now", time.Minute: "1 minute ago", 5 * time.Minute: "5 minutes ago",
		2 * time.Hour: "2 hours ago", 49 * time.Hour: "2 days ago",
	} {
		if got := relTime(now, now.Add(-d)); got != want {
			t.Errorf("%v: %q, want %q", d, got, want)
		}
	}
	if got := relTime(now, time.Time{}); got != "never" {
		t.Errorf("zero = %q", got)
	}
}
