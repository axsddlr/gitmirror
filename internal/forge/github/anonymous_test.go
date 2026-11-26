package github

import (
	"context"
	"net/http"
	"testing"

	"gitmirror/internal/forge"
)

// A site without a token reads anonymously: the client carries no
// credentials, and Probe checks the public rate-limit endpoint.
func TestAnonymousProvider(t *testing.T) {
	f := newFakeAPI(t)
	f.handle("GET /rate_limit", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"resources": map[string]any{}})
	})
	p, err := newWithEndpoints(forge.Options{},
		f.srv.URL+"/api/v3/", f.srv.URL+"/api/uploads/", "https://git.example.com")
	if err != nil {
		t.Fatalf("New without a token: %v", err)
	}
	if err := p.Probe(context.Background()); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if len(f.auth) != 1 || f.auth[0] != "" {
		t.Fatalf("Authorization headers = %q", f.auth)
	}
}
