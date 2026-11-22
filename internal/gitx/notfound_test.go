package gitx

import (
	"errors"
	"testing"
)

func TestRepoNotFound(t *testing.T) {
	cases := map[string]bool{
		"git ls-remote: exit status 128: remote: Repository not found.\nfatal: repository 'https://github.com/o/r.wiki.git/' not found": true,
		"fatal: repository 'https://gitea.example/o/r.wiki.git/' not found":                                                             true,
		"fatal: '/tmp/x/o/r.wiki.git' does not appear to be a git repository":                                                           true,
		"fatal: Authentication failed for 'https://github.com/o/r.git/'":                                                                false,
		"fatal: unable to access 'https://github.com/o/r.git/': Could not resolve host: github.com":                                     false,
	}
	for msg, want := range cases {
		if got := repoNotFound(errors.New(msg)); got != want {
			t.Errorf("repoNotFound(%q) = %v, want %v", msg, got, want)
		}
	}
	if repoNotFound(nil) {
		t.Error("repoNotFound(nil) = true")
	}
}
