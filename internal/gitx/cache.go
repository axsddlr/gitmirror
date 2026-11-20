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
	"unicode/utf8"
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

	// ErrDestination is matched (errors.Is) by Sync errors that came from
	// talking to the destination remote (listing, LFS push or ref push), as
	// opposed to the source or the local cache.
	ErrDestination = errors.New("gitx: destination remote failed")
)

// Bounds on the remote's own output quoted in an ErrPushRejected error.
const (
	maxRemoteLines  = 10
	maxRemoteReason = 2 << 10
)

// dstError marks an error as coming from the destination remote without
// changing its message.
type dstError struct{ err error }

func (e *dstError) Error() string        { return e.err.Error() }
func (e *dstError) Unwrap() error        { return e.err }
func (e *dstError) Is(target error) bool { return target == ErrDestination }

func onDst(err error) error {
	if err == nil {
		return nil
	}
	return &dstError{err: err}
}

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
	// FirstBranch, when set and the destination has no branches or tags at
	// all, is pushed on its own (refs/heads/<FirstBranch>) before the full
	// push. Forges such as Gitea make the first pushed branch the default
	// branch of an empty repository, so callers pass the source's default
	// branch. It is ignored when the source has no such branch.
	FirstBranch string
	// CheckDeletions, when set, is called (also in a DryRun) with the
	// destination refs the sync would delete and the destination's total
	// number of branches and tags, before anything is written. A non-nil
	// error aborts the sync: Sync returns it wrapped together with the
	// computed result, and nothing is transferred. It is not called when
	// nothing would be deleted.
	CheckDeletions func(deleted []string, dstTotal int) error
	// LFSDigest is the Digest of the source refs for which LFS objects are
	// known to have been copied to the destination. With LFS set and the
	// refs otherwise up to date, LFS objects are still fetched and pushed
	// when the current source digest differs from it, so a destination
	// whose refs were pushed without LFS (git-lfs missing, lfs enabled
	// later) heals. Leave it empty to force the LFS transfer.
	LFSDigest string
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
//  4. Diff cache refs against ls-remote dst; with planned deletions, ask
//     CheckDeletions (an error aborts here). Nothing differs and no LFS
//     transfer is owed (see LFSDigest): stop.
//  5. DryRun: stop here and report the diff.
//  6. With LFS: lfs fetch --all from src, lfs push --all to dst, each pinned
//     to its remote's LFS endpoint. This runs before the ref push so a
//     failed LFS transfer is retried next time instead of leaving dst with
//     refs whose LFS objects are missing.
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
		return nil, onDst(err)
	}
	res.DstRefsBefore = dstRefs
	res.Created, res.Updated, res.Deleted = diffRefs(cacheRefs, dstRefs, opt.Prune)
	res.Changed = len(res.Created)+len(res.Updated)+len(res.Deleted) > 0
	if len(res.Deleted) > 0 && opt.CheckDeletions != nil {
		if err := opt.CheckDeletions(res.Deleted, len(dstRefs)); err != nil {
			return res, fmt.Errorf("gitx: refusing to push to %s: %w", dst, err)
		}
	}
	lfsHeal := opt.LFS && !res.Changed && Digest(cacheRefs) != opt.LFSDigest
	if (!res.Changed && !lfsHeal) || opt.DryRun {
		return res, nil
	}

	if opt.LFS {
		if err := lfsTransfer(ctx, r, dir, src, dst); err != nil {
			return nil, err
		}
		res.LFSObjects = countLFSObjects(dir)
	}
	if !res.Changed {
		return res, nil
	}

	if first := firstBranchRef(opt.FirstBranch, cacheRefs, dstRefs); first != "" {
		if err := push(ctx, r, dir, dst, false, []string{"+" + first + ":" + first}); err != nil {
			return nil, err
		}
	}
	if err := push(ctx, r, dir, dst, opt.Prune, mirrorRefspecs); err != nil {
		return nil, err
	}
	return res, nil
}

// lfsTransfer copies all LFS objects reachable from the cache's refs from src
// into the cache and from there to dst. Each command pins git-lfs to the
// endpoint of the remote it talks to (lfs.url and lfs.pushurl given with -c
// outrank any .lfsconfig) and resets the other keys git-lfs accepts from a
// repository's .lfsconfig, so mirrored content cannot redirect the transfer
// or make it skip objects.
func lfsTransfer(ctx context.Context, r *Runner, dir string, src, dst Remote) error {
	if _, err := r.run(ctx, spec{gitDir: dir, auth: []Remote{src}, cfg: lfsPin(src), args: []string{"lfs", "fetch", "--all", "--", src.URL}}); err != nil {
		return fmt.Errorf("gitx: lfs fetch %s: %w", src, err)
	}
	// Push the fetched objects by id. "lfs push --all" would walk the full
	// history of every ref a second time, which runs for hours on
	// repositories with many branches; the destination's batch API skips
	// objects it already has.
	oids := lfsObjectIDs(dir)
	for len(oids) > 0 {
		n := min(len(oids), lfsPushBatch)
		args := append([]string{"lfs", "push", "--object-id", "--", dst.URL}, oids[:n]...)
		if _, err := r.run(ctx, spec{gitDir: dir, auth: []Remote{dst}, cfg: lfsPin(dst), args: args}); err != nil {
			return onDst(fmt.Errorf("gitx: lfs push %s: %w", dst, err))
		}
		oids = oids[n:]
	}
	return nil
}

// lfsPushBatch is how many object ids one lfs push command carries, which
// keeps the command line well inside every operating system's limit.
var lfsPushBatch = 200

// lfsPin returns the git config overrides for an LFS command talking to rem.
func lfsPin(rem Remote) []string {
	ep := lfsEndpoint(rem.URL)
	return []string{
		"lfs.url=" + ep,
		"lfs.pushurl=" + ep,
		"lfs.fetchinclude=",
		"lfs.fetchexclude=",
		"lfs.skipdownloaderrors=false",
		"lfs.allowincompletepush=false",
		// A mirror takes no locks. Verifying them costs a request per pushed
		// ref (slow for repositories with many branches and tags) and prints
		// a "Locking support detected" hint for each.
		"lfs.locksverify=false",
	}
}

// lfsEndpoint returns the LFS endpoint git-lfs derives for a remote URL:
// file:// URLs are used as is; for http(s) a trailing "/" is dropped, ".git"
// appended unless present, then "/info/lfs".
func lfsEndpoint(raw string) string {
	if strings.HasPrefix(strings.ToLower(raw), "file:") {
		return raw
	}
	u := strings.TrimSuffix(raw, "/")
	if !strings.HasSuffix(u, ".git") {
		u += ".git"
	}
	return u + "/info/lfs"
}

// firstBranchRef returns refs/heads/<branch> when it must be pushed on its
// own first: the destination has no refs and the source has that branch.
func firstBranchRef(branch string, src, dst []Ref) string {
	if branch == "" || len(dst) > 0 {
		return ""
	}
	name := "refs/heads/" + branch
	for _, r := range src {
		if r.Name == name {
			return name
		}
	}
	return ""
}

func (c *Cache) emptySource(ctx context.Context, r *Runner, src, dst Remote, opt SyncOptions, res *SyncResult) (*SyncResult, error) {
	if !opt.AllowEmptySource {
		return nil, fmt.Errorf("gitx: source %s: %w", src, ErrEmptySource)
	}
	dstRefs, err := r.LsRemote(ctx, dst)
	if err != nil {
		return nil, onDst(err)
	}
	res.SrcRefs = nil
	res.DstRefsBefore = dstRefs
	return res, nil
}

// cacheHEAD is where the cache's HEAD points: a ref that is never fetched,
// so HEAD never resolves to mirrored content. git-lfs reads .lfsconfig from
// the HEAD tree; a source branch named like the default HEAD target would
// otherwise control git-lfs's endpoints.
const cacheHEAD = "refs/gitmirror/none"

// ensure makes dir a valid bare repository, re-creating it if corrupt, and
// points its HEAD at cacheHEAD (also for caches created by older versions).
func (c *Cache) ensure(ctx context.Context, r *Runner, dir string) error {
	if err := c.ensureRepo(ctx, r, dir); err != nil {
		return err
	}
	if _, err := r.run(ctx, spec{gitDir: dir, args: []string{"symbolic-ref", "HEAD", cacheHEAD}}); err != nil {
		return fmt.Errorf("gitx: cache HEAD: %w", err)
	}
	return nil
}

func (c *Cache) ensureRepo(ctx context.Context, r *Runner, dir string) error {
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
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
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

// push pushes refspecs from the cache to dst, atomically when the remote
// supports it, and reports per-ref rejections from --porcelain output
// together with the remote's own explanation. Errors are marked
// ErrDestination.
func push(ctx context.Context, r *Runner, dir string, dst Remote, prune bool, refspecs []string) error {
	args := func(atomic bool) []string {
		a := []string{"push", "--porcelain", "--force"}
		if prune {
			a = append(a, "--prune")
		}
		if atomic {
			a = append(a, "--atomic")
		}
		a = append(a, "--", dst.URL)
		return append(a, refspecs...)
	}
	out, err := r.run(ctx, spec{gitDir: dir, auth: []Remote{dst}, args: args(true)})
	rejected, statuses := parsePorcelain(out)
	if err != nil && statuses == 0 && ctx.Err() == nil && strings.Contains(strings.ToLower(err.Error()), "atomic") {
		// The remote does not support atomic pushes; retry once without.
		out, err = r.run(ctx, spec{gitDir: dir, auth: []Remote{dst}, args: args(false)})
		rejected, _ = parsePorcelain(out)
	}
	if len(rejected) > 0 {
		msg := strings.Join(rejected, "; ")
		var ce *cmdError
		if errors.As(err, &ce) {
			if why := remoteReason(ce.stderr); why != "" {
				msg += "; remote: " + why
			}
		}
		return onDst(scrubErr(fmt.Errorf("gitx: push to %s: %w: %s", dst, ErrPushRejected, msg), dst))
	}
	if err != nil {
		return onDst(fmt.Errorf("gitx: push to %s: %w", dst, err))
	}
	return nil
}

// remoteReason extracts what the remote said (its "remote: ..." stderr
// lines, e.g. a pre-receive hook's message) from already-scrubbed git
// stderr: the last maxRemoteLines non-empty lines joined with " | ", cut to
// at most maxRemoteReason bytes (keeping the end).
func remoteReason(stderr string) string {
	var lines []string
	for _, l := range strings.Split(stderr, "\n") {
		l = strings.TrimSpace(strings.ReplaceAll(l, "\x1b[K", ""))
		rest, ok := strings.CutPrefix(l, "remote:")
		if !ok {
			continue
		}
		if rest = strings.TrimSpace(rest); rest != "" {
			lines = append(lines, rest)
		}
	}
	if len(lines) > maxRemoteLines {
		lines = lines[len(lines)-maxRemoteLines:]
	}
	s := strings.Join(lines, " | ")
	if len(s) > maxRemoteReason {
		s = s[len(s)-maxRemoteReason:]
		for len(s) > 0 && !utf8.RuneStart(s[0]) {
			s = s[1:]
		}
		s = "..." + s
	}
	return s
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

// countLFSObjects counts LFS objects stored in the cache: 0 when it holds
// none, -1 when the store cannot be read.
func countLFSObjects(dir string) int {
	root := filepath.Join(dir, "lfs", "objects")
	if _, err := os.Stat(root); errors.Is(err, fs.ErrNotExist) {
		return 0
	}
	oids, err := walkLFSObjects(root)
	if err != nil {
		return -1
	}
	return len(oids)
}

// lfsObjectIDs returns the ids of the LFS objects stored in the cache.
func lfsObjectIDs(dir string) []string {
	oids, _ := walkLFSObjects(filepath.Join(dir, "lfs", "objects"))
	return oids
}

func walkLFSObjects(root string) ([]string, error) {
	var oids []string
	err := filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && isOID(d.Name()) {
			oids = append(oids, d.Name())
		}
		return nil
	})
	return oids, err
}

// isOID reports whether s is a lowercase sha256 hex id, the only form of
// name git-lfs gives stored objects.
func isOID(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
