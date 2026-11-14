package gitx

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// hostHome returns a Runner env whose HOME carries the given global
// .gitconfig and which does not itself point GIT_CONFIG_GLOBAL anywhere, so
// only Runner's own isolation stands between git and that file.
func hostHome(t *testing.T, gitconfig string) []string {
	t.Helper()
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte(gitconfig), 0o644); err != nil {
		t.Fatal(err)
	}
	return []string{"HOME=" + home, "USERPROFILE=" + home, "XDG_CONFIG_HOME=" + filepath.Join(home, "xdg")}
}

func TestHostInsteadOfIgnoredByDefault(t *testing.T) {
	for _, useHost := range []bool{false, true} {
		name := "isolated"
		if useHost {
			name = "host-config"
		}
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			want := f.commit("a.txt")
			f.push("main")
			// A decoy repository the host config redirects the source to.
			decoy := filepath.Join(f.root, "decoy.git")
			tgit(t, f.env, f.root, "init", "-q", "--bare", decoy)
			f.git("checkout", "-q", "-b", "decoy")
			decoyHead := f.commit("decoy.txt")
			f.git("push", "-q", fileURL(t, decoy), "decoy:refs/heads/decoy")

			f.r.Env = hostHome(t, "[url \""+fileURL(t, decoy)+"\"]\n\tinsteadOf = "+f.srcRemote.URL+"\n")
			f.r.UseHostConfig = useHost
			f.sync(SyncOptions{})
			got := refsOf(t, f.env, f.dst)
			if useHost {
				// Proves the fixture is meaningful: host config redirects.
				equalMaps(t, "dst refs (host config)", got, map[string]string{"refs/heads/decoy": decoyHead})
				return
			}
			equalMaps(t, "dst refs (isolated)", got, map[string]string{"refs/heads/main": want})
		})
	}
}

func TestHostExtraHeaderNotVisible(t *testing.T) {
	env := hostHome(t, "[http]\n\textraHeader = X-Leak: host-header\n")
	ctx := context.Background()

	r := &Runner{Env: env}
	out, err := r.run(ctx, spec{args: []string{"config", "--get-all", "http.extraHeader"}})
	if err == nil || strings.TrimSpace(out) != "" {
		t.Fatalf("isolated runner sees host http.extraHeader: out %q err %v", out, err)
	}

	r = &Runner{Env: env, UseHostConfig: true}
	out, err = r.run(ctx, spec{args: []string{"config", "--get-all", "http.extraHeader"}})
	if err != nil || !strings.Contains(out, "X-Leak: host-header") {
		t.Fatalf("host-config runner: out %q err %v", out, err)
	}
}

func TestIsolationEnvSurvivesStrippingAndRunnerEnv(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "host.gitconfig"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "0")
	r := &Runner{Env: []string{"GIT_CONFIG_GLOBAL=" + filepath.Join(t.TempDir(), "extra.gitconfig")}}
	var seen []string
	r.hook = func(c *exec.Cmd) { seen = c.Env }
	if _, err := r.run(context.Background(), spec{args: []string{"version"}}); err != nil {
		t.Fatal(err)
	}
	last := map[string]string{}
	for _, kv := range seen {
		k, v, _ := strings.Cut(kv, "=")
		last[strings.ToUpper(k)] = v
	}
	if last["GIT_CONFIG_NOSYSTEM"] != "1" {
		t.Fatalf("GIT_CONFIG_NOSYSTEM = %q", last["GIT_CONFIG_NOSYSTEM"])
	}
	g := last["GIT_CONFIG_GLOBAL"]
	if g == "" || strings.Contains(g, "host.gitconfig") || strings.Contains(g, "extra.gitconfig") {
		t.Fatalf("GIT_CONFIG_GLOBAL = %q, want the runner's empty file", g)
	}
	if fi, err := os.Stat(g); err != nil || fi.Size() != 0 {
		t.Fatalf("GIT_CONFIG_GLOBAL %q: %v (size must be 0)", g, err)
	}
}

func TestProxyAndCAEnvPassThrough(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://proxy.example:3128")
	t.Setenv("NO_PROXY", "localhost")
	t.Setenv("GIT_SSL_CAINFO", "/etc/ssl/custom.pem")
	env, err := buildEnv(os.Environ(), nil)
	if err != nil {
		t.Fatal(err)
	}
	joined := "\n" + strings.Join(env, "\n") + "\n"
	for _, want := range []string{"HTTPS_PROXY=http://proxy.example:3128", "NO_PROXY=localhost", "GIT_SSL_CAINFO=/etc/ssl/custom.pem"} {
		if !strings.Contains(joined, "\n"+want+"\n") {
			t.Fatalf("%s stripped from the child environment", want)
		}
	}
}
