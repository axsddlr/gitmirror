package gitx

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

var (
	// ErrEmptySource is returned (wrapped) by Cache.Sync when the source has
	// no branches or tags and SyncOptions.AllowEmptySource is false. Nothing
	// is written to the destination or the cache in that case.
	ErrEmptySource = errors.New("gitx: source repository has no branches or tags")

	// ErrLFSUnavailable is returned (wrapped) when SyncOptions.LFS is set but
	// git-lfs cannot be run.
	ErrLFSUnavailable = errors.New("gitx: git-lfs is not available")

	// ErrPushRejected is returned (wrapped) when the destination rejected one
	// or more refs (e.g. a protected branch); the message lists them.
	ErrPushRejected = errors.New("gitx: destination rejected refs")
)

var mirrorRefspecs = []string{"+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*"}

// Cache is the persistent local bare repository used to mirror one
// source/destination pair. Reusing it across syncs means only deltas are
// transferred.
type Cache struct {
	// Dir is the bare repository path. It is created on first use and
	// re-created if it is found corrupt.
	Dir string
	// R runs git; nil means a zero Runner.
	R *Runner
	// Logger receives warnings (stale lock removed, corrupt cache
	// re-created). Nil means slog.Default().
	Logger *slog.Logger

	// afterLock, if set, runs right after the lock is acquired (tests).
	afterLock func()
}

// SyncOptions controls Cache.Sync.
type SyncOptions struct {
	// Prune deletes destination branches/tags that no longer exist on the
	// source. Without it, extra destination refs are left alone.
	Prune bool
	// LFS also copies Git LFS objects (requires git-lfs).
	LFS bool
	// DryRun computes the result without writing to the destination. The
	// source is still fetched into the local cache (which is ours), so the
	// reported Created/Updated/Deleted are accurate. No LFS transfer occurs.
	DryRun bool
	// AllowEmptySource makes an empty source a successful no-op instead of
	// ErrEmptySource. An empty source never deletes destination refs, even
	// with Prune.
	AllowEmptySource bool
}

// SyncResult describes what a Sync found and did.
type SyncResult struct {
	// SrcRefs are the source branches/tags as fetched into the cache.
	SrcRefs []Ref
	// DstRefsBefore are the destination branches/tags before any push.
	DstRefsBefore []Ref
	// Created, Updated and Deleted list ref names that were (or, with
	// DryRun, would be) created, moved or deleted on the destination.
	// Deleted is only populated when Prune is set.
	Created, Updated, Deleted []string
	// LFSObjects is the number of LFS objects held in the cache after an LFS
	// transfer, or -1 if no LFS transfer ran.
	LFSObjects int
	// Changed reports whether the destination differed from the source.
	Changed bool
}

func (c *Cache) runner() *Runner {
	if c.R == nil {
		return &Runner{}
	}
	return c.R
}

func (c *Cache) logger() *slog.Logger {
	if c.Logger == nil {
		return slog.Default()
	}
	return c.Logger
}

// Sync mirrors all branches and tags of src onto dst through the cache:
//
//  1. ls-remote src; an empty source fails with ErrEmptySource (or is a
//     no-op with AllowEmptySource) before anything is written.
//  2. Ensure the bare cache exists (init, or re-init if corrupt).
//  3. Fetch src heads/tags into the cache with --prune (never --mirror, so
//     read-only namespaces like refs/pull/* are not copied).
//  4. Diff cache refs against ls-remote dst. Nothing differs: stop.
//  5. DryRun: stop here and report the diff.
//  6. With LFS: lfs fetch --all from src, lfs push --all to dst. This runs
//     before the ref push so a failed LFS transfer is retried next time
//     instead of leaving dst with refs whose LFS objects are missing.
//  7. Push heads/tags to dst (--atomic when supported, --prune if Prune).
//
// Sync holds an exclusive lock on Dir (in-process and via Dir+".lock"); a
// concurrent Sync of the same Dir waits until it is released or ctx ends.
// Each command authenticates only to the remote it talks to.
func (c *Cache) Sync(ctx context.Context, src, dst Remote, opt SyncOptions) (*SyncResult, error) {
	res, err := c.sync(ctx, src, dst, opt)
	return res, scrubErr(err, src, dst)
}

func (c *Cache) sync(ctx context.Context, src, dst Remote, opt SyncOptions) (*SyncResult, error) {
	if c == nil || c.Dir == "" {
		return nil, errors.New("gitx: Cache.Dir is empty")
	}
	if err := src.validate(); err != nil {
		return nil, fmt.Errorf("gitx: source: %w", err)
	}
	if err := dst.validate(); err != nil {
		return nil, fmt.Errorf("gitx: destination: %w", err)
	}
	dir, err := filepath.Abs(c.Dir)
	if err != nil {
		return nil, fmt.Errorf("gitx: cache dir: %w", err)
	}
	r := c.runner()

	release, err := c.lock(ctx, dir, r.timeout())
	if err != nil {
		return nil, err
	}
	defer release()
	if c.afterLock != nil {
		c.afterLock()
	}

	if opt.LFS && !opt.DryRun && r.lfsVersion(ctx) == "" {
		return nil, ErrLFSUnavailable
	}

	res := &SyncResult{LFSObjects: -1}
	srcRefs, err := r.LsRemote(ctx, src)
	if err != nil {
		return nil, err
	}
	if len(srcRefs) == 0 {
		return c.emptySource(ctx, r, src, dst, opt, res)
	}

	if err := c.ensure(ctx, r, dir); err != nil {
		return nil, err
	}
	fetch := append([]string{"fetch", "--prune", "--no-tags", "--force", "--", src.URL}, mirrorRefspecs...)
	if _, err := r.run(ctx, spec{gitDir: dir, auth: []Remote{src}, args: fetch}); err != nil {
		return nil, fmt.Errorf("gitx: fetch %s: %w", src, err)
	}
	cacheRefs, err := localRefs(ctx, r, dir)
	if err != nil {
		return nil, err
	}
	if len(cacheRefs) == 0 {
		// Source emptied between ls-remote and fetch.
		return c.emptySource(ctx, r, src, dst, opt, res)
	}
	res.SrcRefs = cacheRefs

	dstRefs, err := r.LsRemote(ctx, dst)
	if err != nil {
		return nil, err
	}
	res.DstRefsBefore = dstRefs
	res.Created, res.Updated, res.Deleted = diffRefs(cacheRefs, dstRefs, opt.Prune)
	res.Changed = len(res.Created)+len(res.Updated)+len(res.Deleted) > 0
	if !res.Changed || opt.DryRun {
		return res, nil
	}

	if opt.LFS {
		if _, err := r.run(ctx, spec{gitDir: dir, auth: []Remote{src}, args: []string{"lfs", "fetch", "--all", "--", src.URL}}); err != nil {
			return nil, fmt.Errorf("gitx: lfs fetch %s: %w", src, err)
		}
		if _, err := r.run(ctx, spec{gitDir: dir, auth: []Remote{dst}, args: []string{"lfs", "push", "--all", "--", dst.URL}}); err != nil {
			return nil, fmt.Errorf("gitx: lfs push %s: %w", dst, err)
		}
		res.LFSObjects = countLFSObjects(dir)
	}

	if err := push(ctx, r, dir, dst, opt.Prune); err != nil {
		return nil, err
	}
	return res, nil
}

func (c *Cache) emptySource(ctx context.Context, r *Runner, src, dst Remote, opt SyncOptions, res *SyncResult) (*SyncResult, error) {
	if !opt.AllowEmptySource {
		return nil, fmt.Errorf("gitx: source %s: %w", src, ErrEmptySource)
	}
	dstRefs, err := r.LsRemote(ctx, dst)
	if err != nil {
		return nil, err
	}
	res.SrcRefs = nil
	res.DstRefsBefore = dstRefs
	return res, nil
}

// ensure makes dir a valid bare repository, re-creating it if corrupt.
func (c *Cache) ensure(ctx context.Context, r *Runner, dir string) error {
	fi, err := os.Stat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return initBare(ctx, r, dir)
	case err != nil:
		return fmt.Errorf("gitx: cache: %w", err)
	case fi.IsDir():
		out, err := r.run(ctx, spec{gitDir: dir, args: []string{"rev-parse", "--is-bare-repository"}})
		if err == nil && strings.TrimSpace(out) == "true" {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	c.logger().Warn("gitx: cache repository is corrupt; re-creating", "dir", dir)
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("gitx: removing corrupt cache: %w", err)
	}
	return initBare(ctx, r, dir)
}

func initBare(ctx context.Context, r *Runner, dir string) error {
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return fmt.Errorf("gitx: cache: %w", err)
	}
	if _, err := r.run(ctx, spec{args: []string{"init", "--bare", "--quiet", "--", dir}}); err != nil {
		return fmt.Errorf("gitx: init cache: %w", err)
	}
	return nil
}

func localRefs(ctx context.Context, r *Runner, dir string) ([]Ref, error) {
	out, err := r.run(ctx, spec{gitDir: dir, args: []string{"for-each-ref", "--format=%(objectname)\t%(refname)", "refs/heads", "refs/tags"}})
	if err != nil {
		return nil, fmt.Errorf("gitx: reading cache refs: %w", err)
	}
	return parseRefs(out), nil
}

func diffRefs(src, dst []Ref, prune bool) (created, updated, deleted []string) {
	d := make(map[string]string, len(dst))
	for _, r := range dst {
		d[r.Name] = r.Hash
	}
	s := make(map[string]bool, len(src))
	for _, r := range src {
		s[r.Name] = true
		h, ok := d[r.Name]
		switch {
		case !ok:
			created = append(created, r.Name)
		case h != r.Hash:
			updated = append(updated, r.Name)
		}
	}
	if prune {
		for _, r := range dst {
			if !s[r.Name] {
				deleted = append(deleted, r.Name)
			}
		}
	}
	sort.Strings(created)
	sort.Strings(updated)
	sort.Strings(deleted)
	return created, updated, deleted
}

// push pushes cache heads/tags to dst, atomically when the remote supports
// it, and reports per-ref rejections from --porcelain output.
func push(ctx context.Context, r *Runner, dir string, dst Remote, prune bool) error {
	args := func(atomic bool) []string {
		a := []string{"push", "--porcelain", "--force"}
		if prune {
			a = append(a, "--prune")
		}
		if atomic {
			a = append(a, "--atomic")
		}
		a = append(a, "--", dst.URL)
		return append(a, mirrorRefspecs...)
	}
	out, err := r.run(ctx, spec{gitDir: dir, auth: []Remote{dst}, args: args(true)})
	rejected, statuses := parsePorcelain(out)
	if err != nil && statuses == 0 && ctx.Err() == nil && strings.Contains(strings.ToLower(err.Error()), "atomic") {
		// The remote does not support atomic pushes; retry once without.
		out, err = r.run(ctx, spec{gitDir: dir, auth: []Remote{dst}, args: args(false)})
		rejected, _ = parsePorcelain(out)
	}
	if len(rejected) > 0 {
		return scrubErr(fmt.Errorf("gitx: push to %s: %w: %s", dst, ErrPushRejected, strings.Join(rejected, "; ")), dst)
	}
	if err != nil {
		return fmt.Errorf("gitx: push to %s: %w", dst, err)
	}
	return nil
}

// parsePorcelain returns "ref summary" for every rejected ('!') line and
// the number of per-ref status lines seen.
func parsePorcelain(out string) (rejected []string, statuses int) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if len(line) < 3 || line[1] != '\t' {
			continue
		}
		statuses++
		if line[0] != '!' {
			continue
		}
		pair, summary, _ := strings.Cut(line[2:], "\t")
		ref := pair
		if i := strings.LastIndexByte(pair, ':'); i >= 0 {
			ref = pair[i+1:]
		}
		rejected = append(rejected, strings.TrimSpace(ref+" "+summary))
	}
	return rejected, statuses
}

// countLFSObjects counts LFS objects stored in the cache.
func countLFSObjects(dir string) int {
	n := 0
	err := filepath.WalkDir(filepath.Join(dir, "lfs", "objects"), func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && len(d.Name()) == 64 {
			n++
		}
		return nil
	})
	if err != nil {
		return -1
	}
	return n
}
