package gitx

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	defaultTimeout = 30 * time.Minute
	maxStderr      = 64 << 10
	minGitMajor    = 2
	minGitMinor    = 30
)

// Runner executes git commands with a hardened environment.
// The zero value is usable.
type Runner struct {
	// Git is the git executable; default "git" (resolved via PATH).
	Git string
	// Timeout bounds each individual git command; default 30 minutes.
	Timeout time.Duration
	// Env is appended to the sanitized child environment (mainly for tests,
	// e.g. HOME or GIT_CONFIG_NOSYSTEM). Safety variables still win.
	Env []string

	// hook, if set, observes every command just before it starts (tests).
	hook func(*exec.Cmd)
}

// Versions reports the versions of the external tools Runner depends on.
type Versions struct {
	// Git is the git version, e.g. "2.47.1.windows.1".
	Git string
	// LFS is the git-lfs version, e.g. "3.6.0", or "" if git-lfs is absent.
	LFS string
}

// Ref is a single git reference and the object it points to. For annotated
// tags Hash is the tag object, not the peeled commit.
type Ref struct {
	Name string
	Hash string
}

func (r *Runner) gitPath() string {
	if r == nil || r.Git == "" {
		return "git"
	}
	return r.Git
}

func (r *Runner) timeout() time.Duration {
	if r == nil || r.Timeout <= 0 {
		return defaultTimeout
	}
	return r.Timeout
}

// spec describes one git invocation.
type spec struct {
	gitDir string   // passed as --git-dir and used as working directory
	auth   []Remote // remotes whose credentials this command may use
	args   []string // subcommand and its arguments
}

// cmdError is a git failure whose stderr has already been scrubbed.
type cmdError struct {
	op     string
	err    error
	stderr string
}

func (e *cmdError) Error() string {
	msg := "git " + e.op + ": " + e.err.Error()
	if e.stderr != "" {
		msg += ": " + e.stderr
	}
	return msg
}

func (e *cmdError) Unwrap() error { return e.err }

// boundedBuffer keeps at most max bytes, trimming back to the last newline
// on overflow so a secret is never left half-cut (and thus unscrubbable).
type boundedBuffer struct {
	buf       bytes.Buffer
	max       int
	truncated bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.buf.Len(); room > 0 {
		if len(p) > room {
			b.buf.Write(p[:room])
			b.truncated = true
		} else {
			b.buf.Write(p)
		}
	} else if len(p) > 0 {
		b.truncated = true
	}
	return len(p), nil
}

func (b *boundedBuffer) String() string {
	s := b.buf.String()
	if b.truncated {
		if i := strings.LastIndexByte(s, '\n'); i >= 0 {
			s = s[:i]
		} else {
			s = ""
		}
		s += "\n[stderr truncated]"
	}
	return s
}

// run executes git and returns its scrubbed stdout. On failure the error is
// a *cmdError with scrubbed stderr; stdout is still returned.
func (r *Runner) run(ctx context.Context, s spec) (string, error) {
	env, err := buildEnv(os.Environ(), r.envExtra(), s.auth...)
	if err != nil {
		return "", err
	}
	argv := []string{"-c", "credential.helper="}
	if s.gitDir != "" {
		argv = append(argv, "--git-dir="+s.gitDir)
	}
	argv = append(argv, s.args...)

	ctx, cancel := context.WithTimeout(ctx, r.timeout())
	defer cancel()
	cmd := exec.CommandContext(ctx, r.gitPath(), argv...)
	cmd.Env = env
	cmd.Stdin = nil
	if s.gitDir != "" {
		cmd.Dir = s.gitDir
	} else {
		// Avoid picking up config from whatever repository the process
		// happens to be running in.
		cmd.Dir = os.TempDir()
	}
	// Children of git (remote helpers) may hold pipes open after git is
	// killed; don't block forever waiting for them.
	cmd.WaitDelay = 10 * time.Second
	var stdout bytes.Buffer
	stderr := &boundedBuffer{max: maxStderr}
	cmd.Stdout = &stdout
	cmd.Stderr = stderr
	if r != nil && r.hook != nil {
		r.hook(cmd)
	}

	err = cmd.Run()
	out := scrub(stdout.String(), s.auth...)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			err = fmt.Errorf("%w (%v)", ctxErr, err)
		}
		return out, &cmdError{op: opName(s.args), err: err, stderr: strings.TrimSpace(scrub(stderr.String(), s.auth...))}
	}
	return out, nil
}

func (r *Runner) envExtra() []string {
	if r == nil {
		return nil
	}
	return r.Env
}

func opName(args []string) string {
	if len(args) == 0 {
		return ""
	}
	if args[0] == "lfs" && len(args) > 1 {
		return "lfs " + args[1]
	}
	return args[0]
}

// Check verifies git is usable and at least version 2.30, and reports the
// git-lfs version ("" if git-lfs is not installed).
func (r *Runner) Check(ctx context.Context) (Versions, error) {
	var v Versions
	out, err := r.run(ctx, spec{args: []string{"version"}})
	if err != nil {
		return v, fmt.Errorf("gitx: git not usable: %w", err)
	}
	v.Git = strings.TrimPrefix(strings.TrimSpace(out), "git version ")
	if err := checkGitVersion(v.Git); err != nil {
		return v, err
	}
	v.LFS = r.lfsVersion(ctx)
	return v, nil
}

func checkGitVersion(s string) error {
	parts := strings.SplitN(s, ".", 3)
	if len(parts) < 2 {
		return fmt.Errorf("gitx: cannot parse git version %q", s)
	}
	major, err1 := strconv.Atoi(parts[0])
	minor, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return fmt.Errorf("gitx: cannot parse git version %q", s)
	}
	if major < minGitMajor || (major == minGitMajor && minor < minGitMinor) {
		return fmt.Errorf("gitx: git %s is too old; need >= %d.%d", s, minGitMajor, minGitMinor)
	}
	return nil
}

// lfsVersion returns the git-lfs version or "" if it cannot be run.
func (r *Runner) lfsVersion(ctx context.Context) string {
	out, err := r.run(ctx, spec{args: []string{"lfs", "version"}})
	if err != nil {
		return ""
	}
	out = strings.TrimSpace(out)
	if !strings.HasPrefix(out, "git-lfs/") {
		return ""
	}
	v, _, _ := strings.Cut(strings.TrimPrefix(out, "git-lfs/"), " ")
	return v
}

// LsRemote lists the remote's refs/heads/* and refs/tags/* (peeled "^{}"
// entries, HEAD and other namespaces such as refs/pull/* are dropped),
// sorted by name. An empty repository yields an empty slice and no error.
func (r *Runner) LsRemote(ctx context.Context, rem Remote) ([]Ref, error) {
	if err := rem.validate(); err != nil {
		return nil, err
	}
	out, err := r.run(ctx, spec{auth: []Remote{rem}, args: []string{"ls-remote", "--refs", "--", rem.URL}})
	if err != nil {
		return nil, scrubErr(fmt.Errorf("gitx: ls-remote %s: %w", rem, err), rem)
	}
	return parseRefs(out), nil
}

func parseRefs(out string) []Ref {
	var refs []Ref
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		hash, name, ok := strings.Cut(line, "\t")
		if !ok {
			hash, name, ok = strings.Cut(line, " ")
		}
		if !ok || hash == "" || strings.HasSuffix(name, "^{}") {
			continue
		}
		if !strings.HasPrefix(name, "refs/heads/") && !strings.HasPrefix(name, "refs/tags/") {
			continue
		}
		refs = append(refs, Ref{Name: name, Hash: hash})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Name < refs[j].Name })
	return refs
}

// Digest returns a sha256 hex digest over the refs as sorted "hash name\n"
// lines, independent of input order. An empty or nil slice yields "" so an
// empty repository is distinguishable from any non-empty one.
func Digest(refs []Ref) string {
	if len(refs) == 0 {
		return ""
	}
	lines := make([]string, len(refs))
	for i, r := range refs {
		lines[i] = r.Hash + " " + r.Name + "\n"
	}
	sort.Strings(lines)
	h := sha256.New()
	for _, l := range lines {
		h.Write([]byte(l))
	}
	return hex.EncodeToString(h.Sum(nil))
}
