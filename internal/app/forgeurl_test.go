package app

import (
	"testing"

	"gitmirror/internal/config"
)

func TestForgeURL(t *testing.T) {
	for _, c := range []struct {
		f    config.Forge
		want string
	}{
		{config.Forge{Type: "github"}, "https://github.com"},
		{config.Forge{Type: "github", URL: "https://GitHub.com/"}, "https://github.com"},
		{config.Forge{Type: "gitea", URL: "HTTPS://Git.Example:443/sub/"}, "https://git.example/sub"},
		{config.Forge{Type: "gitea", URL: "http://git.example:80"}, "http://git.example"},
		{config.Forge{Type: "gitea", URL: "http://git.example:3000"}, "http://git.example:3000"},
		{config.Forge{Type: "gitea", URL: "http://[::1]:3000/"}, "http://[::1]:3000"},
		{config.Forge{Type: "gitea", URL: "https://[::1]:443"}, "https://[::1]"},
		{config.Forge{Type: "fake"}, ""},
	} {
		if got := ForgeURL(c.f); got != c.want {
			t.Errorf("ForgeURL(%+v) = %q, want %q", c.f, got, c.want)
		}
	}
}

func TestBuildSetsEngineForgeURLs(t *testing.T) {
	f := newFixture(t)
	cfg := f.config()
	fg := cfg.Forges["dst"]
	fg.URL = "https://Dst.Example/"
	cfg.Forges["dst"] = fg
	a, err := Build(t.Context(), cfg, f.options())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if got := a.Snapshot().ForgeURLs["dst"]; got != "https://dst.example" {
		t.Fatalf("Snapshot().ForgeURLs[dst] = %q (all %v)", got, a.Snapshot().ForgeURLs)
	}
}
