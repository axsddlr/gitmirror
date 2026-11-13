// Package gitx is gitmirror's git transport. It shells out to the system
// git and git-lfs binaries to copy every branch and tag (and optionally LFS
// objects) from a source remote to a destination remote through a
// persistent local bare cache repository, so repeated syncs transfer deltas.
//
// Credentials never reach process argv, URLs, files on disk or returned
// errors: they are injected per command as an origin-scoped
// http.<origin>.extraHeader via GIT_CONFIG_COUNT/KEY/VALUE environment
// variables, and all git output is scrubbed before it is returned.
package gitx

import (
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
)

// ErrUnsafeURL is returned (wrapped) when a remote URL is not an http://,
// https:// or file:// URL, embeds credentials, or could be parsed by git as
// an option or transport helper. No git process is started in that case.
var ErrUnsafeURL = errors.New("gitx: unsafe remote URL")

// defaultUsername is used for basic auth when Remote.Username is empty; it is
// accepted by GitHub and ignored by forges that only look at the password.
const defaultUsername = "x-access-token"

// Remote identifies a git remote plus the credentials used to reach it.
// Token may be empty for public or file:// remotes. Formatting a Remote with
// fmt or slog only ever shows the URL with credentials and query removed.
type Remote struct {
	URL      string
	Username string
	Token    string
}

// String returns the remote URL with any userinfo, query and fragment
// removed. It never includes Username or Token.
func (r Remote) String() string { return redactURL(r.URL) }

// GoString implements fmt.GoStringer so %#v cannot print the token.
func (r Remote) GoString() string { return fmt.Sprintf("gitx.Remote{URL:%q}", r.String()) }

// LogValue implements slog.LogValuer, logging only the redacted URL.
func (r Remote) LogValue() slog.Value { return slog.StringValue(r.String()) }

func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<invalid URL>"
	}
	u.User = nil
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment = ""
	u.RawFragment = ""
	return u.String()
}

// validate checks the URL is safe to hand to git.
func (r Remote) validate() error {
	_, err := r.parse()
	return err
}

func (r Remote) parse() (*url.URL, error) {
	raw := r.URL
	if raw == "" {
		return nil, fmt.Errorf("%w: empty URL", ErrUnsafeURL)
	}
	if strings.HasPrefix(raw, "-") {
		return nil, fmt.Errorf("%w: URL starts with '-'", ErrUnsafeURL)
	}
	for _, c := range raw {
		if c <= ' ' || c == 0x7f {
			return nil, fmt.Errorf("%w: URL contains whitespace or control characters", ErrUnsafeURL)
		}
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: unparseable URL", ErrUnsafeURL)
	}
	if u.User != nil {
		return nil, fmt.Errorf("%w: credentials embedded in URL %s; use Remote.Token", ErrUnsafeURL, redactURL(raw))
	}
	if u.Opaque != "" {
		return nil, fmt.Errorf("%w: %s is not a hierarchical URL", ErrUnsafeURL, redactURL(raw))
	}
	switch strings.ToLower(u.Scheme) {
	case "https", "http":
		if u.Hostname() == "" {
			return nil, fmt.Errorf("%w: %s has no host", ErrUnsafeURL, redactURL(raw))
		}
	case "file":
		if u.Host != "" {
			return nil, fmt.Errorf("%w: file URL %s must not have a host", ErrUnsafeURL, redactURL(raw))
		}
		if u.Path == "" || u.Path == "/" {
			return nil, fmt.Errorf("%w: file URL %s has no path", ErrUnsafeURL, redactURL(raw))
		}
	default:
		return nil, fmt.Errorf("%w: scheme of %s not in https, http, file", ErrUnsafeURL, redactURL(raw))
	}
	return u, nil
}

// origin returns "scheme://host[:port]/" for http(s) remotes, or "" when no
// auth header applies (file:// remotes).
func (r Remote) origin() string {
	u, err := r.parse()
	if err != nil {
		return ""
	}
	s := strings.ToLower(u.Scheme)
	if s != "https" && s != "http" {
		return ""
	}
	return s + "://" + strings.ToLower(u.Host) + "/"
}

func (r Remote) username() string {
	if r.Username == "" {
		return defaultUsername
	}
	return r.Username
}

// basicAuth returns base64(user:token).
func (r Remote) basicAuth() string {
	return base64.StdEncoding.EncodeToString([]byte(r.username() + ":" + r.Token))
}

// secrets lists every form of the credential that must be scrubbed.
func (r Remote) secrets() []string {
	if r.Token == "" {
		return nil
	}
	out := []string{r.Token, r.basicAuth(), url.QueryEscape(r.Token), url.PathEscape(r.Token)}
	out = append(out, base64.RawStdEncoding.EncodeToString([]byte(r.username()+":"+r.Token)))
	return out
}

// scrub replaces every secret of the given remotes in s with "***".
// Longer secrets are replaced first so overlapping forms are fully removed.
func scrub(s string, rems ...Remote) string {
	var all []string
	for _, r := range rems {
		all = append(all, r.secrets()...)
	}
	// Sort by length descending (simple insertion sort; lists are tiny).
	for i := 1; i < len(all); i++ {
		for j := i; j > 0 && len(all[j]) > len(all[j-1]); j-- {
			all[j], all[j-1] = all[j-1], all[j]
		}
	}
	for _, sec := range all {
		if sec != "" {
			s = strings.ReplaceAll(s, sec, "***")
		}
	}
	return s
}

// scrubbedError carries a scrubbed message while keeping errors.Is/As
// working against the wrapped chain.
type scrubbedError struct {
	msg string
	err error
}

func (e *scrubbedError) Error() string { return e.msg }
func (e *scrubbedError) Unwrap() error { return e.err }

func scrubErr(err error, rems ...Remote) error {
	if err == nil {
		return nil
	}
	return &scrubbedError{msg: scrub(err.Error(), rems...), err: err}
}
