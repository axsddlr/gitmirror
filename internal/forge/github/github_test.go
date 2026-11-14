package github

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
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gitmirror/internal/forge"
)

const sentinel = "ghp_SENTINELTOKEN1234567890"

var (
	_ forge.Provider        = (*Provider)(nil)
	_ forge.ReleaseProvider = (*Provider)(nil)
	_ forge.WikiProvider    = (*Provider)(nil)
	_ forge.StarProvider    = (*Provider)(nil)
	_ forge.OrgProvider     = (*Provider)(nil)
	_ forge.WebhookProvider = (*Provider)(nil)
)

// fakeAPI is an httptest GitHub that mounts handlers under /api/v3 and
// /api/uploads and records every request in order.
type fakeAPI struct {
	t   *testing.T
	mux *http.ServeMux
	srv *httptest.Server

	mu   sync.Mutex
	log  []string
	auth []string
}

func newFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()
	f := &fakeAPI{t: t, mux: http.NewServeMux()}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.log = append(f.log, r.Method+" "+r.URL.Path)
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		f.mu.Unlock()
		f.mux.ServeHTTP(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// handle registers "METHOD /path" under /api/v3.
func (f *fakeAPI) handle(pattern string, h http.HandlerFunc) {
	m, p, _ := strings.Cut(pattern, " ")
	f.mux.HandleFunc(m+" /api/v3"+p, h)
}

func (f *fakeAPI) handleUpload(pattern string, h http.HandlerFunc) {
	m, p, _ := strings.Cut(pattern, " ")
	f.mux.HandleFunc(m+" /api/uploads"+p, h)
}

func (f *fakeAPI) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.log...)
}

func (f *fakeAPI) provider() *Provider {
	f.t.Helper()
	p, err := newWithEndpoints(forge.Options{Token: sentinel},
		f.srv.URL+"/api/v3/", f.srv.URL+"/api/uploads/", "https://git.example.com")
	if err != nil {
		f.t.Fatal(err)
	}
	return p
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func notFound(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 404, map[string]string{"message": "Not Found"})
}

func decodeBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		t.Errorf("decode body: %v", err)
	}
	return m
}

func (f *fakeAPI) whoami(login string) {
	f.handle("GET /user", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"login": login, "type": "User"})
	})
}

func noToken(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), sentinel) {
		t.Fatalf("error leaks token: %v", err)
	}
}

// ---------------------------------------------------------------- construction

func TestNewDefaultsAndGHE(t *testing.T) {
	for _, u := range []string{"", "https://github.com", "https://github.com/"} {
		p, err := New(forge.Options{URL: u, Token: "t"})
		if err != nil {
			t.Fatal(err)
		}
		if got := p.client.BaseURL(); got != "https://api.github.com/" {
			t.Errorf("url %q: api = %q", u, got)
		}
		if got := p.client.UploadURL(); got != "https://uploads.github.com/" {
			t.Errorf("url %q: upload = %q", u, got)
		}
		if got := p.GitURL("o", "n"); got != "https://github.com/o/n.git" {
			t.Errorf("url %q: git = %q", u, got)
		}
	}
	p, err := New(forge.Options{URL: "https://ghe.example.com/", Token: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if got := p.client.BaseURL(); got != "https://ghe.example.com/api/v3/" {
		t.Errorf("ghe api = %q", got)
	}
	if got := p.client.UploadURL(); got != "https://ghe.example.com/api/uploads/" {
		t.Errorf("ghe upload = %q", got)
	}
	if got := p.GitURL("o", "n"); got != "https://ghe.example.com/o/n.git" {
		t.Errorf("ghe git = %q", got)
	}
	if got := p.WikiGitURL("o", "n"); got != "https://ghe.example.com/o/n.wiki.git" {
		t.Errorf("ghe wiki = %q", got)
	}
}

func TestNewRequiresTokenAndRegisters(t *testing.T) {
	if _, err := New(forge.Options{}); err == nil {
		t.Fatal("expected error for empty token")
	}
	p, err := forge.New("github", forge.Options{Token: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Type() != "github" {
		t.Errorf("Type = %q", p.Type())
	}
}

func TestGitAuthAndURLs(t *testing.T) {
	p, _ := newWithEndpoints(forge.Options{Token: "tok"}, "https://a.example/api/v3/", "https://a.example/api/uploads/", "https://g.example")
	if got := p.GitAuth(); got != (forge.Auth{Username: "x-access-token", Token: "tok"}) {
		t.Errorf("auth = %+v", got)
	}
	p, _ = newWithEndpoints(forge.Options{Token: "tok", Username: "bob"}, "https://a.example/api/v3/", "https://a.example/api/uploads/", "https://g.example")
	if got := p.GitAuth(); got != (forge.Auth{Username: "bob", Token: "tok"}) {
		t.Errorf("auth = %+v", got)
	}
	if got := p.GitURL("o", "n"); got != "https://g.example/o/n.git" {
		t.Errorf("git = %q", got)
	}
	if got := p.WikiGitURL("o", "n"); got != "https://g.example/o/n.wiki.git" {
		t.Errorf("wiki = %q", got)
	}
}

func TestNewMakesNoNetworkCalls(t *testing.T) {
	f := newFakeAPI(t)
	_ = f.provider()
	if n := len(f.requests()); n != 0 {
		t.Fatalf("New made %d requests", n)
	}
}

func TestTokenSentAsBearer(t *testing.T) {
	f := newFakeAPI(t)
	f.whoami("me")
	p := f.provider()
	if _, err := p.login(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := f.auth[0]; !strings.Contains(got, sentinel) {
		t.Errorf("Authorization = %q", got)
	}
}

// ---------------------------------------------------------------- who am i

func TestLoginCachedOnceButNotFailures(t *testing.T) {
	f := newFakeAPI(t)
	var hits atomic.Int32
	f.handle("GET /user", func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			writeJSON(w, 500, map[string]string{"message": "boom"})
			return
		}
		writeJSON(w, 200, map[string]string{"login": "me"})
	})
	p := f.provider()
	ctx := context.Background()
	if _, err := p.login(ctx); err == nil {
		t.Fatal("first call should fail")
	}
	for i := 0; i < 3; i++ {
		got, err := p.login(ctx)
		if err != nil || got != "me" {
			t.Fatalf("login = %q, %v", got, err)
		}
	}
	if hits.Load() != 2 {
		t.Fatalf("GET /user hits = %d, want 2 (1 failure + 1 success cached)", hits.Load())
	}
}

func TestLoginDoesNotHoldLockDuringRequest(t *testing.T) {
	f := newFakeAPI(t)
	started := make(chan struct{})
	release := make(chan struct{})
	var relOnce sync.Once
	defer relOnce.Do(func() { close(release) })
	var once sync.Once
	f.handle("GET /user", func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(started) })
		select {
		case <-release:
		case <-r.Context().Done():
		}
		writeJSON(w, 200, map[string]string{"login": "me"})
	})
	p := f.provider()
	first := make(chan error, 1)
	go func() {
		_, err := p.login(context.Background())
		first <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("first login never reached the server")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	second := make(chan error, 1)
	go func() {
		_, err := p.login(ctx)
		second <- err
	}()
	select {
	case err := <-second:
		if err == nil {
			t.Fatal("cancelled login should fail")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("second login blocked behind the first caller's request")
	}
	relOnce.Do(func() { close(release) })
	if err := <-first; err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------- GetRepo

func TestGetRepoMapping(t *testing.T) {
	f := newFakeAPI(t)
	f.handle("GET /repos/o/n", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{
			"id": 101, "name": "n", "owner": map[string]any{"login": "o"},
			"description": "d", "homepage": "http://h", "topics": []string{"a", "b"},
			"default_branch": "trunk", "private": false, "visibility": "internal",
			"archived": true, "fork": true, "has_wiki": true, "size": 0,
		})
	})
	f.handle("GET /repos/o/pub", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{
			"name": "pub", "owner": map[string]any{"login": "o"},
			"private": false, "visibility": "public", "size": 12, "default_branch": "main",
		})
	})
	f.handle("GET /repos/o/priv", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{
			"name": "priv", "owner": map[string]any{"login": "o"}, "private": true, "size": 3,
		})
	})
	f.handle("GET /repos/o/missing", notFound)
	p := f.provider()
	ctx := context.Background()

	got, err := p.GetRepo(ctx, "o", "n")
	if err != nil {
		t.Fatal(err)
	}
	want := forge.Repo{ID: 101, Owner: "o", Name: "n", Description: "d", Homepage: "http://h",
		Topics: []string{"a", "b"}, DefaultBranch: "trunk", Private: true, Archived: true,
		Fork: true, Empty: true, HasWiki: true}
	if fmt.Sprint(*got) != fmt.Sprint(want) {
		t.Errorf("got  %+v\nwant %+v", *got, want)
	}
	pub, err := p.GetRepo(ctx, "o", "pub")
	if err != nil || pub.Private || pub.Empty {
		t.Errorf("pub = %+v, %v", pub, err)
	}
	priv, err := p.GetRepo(ctx, "o", "priv")
	if err != nil || !priv.Private {
		t.Errorf("priv = %+v, %v", priv, err)
	}
	_, err = p.GetRepo(ctx, "o", "missing")
	if !errors.Is(err, forge.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
	if !strings.Contains(err.Error(), "github: get repo o/missing") {
		t.Errorf("missing context: %v", err)
	}
}

// ---------------------------------------------------------------- ListRepos

// pagedRepos serves 3 pages of repos named prefix1..prefix5 via Link headers.
func (f *fakeAPI) pagedRepos(pattern, prefix string, wantQuery map[string]string) {
	pages := [][]string{{prefix + "1", prefix + "2"}, {prefix + "3", prefix + "4"}, {prefix + "5"}}
	f.handle(pattern, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		for k, v := range wantQuery {
			if q.Get(k) != v {
				f.t.Errorf("%s: query %s = %q, want %q", pattern, k, q.Get(k), v)
			}
		}
		if q.Get("per_page") != "100" {
			f.t.Errorf("per_page = %q", q.Get("per_page"))
		}
		page := 1
		_, _ = fmt.Sscan(q.Get("page"), &page)
		if page < len(pages) {
			next := *r.URL
			nq := next.Query()
			nq.Set("page", fmt.Sprint(page+1))
			next.RawQuery = nq.Encode()
			w.Header().Set("Link", fmt.Sprintf(`<%s%s>; rel="next"`, f.srv.URL, next.RequestURI()))
		}
		var out []map[string]any
		for _, n := range pages[page-1] {
			out = append(out, map[string]any{"name": n, "owner": map[string]any{"login": "o"}, "size": 1})
		}
		writeJSON(w, 200, out)
	})
}

func names(rs []forge.Repo) string {
	var s []string
	for _, r := range rs {
		s = append(s, r.Name)
	}
	return strings.Join(s, ",")
}

func TestListReposSelfIncludesPrivate(t *testing.T) {
	f := newFakeAPI(t)
	f.whoami("Me")
	f.pagedRepos("GET /user/repos", "s", map[string]string{"affiliation": "owner", "visibility": "all"})
	rs, err := f.provider().ListRepos(context.Background(), "me")
	if err != nil {
		t.Fatal(err)
	}
	if got := names(rs); got != "s1,s2,s3,s4,s5" {
		t.Errorf("repos = %s", got)
	}
}

func TestListReposOrgAndUser(t *testing.T) {
	f := newFakeAPI(t)
	f.whoami("me")
	f.handle("GET /users/acme", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"login": "acme", "type": "Organization"})
	})
	f.handle("GET /users/bob", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"login": "bob", "type": "User"})
	})
	f.handle("GET /users/ghost", notFound)
	f.pagedRepos("GET /orgs/acme/repos", "org", map[string]string{"type": "all"})
	f.pagedRepos("GET /users/bob/repos", "usr", map[string]string{"type": "owner"})
	p := f.provider()
	ctx := context.Background()

	rs, err := p.ListRepos(ctx, "acme")
	if err != nil || names(rs) != "org1,org2,org3,org4,org5" {
		t.Errorf("org: %s, %v", names(rs), err)
	}
	rs, err = p.ListRepos(ctx, "bob")
	if err != nil || names(rs) != "usr1,usr2,usr3,usr4,usr5" {
		t.Errorf("user: %s, %v", names(rs), err)
	}
	if _, err = p.ListRepos(ctx, "ghost"); !errors.Is(err, forge.ErrNotFound) {
		t.Errorf("ghost: %v", err)
	}
}

// ---------------------------------------------------------------- CreateRepo

func TestCreateRepoUserAndOrg(t *testing.T) {
	f := newFakeAPI(t)
	f.whoami("me")
	var userBody, orgBody map[string]any
	f.handle("POST /user/repos", func(w http.ResponseWriter, r *http.Request) {
		userBody = decodeBody(t, r)
		writeJSON(w, 201, map[string]any{"id": 7, "name": "n", "owner": map[string]any{"login": "me"}, "private": true, "has_wiki": true})
	})
	f.handle("POST /orgs/acme/repos", func(w http.ResponseWriter, r *http.Request) {
		orgBody = decodeBody(t, r)
		if orgBody["name"] == "dup" {
			writeJSON(w, 422, map[string]any{"message": "Repository creation failed.",
				"errors": []map[string]string{{"resource": "Repository", "code": "custom", "field": "name", "message": "name already exists on this account"}}})
			return
		}
		writeJSON(w, 201, map[string]any{"name": "n", "owner": map[string]any{"login": "acme"}})
	})
	f.handle("POST /orgs/nope/repos", notFound)
	p := f.provider()
	ctx := context.Background()

	r, err := p.CreateRepo(ctx, forge.CreateOptions{Owner: "ME", Name: "n", Description: "desc", Private: true})
	if err != nil {
		t.Fatal(err)
	}
	if r.Owner != "me" || r.Name != "n" || !r.Private || r.ID != 7 {
		t.Errorf("repo = %+v", r)
	}
	check := func(b map[string]any) {
		t.Helper()
		if b["auto_init"] != false || b["has_issues"] != false || b["has_projects"] != false || b["has_wiki"] != true {
			t.Errorf("body flags wrong: %v", b)
		}
	}
	check(userBody)
	if userBody["private"] != true || userBody["description"] != "desc" || userBody["name"] != "n" {
		t.Errorf("user body = %v", userBody)
	}
	if _, err = p.CreateRepo(ctx, forge.CreateOptions{Owner: "acme", Name: "n"}); err != nil {
		t.Fatal(err)
	}
	check(orgBody)
	if orgBody["private"] != false {
		t.Errorf("org private = %v", orgBody["private"])
	}
	_, err = p.CreateRepo(ctx, forge.CreateOptions{Owner: "acme", Name: "dup"})
	if err == nil || !strings.Contains(err.Error(), "already exists") || errors.Is(err, forge.ErrNotFound) {
		t.Errorf("dup err = %v", err)
	}
	if _, err = p.CreateRepo(ctx, forge.CreateOptions{Owner: "nope", Name: "n"}); !errors.Is(err, forge.ErrNotFound) {
		t.Errorf("missing org err = %v", err)
	}
}

// ---------------------------------------------------------------- UpdateRepoSettings

func (f *fakeAPI) settingsHandlers(bodies *[]string) {
	f.handle("PATCH /repos/o/n", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*bodies = append(*bodies, "PATCH "+string(b))
		writeJSON(w, 200, map[string]any{"name": "n"})
	})
	f.handle("PUT /repos/o/n/topics", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*bodies = append(*bodies, "PUT "+string(b))
		writeJSON(w, 200, map[string]any{"names": []string{}})
	})
}

func TestUpdateSettingsUnarchiveFirst(t *testing.T) {
	f := newFakeAPI(t)
	var bodies []string
	f.settingsHandlers(&bodies)
	err := f.provider().UpdateRepoSettings(context.Background(), "o", "n", forge.Settings{
		Archived: ptr(false), Description: ptr("d"), Homepage: ptr("h"), DefaultBranch: ptr("main"),
		Private: ptr(true), Topics: ptr([]string{"Go", "CLI"}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.requests(), "|"); got != "PATCH /api/v3/repos/o/n|PATCH /api/v3/repos/o/n|PUT /api/v3/repos/o/n/topics" {
		t.Fatalf("order = %s", got)
	}
	var first, second map[string]any
	if err := json.Unmarshal([]byte(strings.TrimPrefix(bodies[0], "PATCH ")), &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(bodies[1], "PATCH ")), &second); err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || first["archived"] != false {
		t.Errorf("first patch = %v", first)
	}
	if _, has := second["archived"]; has || second["description"] != "d" || second["homepage"] != "h" ||
		second["default_branch"] != "main" || second["private"] != true {
		t.Errorf("second patch = %v", second)
	}
	var topics map[string][]string
	if err := json.Unmarshal([]byte(strings.TrimPrefix(bodies[2], "PUT ")), &topics); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(topics["names"]) != "[go cli]" {
		t.Errorf("topics = %v", topics)
	}
}

func TestUpdateSettingsArchiveLast(t *testing.T) {
	f := newFakeAPI(t)
	var bodies []string
	f.settingsHandlers(&bodies)
	err := f.provider().UpdateRepoSettings(context.Background(), "o", "n", forge.Settings{
		Archived: ptr(true), Description: ptr("d"), Topics: ptr([]string{"x"}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.requests(), "|"); got != "PATCH /api/v3/repos/o/n|PUT /api/v3/repos/o/n/topics|PATCH /api/v3/repos/o/n" {
		t.Fatalf("order = %s", got)
	}
	var last map[string]any
	if err := json.Unmarshal([]byte(strings.TrimPrefix(bodies[2], "PATCH ")), &last); err != nil {
		t.Fatal(err)
	}
	if len(last) != 1 || last["archived"] != true {
		t.Errorf("last patch = %v", last)
	}
	if strings.Contains(bodies[0], "archived") {
		t.Errorf("first patch contains archived: %s", bodies[0])
	}
}

func TestUpdateSettingsSkipsEmptyCalls(t *testing.T) {
	f := newFakeAPI(t)
	var bodies []string
	f.settingsHandlers(&bodies)
	p := f.provider()
	ctx := context.Background()
	if err := p.UpdateRepoSettings(ctx, "o", "n", forge.Settings{}); err != nil {
		t.Fatal(err)
	}
	if n := len(f.requests()); n != 0 {
		t.Fatalf("empty settings made %d requests", n)
	}
	if err := p.UpdateRepoSettings(ctx, "o", "n", forge.Settings{Archived: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.requests(), "|"); got != "PATCH /api/v3/repos/o/n" {
		t.Fatalf("archive only = %s", got)
	}
	if err := p.UpdateRepoSettings(ctx, "o", "n", forge.Settings{Topics: ptr([]string{})}); err != nil {
		t.Fatal(err)
	}
	if got := f.requests()[1]; got != "PUT /api/v3/repos/o/n/topics" {
		t.Fatalf("topics only = %s", got)
	}
	if !strings.Contains(bodies[len(bodies)-1], `"names":[]`) {
		t.Errorf("empty topics body = %s", bodies[len(bodies)-1])
	}
}

func TestUpdateSettingsNotFound(t *testing.T) {
	f := newFakeAPI(t)
	f.handle("PATCH /repos/o/n", notFound)
	err := f.provider().UpdateRepoSettings(context.Background(), "o", "n", forge.Settings{Description: ptr("x")})
	if !errors.Is(err, forge.ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}

// ---------------------------------------------------------------- RateLimit

func TestRateLimit(t *testing.T) {
	f := newFakeAPI(t)
	reset := time.Now().Add(time.Hour).Truncate(time.Second)
	f.handle("GET /rate_limit", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"resources": map[string]any{
			"core":   map[string]any{"limit": 5000, "remaining": 4321, "reset": reset.Unix()},
			"search": map[string]any{"limit": 30, "remaining": 1, "reset": 1},
		}})
	})
	got, err := f.provider().RateLimit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.Limit != 5000 || got.Remaining != 4321 || !got.Reset.Equal(reset) {
		t.Errorf("rate = %+v", got)
	}
}

func TestRateLimitErrorsMentionReset(t *testing.T) {
	f := newFakeAPI(t)
	reset := time.Now().Add(time.Hour).Truncate(time.Second)
	f.handle("GET /repos/o/primary", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Limit", "60")
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", fmt.Sprint(reset.Unix()))
		writeJSON(w, 403, map[string]string{"message": "API rate limit exceeded"})
	})
	f.handle("GET /repos/o/secondary", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "42")
		writeJSON(w, 403, map[string]string{
			"message":           "You have exceeded a secondary rate limit",
			"documentation_url": "https://docs.github.com/rest/overview/rate-limits-for-the-rest-api#about-secondary-rate-limits",
		})
	})
	p := f.provider()
	_, err := p.GetRepo(context.Background(), "o", "primary")
	if err == nil || !strings.Contains(err.Error(), reset.UTC().Format(time.RFC3339)) {
		t.Errorf("primary err = %v (want reset %s)", err, reset.UTC().Format(time.RFC3339))
	}
	_, err = p.GetRepo(context.Background(), "o", "secondary")
	if err == nil || !strings.Contains(err.Error(), "42s") {
		t.Errorf("secondary err = %v (want retry after 42s)", err)
	}
}

// ---------------------------------------------------------------- releases

func TestListReleasesPaginatedWithAssets(t *testing.T) {
	f := newFakeAPI(t)
	f.handle("GET /repos/o/n/releases", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("per_page") != "100" {
			t.Errorf("per_page = %q", q.Get("per_page"))
		}
		page := 1
		_, _ = fmt.Sscan(q.Get("page"), &page)
		if page < 3 {
			w.Header().Set("Link", fmt.Sprintf(`<%s/api/v3/repos/o/n/releases?per_page=100&page=%d>; rel="next"`, f.srv.URL, page+1))
		}
		rel := map[string]any{"id": page, "tag_name": fmt.Sprintf("v%d", page), "name": "N", "body": "B",
			"target_commitish": "main", "draft": page == 2, "prerelease": page == 3}
		if page == 1 {
			rel["assets"] = []map[string]any{{"id": 77, "name": "a.zip", "size": 1234, "content_type": "application/zip"}}
		}
		writeJSON(w, 200, []any{rel})
	})
	rs, err := f.provider().ListReleases(context.Background(), "o", "n")
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 3 || rs[0].Tag != "v1" || rs[2].Tag != "v3" || !rs[1].Draft || !rs[2].Prerelease {
		t.Fatalf("releases = %+v", rs)
	}
	if rs[0].Name != "N" || rs[0].Body != "B" || rs[0].Target != "main" || rs[0].ID != 1 {
		t.Errorf("rel0 = %+v", rs[0])
	}
	want := forge.Asset{ID: 77, Name: "a.zip", Size: 1234, ContentType: "application/zip"}
	if len(rs[0].Assets) != 1 || rs[0].Assets[0] != want {
		t.Errorf("assets = %+v", rs[0].Assets)
	}
}

func TestCreateUpdateDeleteRelease(t *testing.T) {
	f := newFakeAPI(t)
	var created, updated map[string]any
	f.handle("POST /repos/o/n/releases", func(w http.ResponseWriter, r *http.Request) {
		created = decodeBody(t, r)
		writeJSON(w, 201, map[string]any{"id": 9, "tag_name": "v1", "name": "Name", "body": "Body", "target_commitish": "abc", "draft": true})
	})
	f.handle("PATCH /repos/o/n/releases/9", func(w http.ResponseWriter, r *http.Request) {
		updated = decodeBody(t, r)
		writeJSON(w, 200, map[string]any{"id": 9, "tag_name": "v1", "name": "Name2", "prerelease": true})
	})
	f.handle("DELETE /repos/o/n/releases/9", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	f.handle("DELETE /repos/o/n/releases/10", notFound)
	f.handle("PATCH /repos/o/n/releases/10", notFound)
	p := f.provider()
	ctx := context.Background()

	rel := forge.Release{Tag: "v1", Name: "Name", Body: "Body", Target: "abc", Draft: true}
	got, err := p.CreateRelease(ctx, "o", "n", rel)
	if err != nil || got.ID != 9 || got.Tag != "v1" {
		t.Fatalf("create = %+v, %v", got, err)
	}
	if created["tag_name"] != "v1" || created["name"] != "Name" || created["body"] != "Body" ||
		created["target_commitish"] != "abc" || created["draft"] != true || created["prerelease"] != false {
		t.Errorf("create body = %v", created)
	}
	rel.ID, rel.Name, rel.Draft, rel.Prerelease = 9, "Name2", false, true
	got, err = p.UpdateRelease(ctx, "o", "n", rel)
	if err != nil || got.Name != "Name2" || !got.Prerelease {
		t.Fatalf("update = %+v, %v", got, err)
	}
	if updated["tag_name"] != "v1" || updated["name"] != "Name2" || updated["body"] != "Body" ||
		updated["target_commitish"] != "abc" || updated["draft"] != false || updated["prerelease"] != true {
		t.Errorf("update body = %v", updated)
	}
	if err := p.DeleteRelease(ctx, "o", "n", 9); err != nil {
		t.Fatal(err)
	}
	if err := p.DeleteRelease(ctx, "o", "n", 10); !errors.Is(err, forge.ErrNotFound) {
		t.Errorf("delete missing = %v", err)
	}
	rel.ID = 10
	if _, err := p.UpdateRelease(ctx, "o", "n", rel); !errors.Is(err, forge.ErrNotFound) {
		t.Errorf("update missing = %v", err)
	}
}

func TestDownloadAssetRedirectDropsAuthorization(t *testing.T) {
	var cdnAuth, cdnHits atomic.Value
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cdnAuth.Store(r.Header.Get("Authorization"))
		cdnHits.Store(r.URL.Path)
		_, _ = w.Write([]byte("PAYLOAD"))
	}))
	defer cdn.Close()

	f := newFakeAPI(t)
	var apiAccept string
	f.handle("GET /repos/o/n/releases/assets/5", func(w http.ResponseWriter, r *http.Request) {
		apiAccept = r.Header.Get("Accept")
		http.Redirect(w, r, cdn.URL+"/blob/xyz?sig=1", http.StatusFound)
	})
	f.handle("GET /repos/o/n/releases/assets/6", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("DIRECT"))
	})
	f.handle("GET /repos/o/n/releases/assets/7", notFound)
	p := f.provider()
	ctx := context.Background()

	rc, err := p.DownloadAsset(ctx, "o", "n", forge.Asset{ID: 5})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "PAYLOAD" {
		t.Errorf("body = %q", b)
	}
	if apiAccept != "application/octet-stream" {
		t.Errorf("Accept = %q", apiAccept)
	}
	if !strings.Contains(f.auth[0], sentinel) {
		t.Errorf("API call lacked token: %q", f.auth[0])
	}
	if a, _ := cdnAuth.Load().(string); a != "" {
		t.Errorf("redirect target received Authorization %q", a)
	}
	if cdnHits.Load() != "/blob/xyz" {
		t.Errorf("cdn path = %v", cdnHits.Load())
	}

	rc, err = p.DownloadAsset(ctx, "o", "n", forge.Asset{ID: 6})
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(rc)
	rc.Close()
	if string(b) != "DIRECT" {
		t.Errorf("direct body = %q", b)
	}
	if _, err = p.DownloadAsset(ctx, "o", "n", forge.Asset{ID: 7}); !errors.Is(err, forge.ErrNotFound) {
		t.Errorf("missing asset = %v", err)
	}
}

func TestUploadAssetStreams(t *testing.T) {
	f := newFakeAPI(t)
	var gotBody, gotCT, gotName, gotAuth string
	var gotLen int64
	f.handleUpload("POST /repos/o/n/releases/5/assets", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody, gotCT, gotName, gotAuth = string(b), r.Header.Get("Content-Type"), r.URL.Query().Get("name"), r.Header.Get("Authorization")
		gotLen = r.ContentLength
		writeJSON(w, 201, map[string]any{"id": 88, "name": gotName, "size": len(b), "content_type": gotCT})
	})
	f.handleUpload("POST /repos/o/n/releases/6/assets", notFound)
	p := f.provider()
	ctx := context.Background()

	// MultiReader hides any Len/Seek so the body must be streamed with the declared size.
	body := io.MultiReader(strings.NewReader("hello "), strings.NewReader("world"))
	a, err := p.UploadAsset(ctx, "o", "n", 5, forge.Asset{Name: "my file.bin", Size: 11}, body)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != 88 || a.Name != "my file.bin" || a.Size != 11 {
		t.Errorf("asset = %+v", a)
	}
	if gotBody != "hello world" || gotCT != "application/octet-stream" || gotName != "my file.bin" || gotLen != 11 {
		t.Errorf("body=%q ct=%q name=%q len=%d", gotBody, gotCT, gotName, gotLen)
	}
	if !strings.Contains(gotAuth, sentinel) {
		t.Errorf("upload Authorization = %q", gotAuth)
	}
	_, err = p.UploadAsset(ctx, "o", "n", 5, forge.Asset{Name: "z", Size: 3, ContentType: "application/zip"}, strings.NewReader("abc"))
	if err != nil || gotCT != "application/zip" {
		t.Errorf("ct = %q, err %v", gotCT, err)
	}
	_, err = p.UploadAsset(ctx, "o", "n", 6, forge.Asset{Name: "z", Size: 3}, strings.NewReader("abc"))
	if !errors.Is(err, forge.ErrNotFound) {
		t.Errorf("missing release = %v", err)
	}
}

func TestDeleteAsset(t *testing.T) {
	f := newFakeAPI(t)
	f.handle("DELETE /repos/o/n/releases/assets/3", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	f.handle("DELETE /repos/o/n/releases/assets/4", notFound)
	p := f.provider()
	if err := p.DeleteAsset(context.Background(), "o", "n", 1, 3); err != nil {
		t.Fatal(err)
	}
	if err := p.DeleteAsset(context.Background(), "o", "n", 1, 4); !errors.Is(err, forge.ErrNotFound) {
		t.Errorf("err = %v", err)
	}
}

// ---------------------------------------------------------------- stars

func TestListStarred(t *testing.T) {
	f := newFakeAPI(t)
	f.handle("GET /users/bob/starred", func(w http.ResponseWriter, r *http.Request) {
		page := 1
		_, _ = fmt.Sscan(r.URL.Query().Get("page"), &page)
		if r.URL.Query().Get("per_page") != "100" {
			t.Errorf("per_page = %q", r.URL.Query().Get("per_page"))
		}
		if page < 3 {
			w.Header().Set("Link", fmt.Sprintf(`<%s/api/v3/users/bob/starred?per_page=100&page=%d>; rel="next"`, f.srv.URL, page+1))
		}
		// The star+json media type wraps each repo as {starred_at, repo}.
		writeJSON(w, 200, []map[string]any{{"starred_at": "2024-01-01T00:00:00Z", "repo": map[string]any{
			"name": fmt.Sprintf("r%d", page), "owner": map[string]any{"login": "x"}, "private": false}}})
	})
	f.handle("GET /users/ghost/starred", notFound)
	p := f.provider()
	rs, err := p.ListStarred(context.Background(), "bob")
	if err != nil || names(rs) != "r1,r2,r3" || rs[0].Owner != "x" {
		t.Errorf("stars = %+v, %v", rs, err)
	}
	if _, err = p.ListStarred(context.Background(), "ghost"); !errors.Is(err, forge.ErrNotFound) {
		t.Errorf("ghost = %v", err)
	}
}

// ---------------------------------------------------------------- orgs

func TestEnsureOrg(t *testing.T) {
	f := newFakeAPI(t)
	f.whoami("me")
	f.handle("GET /orgs/acme", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"login": "acme"})
	})
	f.handle("GET /orgs/me", notFound)
	f.handle("GET /orgs/newco", notFound)
	p := f.provider()
	ctx := context.Background()
	if err := p.EnsureOrg(ctx, "acme"); err != nil {
		t.Errorf("existing org: %v", err)
	}
	if err := p.EnsureOrg(ctx, "ME"); err != nil {
		t.Errorf("own namespace: %v", err)
	}
	err := p.EnsureOrg(ctx, "newco")
	if !errors.Is(err, forge.ErrUnsupported) {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "GitHub UI") || !strings.Contains(err.Error(), "newco") {
		t.Errorf("message = %v", err)
	}
}

// ---------------------------------------------------------------- errors / token

func TestErrorsNeverLeakToken(t *testing.T) {
	f := newFakeAPI(t)
	// The 401 body echoes the request headers, as some proxies do.
	echo := func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 401, map[string]string{
			"message": "Bad credentials; saw Authorization: " + r.Header.Get("Authorization"),
		})
	}
	f.handle("GET /repos/o/n", echo)
	f.handle("GET /user", echo)
	f.handle("GET /rate_limit", echo)
	f.handle("GET /users/bob/starred", echo)
	f.handle("GET /repos/o/n/releases", echo)
	f.handle("GET /repos/o/n/releases/assets/1", echo)
	f.handle("PATCH /repos/o/n", echo)
	f.handle("POST /orgs/acme/repos", echo)
	f.handle("GET /users/acme", echo)
	f.handle("GET /orgs/acme", echo)
	f.handleUpload("POST /repos/o/n/releases/1/assets", echo)
	p := f.provider()
	ctx := context.Background()

	_, err := p.GetRepo(ctx, "o", "n")
	noToken(t, err)
	if !strings.Contains(err.Error(), "Bad credentials") {
		t.Errorf("lost useful message: %v", err)
	}
	_, err = p.RateLimit(ctx)
	noToken(t, err)
	_, err = p.ListStarred(ctx, "bob")
	noToken(t, err)
	_, err = p.ListReleases(ctx, "o", "n")
	noToken(t, err)
	_, err = p.DownloadAsset(ctx, "o", "n", forge.Asset{ID: 1})
	noToken(t, err)
	noToken(t, p.UpdateRepoSettings(ctx, "o", "n", forge.Settings{Description: ptr("x")}))
	_, err = p.CreateRepo(ctx, forge.CreateOptions{Owner: "acme", Name: "n"})
	noToken(t, err)
	_, err = p.ListRepos(ctx, "acme")
	noToken(t, err)
	noToken(t, p.EnsureOrg(ctx, "acme"))
	_, err = p.UploadAsset(ctx, "o", "n", 1, forge.Asset{Name: "a", Size: 1}, strings.NewReader("x"))
	noToken(t, err)
}

// ---------------------------------------------------------------- webhooks

func sign(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

func hook(event, ctype, sig string, body []byte) *http.Request {
	r := httptest.NewRequest("POST", "/hook", strings.NewReader(string(body)))
	r.Header.Set("X-GitHub-Event", event)
	r.Header.Set("Content-Type", ctype)
	if sig != "" {
		r.Header.Set("X-Hub-Signature-256", sig)
	}
	return r
}

func TestParseWebhookKinds(t *testing.T) {
	p, _ := New(forge.Options{Token: "t"})
	payload := []byte(`{"action":"x","repository":{"name":"repo","owner":{"login":"alice"}}}`)
	cases := map[string]forge.EventKind{
		"push": forge.EventPush, "create": forge.EventPush, "delete": forge.EventPush,
		"release": forge.EventRelease, "repository": forge.EventRepo, "gollum": forge.EventWiki,
		"issues": forge.EventOther, "ping": forge.EventOther,
	}
	for ev, want := range cases {
		e, err := p.ParseWebhook(hook(ev, "application/json", sign("s3", payload), payload), "s3")
		if err != nil {
			t.Fatalf("%s: %v", ev, err)
		}
		if e.Kind != want || e.Owner != "alice" || e.Name != "repo" {
			t.Errorf("%s: %+v", ev, e)
		}
	}
}

func TestParseWebhookPingWithoutRepository(t *testing.T) {
	p, _ := New(forge.Options{Token: "t"})
	body := []byte(`{"zen":"Keep it logically awesome."}`)
	e, err := p.ParseWebhook(hook("ping", "application/json", sign("s", body), body), "s")
	if err != nil || e.Kind != forge.EventOther || e.Owner != "" || e.Name != "" {
		t.Fatalf("e = %+v, err = %v", e, err)
	}
}

func TestParseWebhookForm(t *testing.T) {
	p, _ := New(forge.Options{Token: "t"})
	json := `{"repository":{"name":"r","owner":{"login":"bob"}}}`
	body := []byte("payload=" + url.QueryEscape(json))
	e, err := p.ParseWebhook(hook("push", "application/x-www-form-urlencoded", sign("s", body), body), "s")
	if err != nil || e.Kind != forge.EventPush || e.Owner != "bob" || e.Name != "r" {
		t.Fatalf("e = %+v, err = %v", e, err)
	}
}

func TestParseWebhookRejections(t *testing.T) {
	p, _ := New(forge.Options{Token: "t"})
	body := []byte(`{"repository":{"name":"r","owner":{"login":"o"}}}`)

	if _, err := p.ParseWebhook(hook("push", "application/json", sign("", body), body), ""); err == nil {
		t.Error("empty secret accepted")
	}
	bad := map[string]string{
		"missing":     "",
		"wrong":       sign("other", body),
		"no prefix":   strings.TrimPrefix(sign("s", body), "sha256="),
		"not hex":     "sha256=zzzz",
		"sha1 header": "sha1=" + hex.EncodeToString([]byte("abc")),
	}
	for name, sig := range bad {
		_, err := p.ParseWebhook(hook("push", "application/json", sig, body), "s")
		if !errors.Is(err, forge.ErrBadSignature) {
			t.Errorf("%s: err = %v, want ErrBadSignature", name, err)
		}
	}
	// Tampered body.
	_, err := p.ParseWebhook(hook("push", "application/json", sign("s", body), append([]byte(" "), body...)), "s")
	if !errors.Is(err, forge.ErrBadSignature) {
		t.Errorf("tampered: %v", err)
	}
}

func TestParseWebhookBodyCap(t *testing.T) {
	p, _ := New(forge.Options{Token: "t"})
	big := []byte(strings.Repeat("a", 25<<20+1))
	_, err := p.ParseWebhook(hook("push", "application/json", sign("s", big), big), "s")
	if err == nil || errors.Is(err, forge.ErrBadSignature) {
		t.Fatalf("err = %v, want size error", err)
	}
}

func TestDeleteRepo(t *testing.T) {
	f := newFakeAPI(t)
	f.handle("DELETE /repos/o/n", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	f.handle("DELETE /repos/o/gone", notFound)
	f.handle("DELETE /repos/o/denied", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 403, map[string]string{"message": "Must have admin rights to Repository."})
	})
	var _ forge.RepoDeleter = f.provider()
	p := f.provider()
	if err := p.DeleteRepo(context.Background(), "o", "n"); err != nil {
		t.Fatal(err)
	}
	if got := f.requests(); len(got) != 1 || got[0] != "DELETE /api/v3/repos/o/n" {
		t.Errorf("requests = %v", got)
	}
	err := p.DeleteRepo(context.Background(), "o", "gone")
	if !errors.Is(err, forge.ErrNotFound) {
		t.Errorf("err = %v", err)
	}
	noToken(t, err)
	err = p.DeleteRepo(context.Background(), "o", "denied")
	noToken(t, err)
	for _, want := range []string{"delete_repo", "Administration: write"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("403 error %q lacks %q", err, want)
		}
	}
}
