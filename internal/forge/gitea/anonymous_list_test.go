package gitea

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"gitmirror/internal/forge"
)

// Listing a user's repositories must not ask who we are: with no token there
// is no account, and GET /user would answer 401 for a public site.
func TestAnonymousListRepos(t *testing.T) {
	var paths []string
	fail := func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("%s %s: a public site must not need an account", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusUnauthorized)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/orgs/qualk":
			w.WriteHeader(http.StatusNotFound)
			if _, err := w.Write([]byte(`{"message":"not found"}`)); err != nil {
				t.Errorf("write 404: %v", err)
			}
		case "/api/v1/users/qualk/repos":
			// One page only; the helper stops on an empty page.
			body := `[]`
			if r.URL.Query().Get("page") == "1" {
				body = `[{"id":1,"name":"cool_fox","owner":{"login":"qualk"},` +
					`"default_branch":"main","has_wiki":false}]`
			}
			if _, err := w.Write([]byte(body)); err != nil {
				t.Errorf("write repos: %v", err)
			}
		default:
			fail(w, r)
		}
	}))
	defer srv.Close()

	p, err := New(forge.Options{URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	got, err := p.ListRepos(context.Background(), "qualk")
	if err != nil {
		t.Fatalf("ListRepos: %v", err)
	}
	if len(got) != 1 || got[0].Name != "cool_fox" || got[0].Owner != "qualk" {
		t.Fatalf("ListRepos returned %d repos", len(got))
	}
	for _, path := range paths {
		if path == "/api/v1/user" || path == "/api/v1/user/repos" {
			t.Fatalf("asked for the signed-in account: %v", paths)
		}
	}
}

// A token that the site refuses is still an error: the fallback is for sites
// without a token, not for a broken one.
func TestListReposTokenFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		if _, err := w.Write([]byte(`{"message":"bad credentials"}`)); err != nil {
			t.Errorf("write body: %v", err)
		}
	}))
	defer srv.Close()

	p, err := New(forge.Options{URL: srv.URL, Token: "revoked"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.ListRepos(context.Background(), "qualk")
	var apiErr *apiError
	if !errors.As(err, &apiErr) || apiErr.status != http.StatusUnauthorized {
		t.Fatalf("err = %v, want the 401", err)
	}
}
