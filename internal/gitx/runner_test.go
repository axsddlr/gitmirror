package gitx

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func TestCheck(t *testing.T) {
	r := &Runner{Env: testEnv(t)}
	v, err := r.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !strings.HasPrefix(v.Git, "2.") {
		t.Fatalf("Git version = %q", v.Git)
	}
	t.Logf("versions: git=%q lfs=%q", v.Git, v.LFS)
}

func TestCheckGitVersion(t *testing.T) {
	for s, ok := range map[string]bool{
		"2.47.1.windows.1": true,
		"2.32.0":           true,
		"2.32":             true,
		"3.0.0":            true,
		"2.31.8":           false, // no GIT_CONFIG_GLOBAL: host config isolation would silently not apply
		"2.30.0":           false,
		"2.29.9":           false,
		"1.9.5":            false,
		"garbage":          false,
		"":                 false,
	} {
		err := checkGitVersion(s)
		if (err == nil) != ok {
			t.Errorf("checkGitVersion(%q) = %v, want ok=%v", s, err, ok)
		}
	}
}

func TestLsRemote(t *testing.T) {
	f := newFixture(t)
	refs, err := f.r.LsRemote(context.Background(), f.srcRemote)
	if err != nil || len(refs) != 0 {
		t.Fatalf("empty repo: refs=%v err=%v", refs, err)
	}

	f.commit("a.txt")
	f.git("tag", "-a", "v2", "-m", "annotated")
	f.git("tag", "v1")
	f.git("checkout", "-q", "-b", "zeta")
	f.commit("z.txt")
	f.push("main", "zeta", "v1", "v2", "zeta:refs/pull/7/head")

	refs, err = f.r.LsRemote(context.Background(), f.srcRemote)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, r := range refs {
		names = append(names, r.Name)
		if strings.HasSuffix(r.Name, "^{}") {
			t.Fatalf("peeled entry %s returned", r.Name)
		}
	}
	if got := strings.Join(names, ","); got != "refs/heads/main,refs/heads/zeta,refs/tags/v1,refs/tags/v2" {
		t.Fatalf("names = %s", got)
	}
	tagObj := tgit(t, f.env, f.src, "rev-parse", "refs/tags/v2")
	if refs[3].Hash != tagObj {
		t.Fatalf("annotated tag hash = %s, want tag object %s", refs[3].Hash, tagObj)
	}
}

func TestParseRefs(t *testing.T) {
	out := "bbb\trefs/tags/v1\naaa\trefs/heads/main\nccc\tHEAD\nddd\trefs/tags/v1^{}\neee\trefs/pull/1/head\nfff\trefs/heads/a\n\n"
	refs := parseRefs(out)
	want := []Ref{{"refs/heads/a", "fff"}, {"refs/heads/main", "aaa"}, {"refs/tags/v1", "bbb"}}
	if len(refs) != len(want) {
		t.Fatalf("refs = %v", refs)
	}
	for i := range want {
		if refs[i] != want[i] {
			t.Fatalf("refs = %v, want %v", refs, want)
		}
	}
}

func TestDigest(t *testing.T) {
	a := []Ref{{"refs/heads/main", "aaa"}, {"refs/tags/v1", "bbb"}}
	b := []Ref{{"refs/tags/v1", "bbb"}, {"refs/heads/main", "aaa"}}
	if Digest(a) != Digest(b) {
		t.Fatal("Digest depends on input order")
	}
	if b[0].Name != "refs/tags/v1" {
		t.Fatal("Digest mutated its input")
	}
	sum := sha256.Sum256([]byte("aaa refs/heads/main\nbbb refs/tags/v1\n"))
	if Digest(a) != hex.EncodeToString(sum[:]) {
		t.Fatalf("Digest = %s", Digest(a))
	}
	if Digest(nil) != "" || Digest([]Ref{}) != "" {
		t.Fatal("empty Digest not empty string")
	}
	c := []Ref{{"refs/heads/main", "aab"}, {"refs/tags/v1", "bbb"}}
	if Digest(a) == Digest(c) {
		t.Fatal("Digest ignores hash change")
	}
}

func TestParsePorcelain(t *testing.T) {
	out := strings.Join([]string{
		"To file:///x",
		"!\trefs/heads/main:refs/heads/main\t[remote rejected] (protected branch hook declined)",
		"=\trefs/heads/dev:refs/heads/dev\t[up to date]",
		"*\trefs/tags/v1:refs/tags/v1\t[new tag]",
		"-\t:refs/heads/old\t[deleted]",
		"!\trefs/heads/x:refs/heads/x\t[rejected] (non-fast-forward)",
		"Done",
	}, "\n")
	rejected, statuses := parsePorcelain(out)
	if statuses != 5 {
		t.Fatalf("statuses = %d", statuses)
	}
	equalStrings(t, "rejected", rejected, []string{
		"refs/heads/main [remote rejected] (protected branch hook declined)",
		"refs/heads/x [rejected] (non-fast-forward)",
	})
}

func TestDiffRefs(t *testing.T) {
	src := []Ref{{"refs/heads/a", "1"}, {"refs/heads/b", "2"}, {"refs/tags/t", "3"}}
	dst := []Ref{{"refs/heads/a", "1"}, {"refs/heads/b", "9"}, {"refs/heads/gone", "4"}}
	c, u, d := diffRefs(src, dst, false)
	equalStrings(t, "created", c, []string{"refs/tags/t"})
	equalStrings(t, "updated", u, []string{"refs/heads/b"})
	equalStrings(t, "deleted", d, nil)
	_, _, d = diffRefs(src, dst, true)
	equalStrings(t, "deleted", d, []string{"refs/heads/gone"})
}
