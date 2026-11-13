package gitx

import (
	"context"
	"crypto/rand"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func requireLFS(t *testing.T) {
	t.Helper()
	if err := exec.Command("git", "lfs", "version").Run(); err != nil {
		t.Skipf("git-lfs not installed: %v", err)
	}
}

func lfsObjects(t *testing.T, bare string) []string {
	t.Helper()
	var oids []string
	_ = filepath.WalkDir(filepath.Join(bare, "lfs", "objects"), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && len(d.Name()) == 64 {
			oids = append(oids, d.Name())
		}
		return nil
	})
	return oids
}

func TestSyncLFSOverFileRemotes(t *testing.T) {
	requireLFS(t)
	f := newFixture(t)
	f.git("lfs", "install", "--local")
	f.git("lfs", "track", "*.bin")
	blob := make([]byte, 4096)
	rand.Read(blob)
	if err := os.WriteFile(filepath.Join(f.work, "a.bin"), blob, 0o644); err != nil {
		t.Fatal(err)
	}
	f.git("add", ".gitattributes", "a.bin")
	f.git("commit", "-q", "-m", "lfs")
	f.push("main")
	srcOIDs := lfsObjects(t, f.src)
	if len(srcOIDs) != 1 {
		t.Fatalf("fixture: src LFS objects = %v", srcOIDs)
	}

	f.resetCmds()
	res := f.sync(SyncOptions{LFS: true})
	equalStrings(t, "dst LFS objects", lfsObjects(t, f.dst), srcOIDs)
	if res.LFSObjects != 1 {
		t.Fatalf("LFSObjects = %d, want 1", res.LFSObjects)
	}

	var fetch, push string
	for _, c := range f.captured() {
		j := strings.Join(c.Args[1:], " ")
		if strings.Contains(j, "lfs fetch") {
			fetch = j
		}
		if strings.Contains(j, "lfs push") {
			push = j
		}
	}
	if !strings.Contains(fetch, "lfs fetch --all -- "+f.srcRemote.URL) {
		t.Fatalf("lfs fetch args = %q", fetch)
	}
	if !strings.Contains(push, "lfs push --all -- "+f.dstRemote.URL) {
		t.Fatalf("lfs push args = %q", push)
	}

	// Nothing changed: LFS step is skipped and LFSObjects is unknown.
	res = f.sync(SyncOptions{LFS: true})
	if res.Changed || res.LFSObjects != -1 {
		t.Fatalf("unchanged LFS run: Changed=%v LFSObjects=%d", res.Changed, res.LFSObjects)
	}
}

func TestSyncLFSUnavailable(t *testing.T) {
	f := newFixture(t)
	f.r.Env = append(f.r.Env, "PATH=")
	probe := exec.Command("git", "lfs", "version")
	probe.Env = append(cleanHostEnv(), "PATH=")
	if probe.Run() == nil {
		t.Skip("git finds git-lfs even with PATH emptied (git wrapper adds its own bin dir); cannot simulate missing git-lfs")
	}
	f.commit("a.txt")
	f.push("main")
	_, err := f.c.Sync(context.Background(), f.srcRemote, f.dstRemote, SyncOptions{LFS: true})
	if !errors.Is(err, ErrLFSUnavailable) {
		t.Fatalf("err = %v, want ErrLFSUnavailable", err)
	}
	if n := len(refsOf(t, f.env, f.dst)); n != 0 {
		t.Fatalf("dst modified (%d refs) although LFS was unavailable", n)
	}
	v, err := f.r.Check(context.Background())
	if err != nil || v.LFS != "" {
		t.Fatalf("Check with no git-lfs: %+v %v", v, err)
	}
}
