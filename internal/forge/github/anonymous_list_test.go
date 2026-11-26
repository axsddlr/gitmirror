package github

import (
	"context"
	"net/http"
	"testing"

	"gitmirror/internal/forge"
)

// Listing a user's repositories must not ask who we are: with no token there
// is no account, and GET /user would answer 401 for a public site.
func TestAnonymousListRepos(t *testing.T) {
	f := newFakeAPI(t)
	f.handle("GET /users/qualk", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"login": "qualk", "type": "User"})
	})
	f.handle("GET /users/qualk/repos", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, []map[string]any{{
			"id": 1, "name": "cool_fox", "full_name": "qualk/cool_fox",
			"owner": map[string]any{"login": "qualk"}, "default_branch": "main",
		}})
	})
	f.handle("GET /user", func(w http.ResponseWriter, r *http.Request) {
		t.Error("a public site must not ask for the authenticated user")
		w.WriteHeader(http.StatusUnauthorized)
	})
	p, err := newWithEndpoints(forge.Options{},
		f.srv.URL+"/api/v3/", f.srv.URL+"/api/uploads/", "https://git.example.com")
	if err != nil {
		t.Fatal(err)
	}
	got, err := p.ListRepos(context.Background(), "qualk")
	if err != nil {
		t.Fatalf("ListRepos: %v", err)
	}
	if len(got) != 1 || got[0].Name != "cool_fox" || got[0].Owner != "qualk" {
		t.Fatalf("ListRepos = %+v", got)
	}
}
