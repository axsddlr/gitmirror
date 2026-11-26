package config

import (
	"strings"
	"testing"
)

// A forge without a token is a public, read-only site: it can be copied
// from, but a rule that copies to it is a configuration error.
func TestReadOnlyForge(t *testing.T) {
	const forges = `
forges:
  pub:
    type: gitea
    url: https://git.example.com
  home:
    type: gitea
    url: https://home.example.com
    token: tok
`
	c := mustParse(t, forges+"\nrules:\n  - name: r1\n    from: {forge: pub, repo: a/b}\n    to: {forge: home, owner: mirrors}\n", nil, nil)
	if !c.Forges["pub"].ReadOnly() || c.Forges["home"].ReadOnly() {
		t.Fatalf("read-only: pub=%v home=%v", c.Forges["pub"].ReadOnly(), c.Forges["home"].ReadOnly())
	}

	_, err := parse(t, forges+"\nrules:\n  - name: r1\n    from: {forge: home, repo: a/b}\n    to: {forge: pub, owner: mirrors}\n", nil, nil)
	if err == nil || !strings.Contains(err.Error(), `to.forge "pub" has no token`) {
		t.Fatalf("copying to a read-only site: err = %v", err)
	}
}

// A token_file still fails when it cannot be read; there is no silent
// fallback to an anonymous site.
func TestTokenFileUnreadable(t *testing.T) {
	doc := `
forges:
  home:
    type: gitea
    url: https://home.example.com
    token_file: /nonexistent/token
rules:
  - name: r1
    from: {forge: home, repo: a/b}
    to: {forge: home, owner: mirrors}
`
	_, err := parse(t, doc, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "token_file") {
		t.Fatalf("err = %v", err)
	}
}
