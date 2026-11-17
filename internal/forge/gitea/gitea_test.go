package gitea

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gitmirror/internal/forge"
)

const tok = "SENTINEL-TOK"

var (
	_ forge.Provider        = (*Provider)(nil)
	_ forge.ReleaseProvider = (*Provider)(nil)
	_ forge.WikiProvider    = (*Provider)(nil)
	_ forge.StarProvider    = (*Provider)(nil)
	_ forge.OrgProvider     = (*Provider)(nil)
	_ forge.WebhookProvider = (*Provider)(nil)
)

// calls records "METHOD path" for every request that reaches the server.
type calls struct {
	mu sync.Mutex
	l  []string
}

func (c *calls) add(s string) { c.mu.Lock(); c.l = append(c.l, s); c.mu.Unlock() }
func (c *calls) all() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.l...)
}

// serve wraps mux, asserts the auth header on every request and records calls.
func serve(t *testing.T, mux *http.ServeMux) (*httptest.Server, *calls) {
	t.Helper()
	c := &calls{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "token "+tok {
			t.Errorf("%s %s: Authorization = %q", r.Method, r.URL.Path, got)
		}
		c.add(r.Method + " " + r.URL.Path)
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, c
}

func newProv(t *testing.T, url string) *Provider {
	t.Helper()
	p, err := New(forge.Options{URL: url, Token: tok})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func msg(w http.ResponseWriter, code int, m string) {
	writeJSON(w, code, map[string]string{"message": m})
}

func readBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		t.Errorf("decode body: %v", err)
	}
	return m
}

func ctx() context.Context { return context.Background() }

func TestNewAndRegistry(t *testing.T) {
	if _, err := New(forge.Options{Token: tok}); err == nil {
		t.Error("missing URL accepted")
	}
	if _, err := New(forge.Options{URL: "https://h"}); err == nil {
		t.Error("missing token accepted")
	}
	p, err := New(forge.Options{URL: "https://h/gitea/", Token: tok})
	if err != nil {
		t.Fatal(err)
	}
	if p.api != "https://h/gitea/api/v1" {
		t.Errorf("api = %q", p.api)
	}
	if p.Type() != "gitea" {
		t.Errorf("type %q", p.Type())
	}
	fp, err := forge.New("gitea", forge.Options{URL: "https://h", Token: tok})
	if err != nil || fp.Type() != "gitea" {
		t.Errorf("registry: %v %v", fp, err)
	}
}

func TestGitURLsAndAuth(t *testing.T) {
	p := newProv(t, "https://h/gitea/")
	if got := p.GitURL("o", "n"); got != "https://h/gitea/o/n.git" {
		t.Error(got)
	}
	if got := p.WikiGitURL("o", "n"); got != "https://h/gitea/o/n.wiki.git" {
		t.Error(got)
	}
	if a := p.GitAuth(); a.Token != tok || a.Username == "" {
		t.Errorf("auth %+v", a)
	}
	p2, _ := New(forge.Options{URL: "https://h", Token: tok, Username: "bob"})
	if a := p2.GitAuth(); a.Username != "bob" || a.Token != tok {
		t.Errorf("auth %+v", a)
	}
}

func TestSubPathGetRepoMapping(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /gitea/api/v1/repos/my-org.x/re-po.y", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{
			"name": "re-po.y", "owner": map[string]any{"login": "my-org.x"},
			"description": "d", "website": "http://w", "topics": []string{"a", "b"},
			"default_branch": "dev", "private": true, "archived": true, "fork": true,
			"empty": true, "has_wiki": true,
		})
	})
	mux.HandleFunc("GET /gitea/api/v1/repos/my-org.x/re-po.y/wiki/pages", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, []map[string]any{{"title": "Home"}})
	})
	srv, _ := serve(t, mux)
	p := newProv(t, srv.URL+"/gitea")
	r, err := p.GetRepo(ctx(), "my-org.x", "re-po.y")
	if err != nil {
		t.Fatal(err)
	}
	want := forge.Repo{Owner: "my-org.x", Name: "re-po.y", Description: "d", Homepage: "http://w",
		Topics: []string{"a", "b"}, DefaultBranch: "dev", Private: true, Archived: true, Fork: true, Empty: true, HasWiki: true}
	if fmt.Sprint(*r) != fmt.Sprint(want) {
		t.Errorf("got %+v\nwant %+v", *r, want)
	}
}

func TestGetRepoTopicsFallback(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/repos/o/n", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"name": "n", "owner": map[string]any{"login": "o"}})
	})
	mux.HandleFunc("GET /api/v1/repos/o/n/topics", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"topics": []string{"x", "y"}})
	})
	srv, _ := serve(t, mux)
	r, err := newProv(t, srv.URL).GetRepo(ctx(), "o", "n")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(r.Topics, ",") != "x,y" {
		t.Errorf("topics %v", r.Topics)
	}
}

func TestRepoIDMapped(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/user", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]any{"login": "me"}) })
	mux.HandleFunc("GET /api/v1/repos/o/n", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"id": 55, "name": "n", "owner": map[string]any{"login": "o"}, "topics": []string{}})
	})
	mux.HandleFunc("POST /api/v1/user/repos", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 201, map[string]any{"id": 56, "name": "c", "owner": map[string]any{"login": "me"}})
	})
	mux.HandleFunc("PATCH /api/v1/repos/me/c", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, repoJSON("me", "c")) })
	srv, _ := serve(t, mux)
	p := newProv(t, srv.URL)
	if r, err := p.GetRepo(ctx(), "o", "n"); err != nil || r.ID != 55 {
		t.Fatalf("GetRepo = %+v, %v; want ID 55", r, err)
	}
	if r, err := p.CreateRepo(ctx(), forge.CreateOptions{Owner: "me", Name: "c"}); err != nil || r.ID != 56 {
		t.Fatalf("CreateRepo = %+v, %v; want ID 56", r, err)
	}
}

func TestPathEscapeAndReject(t *testing.T) {
	var escaped string
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		escaped = r.URL.EscapedPath()
		writeJSON(w, 200, map[string]any{"name": "x", "topics": []string{}})
	})
	srv, c := serve(t, mux)
	p := newProv(t, srv.URL)
	if _, err := p.GetRepo(ctx(), "a b", "n#?x"); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(escaped, "/api/v1/repos/a%20b/n%23%3Fx") {
		t.Errorf("escaped path %q", escaped)
	}
	n := len(c.all())
	for _, bad := range [][2]string{{"a/b", "n"}, {"o", "n/m"}, {"", "n"}, {"o", ""}, {"o", ".."}} {
		if _, err := p.GetRepo(ctx(), bad[0], bad[1]); err == nil {
			t.Errorf("accepted %v", bad)
		}
	}
	if len(c.all()) != n {
		t.Error("rejected names still hit the network")
	}
}

func TestErrors(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/repos/o/missing", func(w http.ResponseWriter, r *http.Request) {
		msg(w, 404, "repo does not exist")
	})
	mux.HandleFunc("GET /api/v1/repos/o/unauth", func(w http.ResponseWriter, r *http.Request) {
		msg(w, 401, "bad credentials")
	})
	mux.HandleFunc("GET /api/v1/repos/o/huge", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte(strings.Repeat("A", 1<<20)))
	})
	srv, _ := serve(t, mux)
	p := newProv(t, srv.URL)

	_, err := p.GetRepo(ctx(), "o", "missing")
	if !errors.Is(err, forge.ErrNotFound) || !strings.Contains(err.Error(), "repo does not exist") {
		t.Errorf("404: %v", err)
	}
	_, err = p.GetRepo(ctx(), "o", "unauth")
	if err == nil || errors.Is(err, forge.ErrNotFound) || !strings.Contains(err.Error(), "bad credentials") || strings.Contains(err.Error(), tok) {
		t.Errorf("401: %v", err)
	}
	_, err = p.GetRepo(ctx(), "o", "huge")
	if err == nil || len(err.Error()) > 16<<10 {
		t.Errorf("huge: err len %d", len(fmt.Sprint(err)))
	}
}

func TestNoTokenInAnyError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { msg(w, 500, "boom") })
	srv, _ := serve(t, mux)
	p := newProv(t, srv.URL)
	p2 := newProv(t, "http://127.0.0.1:1") // connection refused path
	var errs []error
	for _, pp := range []*Provider{p, p2} {
		_, e1 := pp.GetRepo(ctx(), "o", "n")
		_, e2 := pp.ListRepos(ctx(), "o")
		_, e3 := pp.CreateRepo(ctx(), forge.CreateOptions{Owner: "o", Name: "n"})
		e4 := pp.UpdateRepoSettings(ctx(), "o", "n", forge.Settings{Description: ptr("x")})
		_, e5 := pp.ListReleases(ctx(), "o", "n")
		_, e6 := pp.DownloadAsset(ctx(), "o", "n", forge.Asset{ID: 1})
		_, e7 := pp.UploadAsset(ctx(), "o", "n", 1, forge.Asset{Name: "a"}, strings.NewReader("x"))
		e8 := pp.EnsureOrg(ctx(), "org")
		_, e9 := pp.ListStarred(ctx(), "u")
		errs = append(errs, e1, e2, e3, e4, e5, e6, e7, e8, e9)
	}
	for i, e := range errs {
		if e == nil {
			t.Errorf("err %d is nil", i)
		} else if strings.Contains(e.Error(), tok) {
			t.Errorf("err %d leaks token: %v", i, e)
		}
	}
}

func ptr[T any](v T) *T { return &v }

func TestRateLimit(t *testing.T) {
	r, err := newProv(t, "http://127.0.0.1:1").RateLimit(ctx())
	if err != nil || r != (forge.Rate{}) {
		t.Errorf("%+v %v", r, err)
	}
}

func repoJSON(owner, name string) map[string]any {
	return map[string]any{"name": name, "owner": map[string]any{"login": owner}, "topics": []string{}}
}

func TestListReposOrgThreePagesStopsOnEmpty(t *testing.T) {
	// Server caps page size at 2 regardless of limit=50.
	all := []string{"r1", "r2", "r3", "r4", "r5"}
	var pages []string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/orgs/org", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]any{"username": "org"}) })
	mux.HandleFunc("GET /api/v1/orgs/org/repos", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("limit") != "50" {
			t.Errorf("limit = %q", r.URL.Query().Get("limit"))
		}
		pg, _ := strconv.Atoi(r.URL.Query().Get("page"))
		pages = append(pages, r.URL.Query().Get("page"))
		var out []map[string]any
		for i := (pg - 1) * 2; i < pg*2 && i < len(all); i++ {
			out = append(out, repoJSON("org", all[i]))
		}
		writeJSON(w, 200, out)
	})
	srv, _ := serve(t, mux)
	rs, err := newProv(t, srv.URL).ListRepos(ctx(), "org")
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 5 || rs[4].Name != "r5" || rs[0].Owner != "org" {
		t.Errorf("got %+v", rs)
	}
	if strings.Join(pages, ",") != "1,2,3,4" {
		t.Errorf("pages requested %v (must continue past short page until empty)", pages)
	}
}

func TestListReposHonoursTotalCount(t *testing.T) {
	var pages int
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/orgs/org", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]any{}) })
	mux.HandleFunc("GET /api/v1/orgs/org/repos", func(w http.ResponseWriter, r *http.Request) {
		pages++
		w.Header().Set("X-Total-Count", "2")
		writeJSON(w, 200, []map[string]any{repoJSON("org", "a"), repoJSON("org", "b")})
	})
	srv, _ := serve(t, mux)
	rs, err := newProv(t, srv.URL).ListRepos(ctx(), "org")
	if err != nil || len(rs) != 2 || pages != 1 {
		t.Errorf("rs=%d pages=%d err=%v", len(rs), pages, err)
	}
}

func TestListReposUsers(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/orgs/{o}", func(w http.ResponseWriter, r *http.Request) { msg(w, 404, "no org") })
	mux.HandleFunc("GET /api/v1/user", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"login": "me"})
	})
	mux.HandleFunc("GET /api/v1/user/repos", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") != "1" {
			writeJSON(w, 200, []any{})
			return
		}
		// includes repos of another owner (collaborations / orgs) that must be filtered out.
		writeJSON(w, 200, []map[string]any{repoJSON("me", "mine"), repoJSON("other", "theirs"), repoJSON("me", "secret")})
	})
	mux.HandleFunc("GET /api/v1/users/bob/repos", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") != "1" {
			writeJSON(w, 200, []any{})
			return
		}
		writeJSON(w, 200, []map[string]any{repoJSON("bob", "b1")})
	})
	mux.HandleFunc("GET /api/v1/users/ghost/repos", func(w http.ResponseWriter, r *http.Request) { msg(w, 404, "user does not exist") })
	srv, c := serve(t, mux)
	p := newProv(t, srv.URL)

	rs, err := p.ListRepos(ctx(), "me")
	if err != nil || len(rs) != 2 || rs[0].Name != "mine" || rs[1].Name != "secret" {
		t.Errorf("self: %+v %v", rs, err)
	}
	rs, err = p.ListRepos(ctx(), "bob")
	if err != nil || len(rs) != 1 || rs[0].Name != "b1" {
		t.Errorf("bob: %+v %v", rs, err)
	}
	if _, err = p.ListRepos(ctx(), "ghost"); !errors.Is(err, forge.ErrNotFound) {
		t.Errorf("ghost: %v", err)
	}
	n := 0
	for _, s := range c.all() {
		if s == "GET /api/v1/user" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("login fetched %d times, want cached once", n)
	}
	if l, err := p.Login(ctx()); err != nil || l != "me" {
		t.Errorf("Login %q %v", l, err)
	}
}

func TestCreateRepo(t *testing.T) {
	var got []string
	var bodies []map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/user", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]any{"login": "me"}) })
	mux.HandleFunc("POST /api/v1/user/repos", func(w http.ResponseWriter, r *http.Request) {
		b := readBody(t, r)
		bodies = append(bodies, b)
		got = append(got, "user")
		if b["name"] == "dup" {
			msg(w, 409, "The repository with the same name already exists.")
			return
		}
		writeJSON(w, 201, repoJSON("me", b["name"].(string)))
	})
	mux.HandleFunc("POST /api/v1/orgs/org/repos", func(w http.ResponseWriter, r *http.Request) {
		got = append(got, "org")
		writeJSON(w, 201, repoJSON("org", readBody(t, r)["name"].(string)))
	})
	mux.HandleFunc("POST /api/v1/orgs/nope/repos", func(w http.ResponseWriter, r *http.Request) { msg(w, 404, "org does not exist") })
	mux.HandleFunc("PATCH /api/v1/repos/{o}/{n}", func(w http.ResponseWriter, r *http.Request) {
		b := readBody(t, r)
		if b["has_wiki"] != true {
			t.Errorf("wiki patch body %v", b)
		}
		got = append(got, "wiki:"+r.PathValue("o"))
		writeJSON(w, 200, repoJSON(r.PathValue("o"), r.PathValue("n")))
	})
	srv, _ := serve(t, mux)
	p := newProv(t, srv.URL)

	r, err := p.CreateRepo(ctx(), forge.CreateOptions{Owner: "me", Name: "a", Description: "desc", Private: true})
	if err != nil || r.Owner != "me" || r.Name != "a" {
		t.Fatalf("%+v %v", r, err)
	}
	b := bodies[0]
	if b["name"] != "a" || b["description"] != "desc" || b["private"] != true || b["auto_init"] != false {
		t.Errorf("body %v", b)
	}
	if _, err = p.CreateRepo(ctx(), forge.CreateOptions{Owner: "org", Name: "b"}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "user,wiki:me,org,wiki:org" {
		t.Errorf("sequence %v", got)
	}
	if _, err = p.CreateRepo(ctx(), forge.CreateOptions{Owner: "me", Name: "dup"}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("409: %v", err)
	}
	if _, err = p.CreateRepo(ctx(), forge.CreateOptions{Owner: "nope", Name: "x"}); !errors.Is(err, forge.ErrNotFound) {
		t.Errorf("404: %v", err)
	}
}

func settingsServer(t *testing.T) (*httptest.Server, *calls, *[]map[string]any) {
	var bodies []map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("PATCH /api/v1/repos/o/n", func(w http.ResponseWriter, r *http.Request) {
		bodies = append(bodies, readBody(t, r))
		writeJSON(w, 200, repoJSON("o", "n"))
	})
	mux.HandleFunc("PUT /api/v1/repos/o/n/topics", func(w http.ResponseWriter, r *http.Request) {
		bodies = append(bodies, readBody(t, r))
		w.WriteHeader(204)
	})
	srv, c := serve(t, mux)
	return srv, c, &bodies
}

func TestUpdateSettingsArchiveFalseFirst(t *testing.T) {
	srv, c, bodies := settingsServer(t)
	err := newProv(t, srv.URL).UpdateRepoSettings(ctx(), "o", "n", forge.Settings{
		Description: ptr("d"), Homepage: ptr("h"), DefaultBranch: ptr("main"), Private: ptr(true),
		Topics: ptr([]string{"Go", "CLI"}), Archived: ptr(false),
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"PATCH /api/v1/repos/o/n", "PATCH /api/v1/repos/o/n", "PUT /api/v1/repos/o/n/topics"}
	if strings.Join(c.all(), "|") != strings.Join(want, "|") {
		t.Fatalf("order %v", c.all())
	}
	b := *bodies
	if len(b[0]) != 1 || b[0]["archived"] != false {
		t.Errorf("first %v", b[0])
	}
	if b[1]["description"] != "d" || b[1]["website"] != "h" || b[1]["default_branch"] != "main" || b[1]["private"] != true {
		t.Errorf("main %v", b[1])
	}
	if _, has := b[1]["archived"]; has {
		t.Error("archived in main patch")
	}
	if fmt.Sprint(b[2]["topics"]) != "[go cli]" {
		t.Errorf("topics %v", b[2])
	}
}

func TestUpdateSettingsArchiveTrueLast(t *testing.T) {
	srv, c, bodies := settingsServer(t)
	err := newProv(t, srv.URL).UpdateRepoSettings(ctx(), "o", "n", forge.Settings{
		Description: ptr("d"), Topics: ptr([]string{"a"}), Archived: ptr(true),
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"PATCH /api/v1/repos/o/n", "PUT /api/v1/repos/o/n/topics", "PATCH /api/v1/repos/o/n"}
	if strings.Join(c.all(), "|") != strings.Join(want, "|") {
		t.Fatalf("order %v", c.all())
	}
	if last := (*bodies)[2]; len(last) != 1 || last["archived"] != true {
		t.Errorf("last %v", last)
	}
}

func TestUpdateSettingsEmptyMakesNoCalls(t *testing.T) {
	srv, c, _ := settingsServer(t)
	if err := newProv(t, srv.URL).UpdateRepoSettings(ctx(), "o", "n", forge.Settings{}); err != nil {
		t.Fatal(err)
	}
	if len(c.all()) != 0 {
		t.Errorf("calls %v", c.all())
	}
}

func TestReleasesCRUD(t *testing.T) {
	mux := http.NewServeMux()
	rel := func(id int, tag string) map[string]any {
		return map[string]any{"id": id, "tag_name": tag, "name": "N" + tag, "body": "B", "target_commitish": "main",
			"draft": id == 2, "prerelease": id == 3,
			"assets": []map[string]any{{"id": id * 10, "name": "f.zip", "size": 42, "browser_download_url": "http://x/f"}}}
	}
	mux.HandleFunc("GET /api/v1/repos/o/n/releases", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("page") {
		case "1":
			writeJSON(w, 200, []any{rel(1, "v1"), rel(2, "v2")})
		case "2":
			writeJSON(w, 200, []any{rel(3, "v3")})
		default:
			writeJSON(w, 200, []any{})
		}
	})
	mux.HandleFunc("POST /api/v1/repos/o/n/releases", func(w http.ResponseWriter, r *http.Request) {
		b := readBody(t, r)
		if b["tag_name"] != "v9" || b["name"] != "Nine" || b["body"] != "bb" || b["target_commitish"] != "dev" || b["draft"] != true || b["prerelease"] != false {
			t.Errorf("create body %v", b)
		}
		writeJSON(w, 201, rel(9, "v9"))
	})
	mux.HandleFunc("PATCH /api/v1/repos/o/n/releases/9", func(w http.ResponseWriter, r *http.Request) {
		if readBody(t, r)["tag_name"] != "v9" {
			t.Error("update body")
		}
		writeJSON(w, 200, rel(9, "v9"))
	})
	mux.HandleFunc("DELETE /api/v1/repos/o/n/releases/9", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	srv, _ := serve(t, mux)
	p := newProv(t, srv.URL)

	rs, err := p.ListReleases(ctx(), "o", "n")
	if err != nil || len(rs) != 3 {
		t.Fatalf("%v %v", rs, err)
	}
	if r := rs[1]; r.ID != 2 || r.Tag != "v2" || r.Name != "Nv2" || r.Body != "B" || r.Target != "main" || !r.Draft ||
		len(r.Assets) != 1 || r.Assets[0] != (forge.Asset{ID: 20, Name: "f.zip", Size: 42}) {
		t.Errorf("mapping %+v", r)
	}
	if !rs[2].Prerelease {
		t.Error("prerelease")
	}
	in := forge.Release{Tag: "v9", Name: "Nine", Body: "bb", Target: "dev", Draft: true}
	cr, err := p.CreateRelease(ctx(), "o", "n", in)
	if err != nil || cr.ID != 9 {
		t.Fatalf("create %v %v", cr, err)
	}
	in.ID = 9
	if ur, err := p.UpdateRelease(ctx(), "o", "n", in); err != nil || ur.ID != 9 {
		t.Fatalf("update %v %v", ur, err)
	}
	if err := p.DeleteRelease(ctx(), "o", "n", 9); err != nil {
		t.Fatal(err)
	}
}

func TestDownloadAssetTokenOnlyToSameHost(t *testing.T) {
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("token leaked to foreign host: %q", r.Header.Get("Authorization"))
		}
		_, _ = io.WriteString(w, "foreign-bytes")
	}))
	defer foreign.Close()

	var srvURL string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/repos/o/n/releases", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") != "1" {
			writeJSON(w, 200, []any{})
			return
		}
		writeJSON(w, 200, []any{map[string]any{"id": 1, "tag_name": "v1", "assets": []map[string]any{
			{"id": 10, "name": "same", "size": 9, "browser_download_url": srvURL + "/attachments/same"},
			{"id": 11, "name": "foreign", "size": 13, "browser_download_url": foreign.URL + "/attachments/foreign"},
		}}})
	})
	mux.HandleFunc("GET /attachments/same", func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "same-bytes") })
	mux.HandleFunc("GET /attachments/gone", func(w http.ResponseWriter, r *http.Request) { msg(w, 404, "gone") })
	srv, _ := serve(t, mux)
	srvURL = srv.URL
	p := newProv(t, srv.URL)

	read := func(a forge.Asset) string {
		rc, err := p.DownloadAsset(ctx(), "o", "n", a)
		if err != nil {
			t.Fatal(err)
		}
		defer rc.Close()
		b, _ := io.ReadAll(rc)
		return string(b)
	}
	// Not cached yet: provider must discover the URL itself.
	if got := read(forge.Asset{ID: 10}); got != "same-bytes" {
		t.Errorf("same: %q", got)
	}
	if got := read(forge.Asset{ID: 11}); got != "foreign-bytes" {
		t.Errorf("foreign: %q", got)
	}
	if _, err := p.DownloadAsset(ctx(), "o", "n", forge.Asset{ID: 999}); !errors.Is(err, forge.ErrNotFound) {
		t.Errorf("unknown asset: %v", err)
	}
}

func TestUploadAssetStreamsMultipart(t *testing.T) {
	const size = 3 << 20
	var gotName, gotField, gotFile string
	var gotLen int64
	var contentLength int64
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/repos/o/n/releases/5/assets", func(w http.ResponseWriter, r *http.Request) {
		contentLength = r.ContentLength
		gotName = r.URL.Query().Get("name")
		mr, err := r.MultipartReader()
		if err != nil {
			t.Error(err)
			msg(w, 400, "bad")
			return
		}
		part, err := mr.NextPart()
		if err != nil {
			t.Error(err)
			msg(w, 400, "bad")
			return
		}
		gotField, gotFile = part.FormName(), part.FileName()
		gotLen, _ = io.Copy(io.Discard, part)
		writeJSON(w, 201, map[string]any{"id": 77, "name": gotName, "size": gotLen})
	})
	srv, _ := serve(t, mux)
	p := newProv(t, srv.URL)

	body := io.LimitReader(zeroReader{}, size)
	a, err := p.UploadAsset(ctx(), "o", "n", 5, forge.Asset{Name: "my file.bin"}, body)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != 77 || a.Size != size || a.Name != "my file.bin" {
		t.Errorf("asset %+v", a)
	}
	if gotName != "my file.bin" || gotField != "attachment" || gotFile != "my file.bin" || gotLen != size {
		t.Errorf("name=%q field=%q file=%q len=%d", gotName, gotField, gotFile, gotLen)
	}
	if contentLength != -1 {
		t.Errorf("ContentLength=%d: body was buffered, not streamed", contentLength)
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }

func TestUploadAssetSourceErrorAborts(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/repos/o/n/releases/5/assets", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		writeJSON(w, 201, map[string]any{"id": 1})
	})
	srv, _ := serve(t, mux)
	_, err := newProv(t, srv.URL).UploadAsset(ctx(), "o", "n", 5, forge.Asset{Name: "a"},
		io.MultiReader(strings.NewReader("abc"), errReader{}))
	if err == nil || !strings.Contains(err.Error(), "source boom") {
		t.Errorf("err %v", err)
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("source boom") }

func TestDeleteAsset(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /api/v1/repos/o/n/releases/5/assets/6", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	srv, _ := serve(t, mux)
	if err := newProv(t, srv.URL).DeleteAsset(ctx(), "o", "n", 5, 6); err != nil {
		t.Fatal(err)
	}
}

func TestListStarred(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/users/u-1/starred", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("page") {
		case "1":
			writeJSON(w, 200, []any{repoJSON("a", "x")})
		case "2":
			writeJSON(w, 200, []any{repoJSON("b", "y")})
		default:
			writeJSON(w, 200, []any{})
		}
	})
	srv, _ := serve(t, mux)
	rs, err := newProv(t, srv.URL).ListStarred(ctx(), "u-1")
	if err != nil || len(rs) != 2 || rs[1].FullName() != "b/y" {
		t.Errorf("%+v %v", rs, err)
	}
}

func TestEnsureOrg(t *testing.T) {
	var created []string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/user", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]any{"login": "me"}) })
	mux.HandleFunc("GET /api/v1/orgs/{o}", func(w http.ResponseWriter, r *http.Request) {
		// "race" is created concurrently: visible once our POST was refused.
		if o := r.PathValue("o"); o == "exists" || (o == "race" && slices.Contains(created, "race")) {
			writeJSON(w, 200, map[string]any{})
			return
		}
		msg(w, 404, "nope")
	})
	mux.HandleFunc("POST /api/v1/orgs", func(w http.ResponseWriter, r *http.Request) {
		b := readBody(t, r)
		if b["visibility"] != "private" {
			t.Errorf("body %v", b)
		}
		name := b["username"].(string)
		created = append(created, name)
		if name == "race" {
			msg(w, 422, "user already exists [name: race]")
			return
		}
		if name == "bad" {
			msg(w, 422, "invalid name")
			return
		}
		writeJSON(w, 201, map[string]any{})
	})
	srv, _ := serve(t, mux)
	p := newProv(t, srv.URL)
	for _, n := range []string{"exists", "me", "fresh", "race"} {
		if err := p.EnsureOrg(ctx(), n); err != nil {
			t.Errorf("%s: %v", n, err)
		}
	}
	if strings.Join(created, ",") != "fresh,race" {
		t.Errorf("created %v", created)
	}
	if err := p.EnsureOrg(ctx(), "bad"); err == nil {
		t.Error("other 422 must error")
	}
}

func sign(body, secret string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(body))
	return hex.EncodeToString(m.Sum(nil))
}

func hook(body, sig, event string, hdrSig, hdrEv string) *http.Request {
	r := httptest.NewRequest("POST", "/hook", strings.NewReader(body))
	if sig != "" {
		r.Header.Set(hdrSig, sig)
	}
	if event != "" {
		r.Header.Set(hdrEv, event)
	}
	return r
}

func TestParseWebhook(t *testing.T) {
	p := newProv(t, "https://h")
	body := `{"repository":{"name":"n","owner":{"login":"o"}}}`
	good := sign(body, "s3")

	for ev, want := range map[string]forge.EventKind{
		"push": forge.EventPush, "create": forge.EventPush, "delete": forge.EventPush,
		"release": forge.EventRelease, "repository": forge.EventRepo, "wiki": forge.EventWiki, "issues": forge.EventOther,
	} {
		e, err := p.ParseWebhook(hook(body, good, ev, "X-Gitea-Signature", "X-Gitea-Event"), "s3")
		if err != nil || e.Kind != want || e.Owner != "o" || e.Name != "n" {
			t.Errorf("%s: %+v %v", ev, e, err)
		}
	}
	// Forgejo headers and owner.username fallback.
	body2 := `{"repository":{"name":"n","owner":{"username":"u"}}}`
	e, err := p.ParseWebhook(hook(body2, sign(body2, "s3"), "push", "X-Forgejo-Signature", "X-Forgejo-Event"), "s3")
	if err != nil || e.Owner != "u" || e.Kind != forge.EventPush {
		t.Errorf("forgejo: %+v %v", e, err)
	}
	// Bad / missing / wrong-secret / uppercase-nothing signatures.
	for name, r := range map[string]*http.Request{
		"missing": hook(body, "", "push", "", "X-Gitea-Event"),
		"bad":     hook(body, "deadbeef", "push", "X-Gitea-Signature", "X-Gitea-Event"),
		"wrong":   hook(body, sign(body, "other"), "push", "X-Gitea-Signature", "X-Gitea-Event"),
		"prefix":  hook(body, "sha256="+good, "push", "X-Gitea-Signature", "X-Gitea-Event"),
		"tamper":  hook(body+" ", good, "push", "X-Gitea-Signature", "X-Gitea-Event"),
	} {
		if _, err := p.ParseWebhook(r, "s3"); !errors.Is(err, forge.ErrBadSignature) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := p.ParseWebhook(hook(body, sign(body, ""), "push", "X-Gitea-Signature", "X-Gitea-Event"), ""); err == nil {
		t.Error("empty secret accepted")
	}
}

func TestParseWebhookBodyCap(t *testing.T) {
	p := newProv(t, "https://h")
	big := strings.Repeat("a", 25<<20+1)
	_, err := p.ParseWebhook(hook(big, sign(big, "s"), "push", "X-Gitea-Signature", "X-Gitea-Event"), "s")
	if err == nil || errors.Is(err, forge.ErrBadSignature) {
		t.Errorf("oversize: %v", err)
	}
}

func TestDeleteRepo(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /api/v1/repos/o/n", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	mux.HandleFunc("DELETE /api/v1/repos/o/gone", func(w http.ResponseWriter, r *http.Request) { msg(w, 404, "not found") })
	mux.HandleFunc("DELETE /api/v1/repos/o/denied", func(w http.ResponseWriter, r *http.Request) { msg(w, 403, "forbidden") })
	srv, c := serve(t, mux)
	p := newProv(t, srv.URL)
	var _ forge.RepoDeleter = p
	if err := p.DeleteRepo(ctx(), "o", "n"); err != nil {
		t.Fatal(err)
	}
	if got := c.all(); len(got) != 1 || got[0] != "DELETE /api/v1/repos/o/n" {
		t.Errorf("calls = %v", got)
	}
	err := p.DeleteRepo(ctx(), "o", "gone")
	if !errors.Is(err, forge.ErrNotFound) {
		t.Errorf("err = %v", err)
	}
	if err != nil && strings.Contains(err.Error(), tok) {
		t.Errorf("leaks token: %v", err)
	}
	err = p.DeleteRepo(ctx(), "o", "denied")
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"write:repository", "admin"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("403 error %q lacks %q", err, want)
		}
	}
	if strings.Contains(err.Error(), tok) {
		t.Errorf("leaks token: %v", err)
	}
	if err := p.DeleteRepo(ctx(), "o", "a/b"); err == nil {
		t.Error("invalid segment accepted")
	}
}

// Gitea reports has_wiki:true for a repository whose wiki was never
// initialised, and answers 404 on its page list (and refuses git on
// <repo>.wiki.git) until a first page exists.
func TestGetRepoWikiNotInitialised(t *testing.T) {
	mux := http.NewServeMux()
	for _, n := range []string{"none", "some", "off", "broken"} {
		mux.HandleFunc("GET /api/v1/repos/o/"+n, func(w http.ResponseWriter, r *http.Request) {
			j := repoJSON("o", n)
			j["has_wiki"] = n != "off"
			writeJSON(w, 200, j)
		})
	}
	mux.HandleFunc("GET /api/v1/repos/o/none/wiki/pages", func(w http.ResponseWriter, r *http.Request) {
		msg(w, 404, "no such file or directory")
	})
	mux.HandleFunc("GET /api/v1/repos/o/some/wiki/pages", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("limit") != "1" {
			t.Errorf("wiki probe query %q", r.URL.RawQuery)
		}
		writeJSON(w, 200, []map[string]any{{"title": "Home"}})
	})
	mux.HandleFunc("GET /api/v1/repos/o/broken/wiki/pages", func(w http.ResponseWriter, r *http.Request) {
		msg(w, 500, "boom")
	})
	srv, c := serve(t, mux)
	p := newProv(t, srv.URL)
	for n, want := range map[string]bool{"none": false, "some": true, "off": false, "broken": true} {
		r, err := p.GetRepo(ctx(), "o", n)
		if err != nil {
			t.Fatalf("%s: %v", n, err)
		}
		if r.HasWiki != want {
			t.Errorf("%s: HasWiki = %v, want %v", n, r.HasWiki, want)
		}
	}
	for _, call := range c.all() {
		if call == "GET /api/v1/repos/o/off/wiki/pages" {
			t.Error("probed the wiki of a repo with the wiki disabled")
		}
	}
}

func TestEnsureWiki(t *testing.T) {
	var pages []map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/repos/o/n/wiki/pages", func(w http.ResponseWriter, r *http.Request) {
		if len(pages) == 0 {
			msg(w, 404, "no such file or directory")
			return
		}
		writeJSON(w, 200, pages)
	})
	mux.HandleFunc("POST /api/v1/repos/o/n/wiki/new", func(w http.ResponseWriter, r *http.Request) {
		b := readBody(t, r)
		if b["title"] != "Home" || b["content_base64"] == "" {
			t.Errorf("body %v", b)
		}
		pages = append(pages, b)
		writeJSON(w, 201, map[string]any{"title": "Home"})
	})
	mux.HandleFunc("GET /api/v1/repos/o/off/wiki/pages", func(w http.ResponseWriter, r *http.Request) { msg(w, 404, "not found") })
	mux.HandleFunc("POST /api/v1/repos/o/off/wiki/new", func(w http.ResponseWriter, r *http.Request) { msg(w, 404, "not found") })
	srv, c := serve(t, mux)
	p := newProv(t, srv.URL)
	if err := p.EnsureWiki(ctx(), "o", "n"); err != nil {
		t.Fatal(err)
	}
	if err := p.EnsureWiki(ctx(), "o", "n"); err != nil {
		t.Fatal(err)
	}
	if len(pages) != 1 {
		t.Errorf("created %d pages, want 1 (second call must be a no-op)", len(pages))
	}
	want := "GET /api/v1/repos/o/n/wiki/pages,POST /api/v1/repos/o/n/wiki/new,GET /api/v1/repos/o/n/wiki/pages"
	if got := strings.Join(c.all(), ","); got != want {
		t.Errorf("calls %s", got)
	}
	if err := p.EnsureWiki(ctx(), "o", "off"); err == nil || !strings.Contains(err.Error(), "wiki enabled") {
		t.Errorf("disabled wiki: %v", err)
	}
}

// POST /orgs answers 422 "user already exists" for a user name too; that
// must not count as an existing organisation.
func TestEnsureOrgNameTakenByUser(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/user", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]any{"login": "me"}) })
	mux.HandleFunc("GET /api/v1/orgs/carol", func(w http.ResponseWriter, r *http.Request) { msg(w, 404, "org does not exist") })
	mux.HandleFunc("POST /api/v1/orgs", func(w http.ResponseWriter, r *http.Request) { msg(w, 422, "user already exists [name: carol]") })
	srv, _ := serve(t, mux)
	err := newProv(t, srv.URL).EnsureOrg(ctx(), "carol")
	if err == nil || !strings.Contains(err.Error(), "existing user") {
		t.Fatalf("err = %v", err)
	}
}

// Right after the first push Gitea may still flag the repo empty and
// refuse a release with 422 "repo is empty".
func TestCreateReleaseWaitsForPushedRepo(t *testing.T) {
	oldWait, oldPoll := emptyWait, emptyPoll
	emptyWait, emptyPoll = 2*time.Second, time.Millisecond
	t.Cleanup(func() { emptyWait, emptyPoll = oldWait, oldPoll })

	gets, posts := 0, 0
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/repos/o/{n}", func(w http.ResponseWriter, r *http.Request) {
		gets++
		j := repoJSON("o", r.PathValue("n"))
		j["empty"] = r.PathValue("n") == "stuck" || gets < 3
		writeJSON(w, 200, j)
	})
	mux.HandleFunc("POST /api/v1/repos/o/{n}/releases", func(w http.ResponseWriter, r *http.Request) {
		posts++
		if r.PathValue("n") == "stuck" || gets < 3 {
			msg(w, 422, "repo is empty")
			return
		}
		writeJSON(w, 201, map[string]any{"id": 7, "tag_name": "v1"})
	})
	srv, _ := serve(t, mux)
	p := newProv(t, srv.URL)
	rel, err := p.CreateRelease(ctx(), "o", "n", forge.Release{Tag: "v1"})
	if err != nil || rel.ID != 7 {
		t.Fatalf("%+v %v", rel, err)
	}
	if posts != 2 || gets != 3 {
		t.Errorf("posts=%d gets=%d, want 2 and 3", posts, gets)
	}

	emptyWait = 50 * time.Millisecond
	posts = 0
	_, err = p.CreateRelease(ctx(), "o", "stuck", forge.Release{Tag: "v1"})
	if err == nil || !strings.Contains(err.Error(), "repo is empty") {
		t.Fatalf("stuck: %v", err)
	}
	if posts != 1 {
		t.Errorf("stuck: %d posts, want 1", posts)
	}
}

// ---- tag-sync race on CreateRelease ----

const uniqueMsg = "constraint failed: UNIQUE constraint failed: release.repo_id, release.tag_name (2067)"

// instantBackoff replaces the retry sleep and records the requested delays.
func instantBackoff(t *testing.T) *[]time.Duration {
	t.Helper()
	old := releaseSleep
	var got []time.Duration
	releaseSleep = func(c context.Context, d time.Duration) error {
		got = append(got, d)
		return c.Err()
	}
	t.Cleanup(func() { releaseSleep = old })
	return &got
}

func TestCreateReleaseRetriesTagSyncRace(t *testing.T) {
	instantBackoff(t)
	posts := 0
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/repos/o/n/releases", func(w http.ResponseWriter, r *http.Request) {
		posts++
		if posts == 1 {
			msg(w, 500, uniqueMsg)
			return
		}
		writeJSON(w, 201, map[string]any{"id": 9, "tag_name": "v1"})
	})
	mux.HandleFunc("GET /api/v1/repos/o/n/releases/tags/v1", func(w http.ResponseWriter, r *http.Request) {
		msg(w, 404, "not found") // tag-only rows are not visible
	})
	srv, _ := serve(t, mux)
	rel, err := newProv(t, srv.URL).CreateRelease(ctx(), "o", "n", forge.Release{Tag: "v1"})
	if err != nil || rel.ID != 9 {
		t.Fatalf("%+v %v", rel, err)
	}
	if posts != 2 {
		t.Errorf("posts = %d, want 2", posts)
	}
}

func TestCreateReleasePersistentRaceIsBounded(t *testing.T) {
	delays := instantBackoff(t)
	posts := 0
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/repos/o/n/releases", func(w http.ResponseWriter, r *http.Request) {
		posts++
		msg(w, 500, strings.ToLower(uniqueMsg))
	})
	mux.HandleFunc("GET /api/v1/repos/o/n/releases/tags/v1", func(w http.ResponseWriter, r *http.Request) {
		msg(w, 404, "not found")
	})
	srv, _ := serve(t, mux)
	_, err := newProv(t, srv.URL).CreateRelease(ctx(), "o", "n", forge.Release{Tag: "v1"})
	if err == nil || !strings.Contains(err.Error(), "tag_name") {
		t.Fatalf("err = %v", err)
	}
	if posts != 5 || len(*delays) != 4 {
		t.Errorf("posts=%d delays=%v, want 5 posts and 4 delays", posts, *delays)
	}
	var total time.Duration
	for _, d := range *delays {
		total += d
	}
	if total > 8*time.Second {
		t.Errorf("total backoff %v > 8s", total)
	}
	if strings.Contains(err.Error(), tok) {
		t.Error("token leaked")
	}
}

func TestCreateReleaseOther500NotRetried(t *testing.T) {
	instantBackoff(t)
	posts := 0
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/repos/o/n/releases", func(w http.ResponseWriter, r *http.Request) {
		posts++
		msg(w, 500, "boom")
	})
	srv, _ := serve(t, mux)
	_, err := newProv(t, srv.URL).CreateRelease(ctx(), "o", "n", forge.Release{Tag: "v1"})
	if err == nil || posts != 1 {
		t.Fatalf("err=%v posts=%d, want error and 1 post", err, posts)
	}
}

func TestCreateReleaseRaceCtxCancelled(t *testing.T) {
	c, cancel := context.WithCancel(context.Background())
	old := releaseSleep
	releaseSleep = func(cc context.Context, d time.Duration) error { cancel(); return cc.Err() }
	t.Cleanup(func() { releaseSleep = old })
	posts := 0
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/repos/o/n/releases", func(w http.ResponseWriter, r *http.Request) {
		posts++
		msg(w, 500, uniqueMsg)
	})
	mux.HandleFunc("GET /api/v1/repos/o/n/releases/tags/v1", func(w http.ResponseWriter, r *http.Request) {
		msg(w, 404, "not found")
	})
	srv, _ := serve(t, mux)
	_, err := newProv(t, srv.URL).CreateRelease(c, "o", "n", forge.Release{Tag: "v1"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if posts != 1 {
		t.Errorf("posts = %d, want 1", posts)
	}
}

func TestCreateReleaseRaceRealReleaseIsPatched(t *testing.T) {
	instantBackoff(t)
	posts := 0
	var patched map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/repos/o/n/releases", func(w http.ResponseWriter, r *http.Request) {
		posts++
		msg(w, 500, uniqueMsg)
	})
	mux.HandleFunc("GET /api/v1/repos/o/n/releases/tags/v1", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"id": 42, "tag_name": "v1"})
	})
	mux.HandleFunc("PATCH /api/v1/repos/o/n/releases/42", func(w http.ResponseWriter, r *http.Request) {
		patched = readBody(t, r)
		writeJSON(w, 200, map[string]any{"id": 42, "tag_name": "v1", "name": "Rel", "body": "b"})
	})
	srv, _ := serve(t, mux)
	rel, err := newProv(t, srv.URL).CreateRelease(ctx(), "o", "n", forge.Release{Tag: "v1", Name: "Rel", Body: "b"})
	if err != nil || rel.ID != 42 || rel.Name != "Rel" {
		t.Fatalf("%+v %v", rel, err)
	}
	if posts != 1 {
		t.Errorf("posts = %d, want 1 (no POST after a real release is seen)", posts)
	}
	if patched["name"] != "Rel" || patched["body"] != "b" {
		t.Errorf("patch body = %v", patched)
	}
}

// ---- org-creation race on EnsureOrg ----

const orgUniqueMsg = "insert organization: constraint failed: UNIQUE constraint failed: user.name (2067)"

func orgMux(t *testing.T, posts *int, post http.HandlerFunc, org http.HandlerFunc) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/user", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]any{"login": "me"}) })
	mux.HandleFunc("GET /api/v1/orgs/{o}", org)
	mux.HandleFunc("POST /api/v1/orgs", func(w http.ResponseWriter, r *http.Request) { *posts++; post(w, r) })
	return mux
}

func TestEnsureOrgRaceThenExists(t *testing.T) {
	instantBackoff(t)
	posts := 0
	mux := orgMux(t, &posts,
		func(w http.ResponseWriter, r *http.Request) { msg(w, 500, orgUniqueMsg) },
		func(w http.ResponseWriter, r *http.Request) {
			if posts > 0 {
				writeJSON(w, 200, map[string]any{})
				return
			}
			msg(w, 404, "nope")
		})
	srv, _ := serve(t, mux)
	if err := newProv(t, srv.URL).EnsureOrg(ctx(), "mirrors"); err != nil {
		t.Fatal(err)
	}
	if posts != 1 {
		t.Errorf("posts = %d, want 1", posts)
	}
}

// Gitea 28 reports the same race on the lower_name column.
func TestEnsureOrgRaceOnLowerNameThenExists(t *testing.T) {
	instantBackoff(t)
	posts := 0
	mux := orgMux(t, &posts,
		func(w http.ResponseWriter, r *http.Request) {
			msg(w, 500, "insert organization: constraint failed: UNIQUE constraint failed: user.lower_name (2067)")
		},
		func(w http.ResponseWriter, r *http.Request) {
			if posts > 0 {
				writeJSON(w, 200, map[string]any{})
				return
			}
			msg(w, 404, "nope")
		})
	srv, _ := serve(t, mux)
	if err := newProv(t, srv.URL).EnsureOrg(ctx(), "mirrors"); err != nil {
		t.Fatal(err)
	}
	if posts != 1 {
		t.Errorf("posts = %d, want 1", posts)
	}
}

func TestEnsureOrgPersistentRaceIsBounded(t *testing.T) {
	delays := instantBackoff(t)
	posts := 0
	mux := orgMux(t, &posts,
		func(w http.ResponseWriter, r *http.Request) { msg(w, 500, strings.ToLower(orgUniqueMsg)) },
		func(w http.ResponseWriter, r *http.Request) { msg(w, 404, "nope") })
	srv, _ := serve(t, mux)
	err := newProv(t, srv.URL).EnsureOrg(ctx(), "mirrors")
	if err == nil || !strings.Contains(err.Error(), "user.name") {
		t.Fatalf("err = %v", err)
	}
	if len(*delays) != 4 || posts != 5 {
		t.Errorf("posts=%d delays=%v, want 5 posts, 4 delays", posts, *delays)
	}
}

func TestEnsureOrgOther500NotRetried(t *testing.T) {
	instantBackoff(t)
	posts := 0
	mux := orgMux(t, &posts,
		func(w http.ResponseWriter, r *http.Request) { msg(w, 500, "boom") },
		func(w http.ResponseWriter, r *http.Request) { msg(w, 404, "nope") })
	srv, _ := serve(t, mux)
	if err := newProv(t, srv.URL).EnsureOrg(ctx(), "mirrors"); err == nil || posts != 1 {
		t.Fatalf("err=%v posts=%d, want error and 1 post", err, posts)
	}
}

func TestEnsureOrgRaceNameIsUser(t *testing.T) {
	instantBackoff(t)
	posts := 0
	mux := orgMux(t, &posts,
		func(w http.ResponseWriter, r *http.Request) {
			if posts == 1 {
				msg(w, 500, orgUniqueMsg)
				return
			}
			msg(w, 422, "user already exists [name: carol]")
		},
		func(w http.ResponseWriter, r *http.Request) { msg(w, 404, "nope") })
	srv, _ := serve(t, mux)
	err := newProv(t, srv.URL).EnsureOrg(ctx(), "carol")
	if err == nil || !strings.Contains(err.Error(), "existing user") {
		t.Fatalf("err = %v", err)
	}
}

func TestEnsureOrgRaceCtxCancelled(t *testing.T) {
	c, cancel := context.WithCancel(context.Background())
	old := releaseSleep
	releaseSleep = func(cc context.Context, d time.Duration) error { cancel(); return cc.Err() }
	t.Cleanup(func() { releaseSleep = old })
	posts := 0
	mux := orgMux(t, &posts,
		func(w http.ResponseWriter, r *http.Request) { msg(w, 500, orgUniqueMsg) },
		func(w http.ResponseWriter, r *http.Request) { msg(w, 404, "nope") })
	srv, _ := serve(t, mux)
	if err := newProv(t, srv.URL).EnsureOrg(c, "mirrors"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}
