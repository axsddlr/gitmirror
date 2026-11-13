package gitx

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"testing"
)

const sentinel = "SENTINEL-TOKEN-xyz"

func TestUnsafeURLsRejectedBeforeGit(t *testing.T) {
	f := newFixture(t)
	bad := []string{
		"",
		"ext::sh -c touch% pwned",
		"ext::sh",
		"-oProxyCommand=calc",
		"--upload-pack=calc",
		"ssh://example.com/r.git",
		"git@github.com:o/r.git",
		"fd::17",
		"https://user:pw@example.com/r.git",
		"https://",
		"file://",
		"file://host/share/r.git",
		"https://exa mple.com/r",
		"https://example.com/r\n.git",
		"/local/path",
		`C:\repo`,
	}
	for _, u := range bad {
		f.resetCmds()
		rem := Remote{URL: u}
		if _, err := f.r.LsRemote(context.Background(), rem); !errors.Is(err, ErrUnsafeURL) {
			t.Errorf("LsRemote(%q) err = %v, want ErrUnsafeURL", u, err)
		}
		if _, err := f.c.Sync(context.Background(), rem, f.dstRemote, SyncOptions{}); !errors.Is(err, ErrUnsafeURL) {
			t.Errorf("Sync(src=%q) err = %v, want ErrUnsafeURL", u, err)
		}
		if _, err := f.c.Sync(context.Background(), f.srcRemote, rem, SyncOptions{}); !errors.Is(err, ErrUnsafeURL) {
			t.Errorf("Sync(dst=%q) err = %v, want ErrUnsafeURL", u, err)
		}
		if n := len(f.captured()); n != 0 {
			t.Errorf("URL %q: git was invoked %d times", u, n)
		}
	}
	if err := (Remote{URL: "https://user:pw@example.com/r.git"}).validate(); err != nil && strings.Contains(err.Error(), "pw") {
		t.Fatalf("validation error leaks embedded password: %v", err)
	}
}

func TestTokenScrubbedFromNetworkErrors(t *testing.T) {
	f := newFixture(t)
	rem := Remote{URL: "http://127.0.0.1:1/repo.git", Token: sentinel}
	_, err := f.r.LsRemote(context.Background(), rem)
	if err == nil {
		t.Fatal("LsRemote to unreachable remote succeeded")
	}
	for _, s := range secretForms("", sentinel) {
		if strings.Contains(err.Error(), s) {
			t.Fatalf("LsRemote error leaks %q: %v", s, err)
		}
	}
	_, err = f.c.Sync(context.Background(), rem, f.dstRemote, SyncOptions{})
	if err == nil {
		t.Fatal("Sync from unreachable remote succeeded")
	}
	for _, s := range secretForms("", sentinel) {
		if strings.Contains(err.Error(), s) {
			t.Fatalf("Sync error leaks %q: %v", s, err)
		}
	}
	assertCmdHygiene(t, f.captured(), "", sentinel)
}

// TestTokenScrubbedFromGitStderr uses a fake git that echoes the auth env
// (both base64 and decoded) to stderr, proving the scrubber actually runs.
func TestTokenScrubbedFromGitStderr(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	r := &Runner{Git: exe, Env: append(testEnv(t), "GITX_FAKE_GIT=1")}
	rem := Remote{URL: "https://git.example.com/o/r.git", Username: "bob", Token: sentinel}
	_, err = r.LsRemote(context.Background(), rem)
	if err == nil {
		t.Fatal("fake git succeeded")
	}
	msg := err.Error()
	if !strings.Contains(msg, "***") {
		t.Fatalf("expected scrubbed marker in error, got: %v", msg)
	}
	for _, s := range secretForms("bob", sentinel) {
		if strings.Contains(msg, s) {
			t.Fatalf("error leaks %q: %v", s, msg)
		}
	}
}

func TestScrub(t *testing.T) {
	rem := Remote{URL: "https://h/r.git", Username: "u", Token: "tok/en+1"}
	enc := base64.StdEncoding.EncodeToString([]byte("u:tok/en+1"))
	in := fmt.Sprintf("a tok/en+1 b %s c tok%%2Fen%%2B1 d", enc)
	out := scrub(in, rem)
	if strings.Contains(out, "tok/en+1") || strings.Contains(out, enc) || strings.Contains(out, "tok%2Fen%2B1") {
		t.Fatalf("scrub left secrets: %q", out)
	}
	if got := scrub("nothing here", Remote{URL: "file:///x"}); got != "nothing here" {
		t.Fatalf("scrub with empty token changed text: %q", got)
	}
}

func TestRemoteStringNeverShowsCredentials(t *testing.T) {
	rem := Remote{URL: "https://u:pw@git.example.com/o/r.git?access_token=qq", Username: "u", Token: sentinel}
	for _, s := range []string{rem.String(), fmt.Sprint(rem), fmt.Sprintf("%+v", rem), fmt.Sprintf("%#v", rem)} {
		if strings.Contains(s, sentinel) || strings.Contains(s, "pw") || strings.Contains(s, "qq") {
			t.Fatalf("formatted Remote leaks credentials: %q", s)
		}
		if !strings.Contains(s, "git.example.com/o/r.git") {
			t.Fatalf("formatted Remote lost URL: %q", s)
		}
	}
	var buf bytes.Buffer
	slog.New(slog.NewTextHandler(&buf, nil)).Info("sync", "remote", rem)
	if strings.Contains(buf.String(), sentinel) || !strings.Contains(buf.String(), "git.example.com") {
		t.Fatalf("slog output: %q", buf.String())
	}
}

func envMap(env []string) map[string]string {
	m := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		m[strings.ToUpper(k)] = v
	}
	return m
}

func TestBuildEnv(t *testing.T) {
	base := []string{
		"PATH=/bin", "HOME=/h",
		"GIT_CONFIG_COUNT=5", "GIT_CONFIG_KEY_0=http.extraHeader", "git_config_value_0=evil",
		"GIT_CONFIG_PARAMETERS='core.sshCommand=evil'", "GIT_CONFIG_GLOBAL=/evil",
		"GIT_DIR=/evil", "GIT_WORK_TREE=/evil", "GIT_INDEX_FILE=/evil",
		"GIT_TRACE_CURL=1", "GIT_ASKPASS=/evil/askpass", "GIT_ALLOW_PROTOCOL=ext",
	}
	env, err := buildEnv(base, []string{"EXTRA=1"}, Remote{URL: "https://Example.COM:8443/o/r.git", Token: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	m := envMap(env)
	for _, k := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_CONFIG_PARAMETERS", "GIT_CONFIG_GLOBAL", "GIT_TRACE_CURL", "GIT_CONFIG_KEY_1"} {
		if _, ok := m[k]; ok {
			t.Errorf("%s not stripped", k)
		}
	}
	for _, kv := range env {
		if strings.Contains(kv, "evil") || strings.Contains(kv, "/evil/askpass") {
			t.Errorf("host value survived: %q", kv)
		}
	}
	want := map[string]string{
		"PATH": "/bin", "HOME": "/h", "EXTRA": "1",
		"GIT_TERMINAL_PROMPT": "0", "GCM_INTERACTIVE": "never",
		"GIT_ASKPASS": "", "SSH_ASKPASS": "",
		"GIT_ALLOW_PROTOCOL": "https:http:file",
		"GIT_CONFIG_COUNT":   "1",
		"GIT_CONFIG_KEY_0":   "http.https://example.com:8443/.extraHeader",
		"GIT_CONFIG_VALUE_0": "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:tok")),
	}
	for k, v := range want {
		got, ok := m[k]
		if !ok || got != v {
			t.Errorf("%s = %q (present=%v), want %q", k, got, ok, v)
		}
	}

	t.Run("different origins both set", func(t *testing.T) {
		env, err := buildEnv(nil, nil,
			Remote{URL: "https://a.example/r.git", Token: "A"},
			Remote{URL: "https://b.example/r.git", Username: "bob", Token: "B"})
		if err != nil {
			t.Fatal(err)
		}
		m := envMap(env)
		if m["GIT_CONFIG_COUNT"] != "2" || m["GIT_CONFIG_KEY_0"] != "http.https://a.example/.extraHeader" || m["GIT_CONFIG_KEY_1"] != "http.https://b.example/.extraHeader" {
			t.Fatalf("env = %v", m)
		}
	})
	t.Run("same origin same creds deduped", func(t *testing.T) {
		env, err := buildEnv(nil, nil,
			Remote{URL: "https://a.example/x.git", Token: "A"},
			Remote{URL: "https://A.example/y.git", Token: "A"})
		if err != nil {
			t.Fatal(err)
		}
		if envMap(env)["GIT_CONFIG_COUNT"] != "1" {
			t.Fatalf("env = %v", env)
		}
	})
	t.Run("same origin different tokens refused", func(t *testing.T) {
		_, err := buildEnv(nil, nil,
			Remote{URL: "https://a.example/x.git", Token: "A"},
			Remote{URL: "https://a.example/y.git", Token: "B"})
		if err == nil {
			t.Fatal("buildEnv allowed two different headers for one origin")
		}
		if strings.Contains(err.Error(), "A") && strings.Contains(err.Error(), "B\"") {
			t.Fatalf("error leaks tokens: %v", err)
		}
	})
	t.Run("file and tokenless remotes add no header", func(t *testing.T) {
		env, err := buildEnv(nil, nil, Remote{URL: "file:///D:/x.git", Token: "A"}, Remote{URL: "https://a.example/x.git"})
		if err != nil {
			t.Fatal(err)
		}
		if envMap(env)["GIT_CONFIG_COUNT"] != "0" {
			t.Fatalf("env = %v", env)
		}
	})
	t.Run("header injection refused", func(t *testing.T) {
		if _, err := buildEnv(nil, nil, Remote{URL: "https://a.example/x.git", Token: "A\r\nX-Evil: 1"}); err == nil {
			t.Fatal("CRLF in token accepted")
		}
		if _, err := buildEnv(nil, nil, Remote{URL: "https://a.example/x.git", Username: "a:b", Token: "A"}); err == nil {
			t.Fatal("colon in username accepted")
		}
	})
}

func TestHTTPAuthDifferentOrigins(t *testing.T) {
	f := newFixture(t)
	tokA, tokB := "SRC-"+randomHex(t, 12), "DST-"+randomHex(t, 12)
	srcURL := startGitHTTP(t, f.root, map[string]string{"src.git": "x-access-token:" + tokA})
	dstURL := startGitHTTP(t, f.root, map[string]string{"dst.git": "alice:" + tokB})
	f.commit("a.txt")
	f.git("tag", "-a", "v1", "-m", "v1")
	f.push("main", "v1")

	src := Remote{URL: srcURL + "/src.git", Token: tokA}
	dst := Remote{URL: dstURL + "/dst.git", Username: "alice", Token: tokB}
	f.resetCmds()
	res, err := f.c.Sync(context.Background(), src, dst, SyncOptions{Prune: true})
	if err != nil {
		t.Fatalf("Sync over HTTP: %v", err)
	}
	if !res.Changed {
		t.Fatal("Changed = false")
	}
	equalMaps(t, "dst refs", refsOf(t, f.env, f.dst), refsOf(t, f.env, f.src))

	cmds := f.captured()
	assertCmdHygiene(t, cmds, "", tokA)
	assertCmdHygiene(t, cmds, "alice", tokB)
	assertAuthScoping(t, cmds, src, dst)
	home := envMap(f.env)["HOME"]
	assertNoSecretsOnDisk(t, append(secretForms("", tokA), secretForms("alice", tokB)...), f.cache, home, f.src, f.dst)

	// A wrong destination token fails, and the failure does not leak it.
	f.commit("b.txt")
	f.push("main")
	bad := Remote{URL: dst.URL, Username: "alice", Token: sentinel}
	_, err = f.c.Sync(context.Background(), src, bad, SyncOptions{})
	if err == nil {
		t.Fatal("Sync with wrong token succeeded")
	}
	for _, s := range append(secretForms("alice", sentinel), secretForms("", tokA)...) {
		if strings.Contains(err.Error(), s) {
			t.Fatalf("error leaks %q: %v", s, err)
		}
	}
}

// TestHTTPAuthSameOriginDifferentTokens serves src and dst from one origin;
// the server rejects any request carrying more than one Authorization
// header, so the sync only succeeds if fetch and push are split.
func TestHTTPAuthSameOriginDifferentTokens(t *testing.T) {
	f := newFixture(t)
	tokA, tokB := "SRC-"+randomHex(t, 12), "DST-"+randomHex(t, 12)
	base := startGitHTTP(t, f.root, map[string]string{
		"src.git": "x-access-token:" + tokA,
		"dst.git": "x-access-token:" + tokB,
	})
	f.commit("a.txt")
	f.push("main")
	src := Remote{URL: base + "/src.git", Token: tokA}
	dst := Remote{URL: base + "/dst.git", Token: tokB}
	f.resetCmds()
	if _, err := f.c.Sync(context.Background(), src, dst, SyncOptions{}); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	equalMaps(t, "dst refs", refsOf(t, f.env, f.dst), refsOf(t, f.env, f.src))
	cmds := f.captured()
	assertAuthScoping(t, cmds, src, dst)
	for _, c := range cmds {
		if cmdHasSecret(c, "", tokA) && cmdHasSecret(c, "", tokB) {
			t.Fatalf("command %q carries both tokens", c.Args)
		}
	}
}

// assertAuthScoping: commands addressing src carry only src credentials;
// commands addressing dst carry only dst credentials.
func assertAuthScoping(t *testing.T, cmds []*exec.Cmd, src, dst Remote) {
	t.Helper()
	var sawSrc, sawDst bool
	for _, c := range cmds {
		switch {
		case cmdMentions(c, src.URL):
			sawSrc = true
			if !cmdHasSecret(c, src.Username, src.Token) || cmdHasSecret(c, dst.Username, dst.Token) {
				t.Errorf("src command %q has wrong credentials", c.Args)
			}
		case cmdMentions(c, dst.URL):
			sawDst = true
			if !cmdHasSecret(c, dst.Username, dst.Token) || cmdHasSecret(c, src.Username, src.Token) {
				t.Errorf("dst command %q has wrong credentials", c.Args)
			}
		default:
			if cmdHasSecret(c, src.Username, src.Token) || cmdHasSecret(c, dst.Username, dst.Token) {
				t.Errorf("local command %q carries credentials", c.Args)
			}
		}
	}
	if !sawSrc || !sawDst {
		t.Fatalf("expected commands for both remotes (src=%v dst=%v)", sawSrc, sawDst)
	}
}

func TestRunnerAlwaysDisablesCredentialHelpers(t *testing.T) {
	f := newFixture(t)
	f.commit("a.txt")
	f.push("main")
	f.sync(SyncOptions{})
	cmds := f.captured()
	if len(cmds) == 0 {
		t.Fatal("no commands captured")
	}
	for _, c := range cmds {
		if len(c.Args) < 3 || c.Args[1] != "-c" || c.Args[2] != "credential.helper=" {
			t.Errorf("command %q does not start with -c credential.helper=", c.Args)
		}
		m := envMap(c.Env)
		if m["GIT_TERMINAL_PROMPT"] != "0" || m["GCM_INTERACTIVE"] != "never" || m["GIT_ALLOW_PROTOCOL"] != "https:http:file" {
			t.Errorf("command %q missing safety env", c.Args)
		}
		if _, ok := m["GIT_ASKPASS"]; !ok {
			t.Errorf("GIT_ASKPASS not set (empty) for %q", c.Args)
		}
		if c.Cancel == nil {
			t.Errorf("command %q not created with a context", c.Args)
		}
	}

}
