package gitea

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gitmirror/internal/forge"
)

// A site without a token is read anonymously: no Authorization header goes
// out, and Probe proves the address answers as Gitea or Forgejo through the
// public version endpoint.
func TestAnonymousProvider(t *testing.T) {
	var auth, paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = append(auth, r.Header.Get("Authorization"))
		paths = append(paths, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		if _, err := io.WriteString(w, `{"version":"1.27.0"}`); err != nil {
			t.Errorf("write version: %v", err)
		}
	}))
	defer srv.Close()

	p, err := New(forge.Options{URL: srv.URL})
	if err != nil {
		t.Fatalf("New without a token: %v", err)
	}
	if err := p.Probe(context.Background()); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if len(auth) != 1 || auth[0] != "" {
		t.Fatalf("Authorization headers = %q", auth)
	}
	if len(paths) != 1 || paths[0] != "GET /api/v1/version" {
		t.Fatalf("paths = %v", paths)
	}
}

// An address that answers but is not a Gitea server fails the probe.
func TestAnonymousProbeRejectsOtherSites(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.WriteString(w, `{}`); err != nil {
			t.Errorf("write body: %v", err)
		}
	}))
	defer srv.Close()

	p, err := New(forge.Options{URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Probe(context.Background()); err == nil || !strings.Contains(err.Error(), "did not report a version") {
		t.Fatalf("Probe of a non-Gitea site: %v", err)
	}
}
