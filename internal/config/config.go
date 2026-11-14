// Package config loads, interpolates and validates the gitmirror YAML
// configuration.
//
// Loading is a pipeline: strict YAML decode (unknown keys are errors),
// ${VAR} interpolation of decoded string values (so an environment value can
// never inject YAML structure), defaults and normalisation, secret resolution
// (token / token_file) and finally Validate, which reports every problem at
// once. Secrets are never included in error messages and never printed by
// the Stringer / slog.LogValuer implementations.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/goccy/go-yaml"
)

// KnownForgeTypes lists the forge types accepted in forges.*.type. It is a
// variable so that provider packages can extend it.
var KnownForgeTypes = []string{"github", "gitea"}

// Include feature names accepted in `include`.
const (
	FeatureGit      = "git"
	FeatureLFS      = "lfs"
	FeatureSettings = "settings"
	FeatureReleases = "releases"
	FeatureWiki     = "wiki"
)

// allFeatures is the default include list, in canonical order.
var allFeatures = []string{FeatureGit, FeatureLFS, FeatureSettings, FeatureReleases, FeatureWiki}

const (
	defaultStateDir    = "./data"
	defaultConcurrency = 4
	defaultListen      = ":8080"
	defaultInterval    = "1h"
	minInterval        = time.Minute
	defaultGrace       = "24h"
	minGrace           = time.Hour
	redacted           = "[REDACTED]"
)

// Config is the whole configuration file.
type Config struct {
	// StateDir holds the sqlite database and the git cache. Default "./data".
	StateDir string `yaml:"state_dir"`
	// Concurrency is the number of rules/repos processed in parallel, 1..64.
	// Default 4 (an omitted or zero value selects the default).
	Concurrency int `yaml:"concurrency"`
	// Forges maps a forge name (referenced by rules) to its connection settings.
	Forges map[string]Forge `yaml:"forges"`
	// Defaults are the per-rule settings used when a rule does not override them.
	Defaults Defaults `yaml:"defaults"`
	// Server configures the HTTP listener / webhook receiver.
	Server Server `yaml:"server"`
	// Git configures how git child processes are run.
	Git Git `yaml:"git"`
	// Rules are the one-way mirror rules. At least one is required.
	Rules []Rule `yaml:"rules"`
}

// Forge describes one forge instance. After Parse, Token always holds the
// resolved secret (read from TokenFile when that was used).
type Forge struct {
	// Type is the lowercase provider type, one of KnownForgeTypes.
	Type string `yaml:"type"`
	// URL is the base URL (required for gitea, optional for github).
	// Trailing slashes are trimmed. No default is inserted.
	URL string `yaml:"url"`
	// Token is the API token. Exactly one of Token / TokenFile must be set in the file.
	Token string `yaml:"token"`
	// TokenFile is a path whose trimmed contents are the token.
	TokenFile string `yaml:"token_file"`
	// Username is the account name used for git-over-HTTP authentication.
	Username string `yaml:"username"`
}

// String implements fmt.Stringer without exposing the token.
func (f Forge) String() string {
	return fmt.Sprintf("{type=%s url=%s username=%s token=%s token_file=%s}",
		f.Type, f.URL, f.Username, maskIf(f.Token), f.TokenFile)
}

// GoString implements fmt.GoStringer (%#v) without exposing the token.
func (f Forge) GoString() string { return "config.Forge" + f.String() }

// LogValue implements slog.LogValuer without exposing the token.
func (f Forge) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("type", f.Type),
		slog.String("url", f.URL),
		slog.String("username", f.Username),
		slog.String("token", maskIf(f.Token)),
		slog.String("token_file", f.TokenFile),
	)
}

// Defaults are settings every rule inherits unless it overrides them.
type Defaults struct {
	// Interval is a Go duration string, minimum 1m. Default "1h".
	Interval string `yaml:"interval"`
	// Include lists what to mirror (git, lfs, settings, releases, wiki). Default: all five.
	Include []string `yaml:"include"`
	// Visibility of created destination repos: "private" (default) or "match-source".
	Visibility string `yaml:"visibility"`
	// OnSourceDeleted is "keep" (default), "archive" or "delete".
	OnSourceDeleted string `yaml:"on_source_deleted"`
	// DeletionGrace is a Go duration string, minimum 1h, default "24h": how
	// long a source must keep being reported as not found before
	// OnSourceDeleted is applied. A forge can report a repository as not
	// found when the token merely lost access to it.
	DeletionGrace string `yaml:"deletion_grace"`
	// DeleteMissingReleases removes destination releases that no longer exist at the source.
	DeleteMissingReleases bool `yaml:"delete_missing_releases"`
	// MaxRefDeletions and MaxRefDeletionsPercent form the mass ref deletion
	// guard: a sync that would delete more than MaxRefDeletions branches/tags
	// AND more than MaxRefDeletionsPercent percent of the destination's
	// branches/tags is aborted before anything is pushed (sync --allow-prune
	// overrides it for one run). Defaults 5 and 50; after Parse both are
	// non-nil. Count >= 0, percent 1..100.
	MaxRefDeletions        *int `yaml:"max_ref_deletions"`
	MaxRefDeletionsPercent *int `yaml:"max_ref_deletions_percent"`
}

// Git configures how gitmirror runs git and git-lfs.
type Git struct {
	// UseHostConfig lets git read the host's system and global git
	// configuration (/etc/gitconfig, ~/.gitconfig, XDG config). Default
	// false: git runs isolated from them, so host settings such as
	// url.<x>.insteadOf, http.extraHeader or core.hooksPath cannot redirect
	// or alter mirroring. Proxies then come from HTTPS_PROXY / HTTP_PROXY /
	// NO_PROXY and a custom CA bundle from GIT_SSL_CAINFO.
	UseHostConfig bool `yaml:"use_host_config"`
}

// Server configures the webhook/HTTP server.
type Server struct {
	// Listen is the listen address. Default ":8080".
	Listen string `yaml:"listen"`
	// WebhookSecret is the resolved shared secret; empty means webhooks are
	// not configured (the server package must then refuse webhooks).
	WebhookSecret string `yaml:"webhook_secret"`
	// WebhookSecretFile is a path whose trimmed contents are the secret.
	WebhookSecretFile string `yaml:"webhook_secret_file"`
	// MetricsListen is where /metrics is served, on its own listener (never
	// the main one). Default "127.0.0.1:9090"; "off" disables metrics.
	MetricsListen string `yaml:"metrics_listen"`
}

// String implements fmt.Stringer without exposing the webhook secret.
func (s Server) String() string {
	return fmt.Sprintf("{listen=%s webhook_secret=%s webhook_secret_file=%s}",
		s.Listen, maskIf(s.WebhookSecret), s.WebhookSecretFile)
}

// GoString implements fmt.GoStringer without exposing the webhook secret.
func (s Server) GoString() string { return "config.Server" + s.String() }

// LogValue implements slog.LogValuer without exposing the webhook secret.
func (s Server) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("listen", s.Listen),
		slog.String("webhook_secret", maskIf(s.WebhookSecret)),
		slog.String("webhook_secret_file", s.WebhookSecretFile),
	)
}

// Rule is one one-way mirror rule. The optional fields override Defaults.
type Rule struct {
	// Name is the unique rule name (same charset as forge names).
	Name string `yaml:"name"`
	// From selects the source repositories.
	From Source `yaml:"from"`
	// To describes where they are mirrored to.
	To Dest `yaml:"to"`

	// Interval overrides Defaults.Interval when non-empty.
	Interval string `yaml:"interval"`
	// Include overrides Defaults.Include when non-nil.
	Include []string `yaml:"include"`
	// Visibility overrides Defaults.Visibility when non-empty.
	Visibility string `yaml:"visibility"`
	// OnSourceDeleted overrides Defaults.OnSourceDeleted when non-empty.
	OnSourceDeleted string `yaml:"on_source_deleted"`
	// DeletionGrace overrides Defaults.DeletionGrace when non-empty.
	DeletionGrace string `yaml:"deletion_grace"`
	// DeleteMissingReleases overrides Defaults.DeleteMissingReleases when non-nil.
	DeleteMissingReleases *bool `yaml:"delete_missing_releases"`
	// MaxRefDeletions overrides Defaults.MaxRefDeletions when non-nil.
	MaxRefDeletions *int `yaml:"max_ref_deletions"`
	// MaxRefDeletionsPercent overrides Defaults.MaxRefDeletionsPercent when non-nil.
	MaxRefDeletionsPercent *int `yaml:"max_ref_deletions_percent"`
	// Adopt allows writing to a destination repo this tool did not create.
	Adopt bool `yaml:"adopt"`
}

// Source selects source repositories: exactly one of Owner, StarredBy, Repo.
type Source struct {
	// Forge names an entry of Config.Forges.
	Forge string `yaml:"forge"`
	// Owner selects every repo of a user/org.
	Owner string `yaml:"owner"`
	// StarredBy selects every repo starred by a user.
	StarredBy string `yaml:"starred_by"`
	// Repo selects a single repo, as "owner/name".
	Repo string `yaml:"repo"`
	// Filter narrows Owner / StarredBy selections. After Parse it is non-nil
	// (with defaults filled) for those selections and nil for Repo.
	Filter *Filter `yaml:"filter"`
}

// Filter narrows a multi-repo source selection.
type Filter struct {
	// Forks: include forks (default false).
	Forks *bool `yaml:"forks"`
	// Archived: include archived repos (default true).
	Archived *bool `yaml:"archived"`
	// Private: include private repos (default true).
	Private *bool `yaml:"private"`
	// Include is a list of path.Match patterns on the repo name (default ["*"]).
	Include []string `yaml:"include"`
	// Exclude is a list of path.Match patterns on the repo name.
	Exclude []string `yaml:"exclude"`
}

// Dest describes a destination: exactly one of Owner (with optional Name
// template) or Repo (literal "owner/name"). Owner and Name are text/template
// strings rendered against the source repo's {Owner, Name}; see Render.
//
// Validate can only check literal (Repo) destinations for collisions, loops
// and self-mirroring; templated destinations can collide at runtime and must
// be checked then.
type Dest struct {
	// Forge names an entry of Config.Forges.
	Forge string `yaml:"forge"`
	// Owner is the destination owner (template).
	Owner string `yaml:"owner"`
	// Name is the destination repo name template. Default "{{.Name}}".
	Name string `yaml:"name"`
	// Repo is a literal "owner/name" destination.
	Repo string `yaml:"repo"`
}

// RuleSettings is a rule's resolved per-rule view (rule overrides applied over defaults).
type RuleSettings struct {
	Interval              time.Duration
	Include               []string
	Visibility            string
	OnSourceDeleted       string
	DeletionGrace         time.Duration // how long a source must stay not found before OnSourceDeleted applies
	DeleteMissingReleases bool
	Adopt                 bool
	// MaxRefDeletions and MaxRefDeletionsPercent are the mass ref deletion
	// guard (see Defaults). A zero MaxRefDeletionsPercent (a RuleSettings not
	// built by Effective) means the defaults 5 and 50.
	MaxRefDeletions        int
	MaxRefDeletionsPercent int
}

// Defaults of the mass ref deletion guard.
const (
	DefaultMaxRefDeletions        = 5
	DefaultMaxRefDeletionsPercent = 50
)

// Includes reports whether feature (e.g. "releases") is enabled.
func (s RuleSettings) Includes(feature string) bool {
	for _, f := range s.Include {
		if f == feature {
			return true
		}
	}
	return false
}

// Effective returns the rule's settings with its overrides applied over d.
// It assumes a validated config; an unparsable interval or deletion grace
// yields zero.
func (r Rule) Effective(d Defaults) RuleSettings {
	s := RuleSettings{
		Include:               append([]string(nil), d.Include...),
		Visibility:            d.Visibility,
		OnSourceDeleted:       d.OnSourceDeleted,
		DeleteMissingReleases: d.DeleteMissingReleases,
		Adopt:                 r.Adopt,
	}
	iv := d.Interval
	if r.Interval != "" {
		iv = r.Interval
	}
	s.Interval, _ = time.ParseDuration(iv)
	grace := d.DeletionGrace
	if r.DeletionGrace != "" {
		grace = r.DeletionGrace
	}
	s.DeletionGrace, _ = time.ParseDuration(grace)
	if r.Include != nil {
		s.Include = append([]string(nil), r.Include...)
	}
	if r.Visibility != "" {
		s.Visibility = r.Visibility
	}
	if r.OnSourceDeleted != "" {
		s.OnSourceDeleted = r.OnSourceDeleted
	}
	if r.DeleteMissingReleases != nil {
		s.DeleteMissingReleases = *r.DeleteMissingReleases
	}
	s.MaxRefDeletions = intOr(r.MaxRefDeletions, intOr(d.MaxRefDeletions, DefaultMaxRefDeletions))
	s.MaxRefDeletionsPercent = intOr(r.MaxRefDeletionsPercent, intOr(d.MaxRefDeletionsPercent, DefaultMaxRefDeletionsPercent))
	return s
}

func intOr(p *int, def int) int {
	if p != nil {
		return *p
	}
	return def
}

type plainConfig Config // no methods: avoids String recursion

// String implements fmt.Stringer; secrets are redacted.
func (c Config) String() string { return fmt.Sprintf("%+v", plainConfig(c.Redacted())) }

// Redacted returns a copy of c with every secret replaced by a placeholder.
// The original is not modified.
func (c Config) Redacted() Config {
	out := c
	out.Forges = make(map[string]Forge, len(c.Forges))
	for k, f := range c.Forges {
		f.Token = maskIf(f.Token)
		out.Forges[k] = f
	}
	out.Server.WebhookSecret = maskIf(c.Server.WebhookSecret)
	return out
}

func maskIf(s string) string {
	if s == "" {
		return ""
	}
	return redacted
}

// Load reads and parses the config file at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	return Parse(data, os.LookupEnv, os.ReadFile)
}

// Parse decodes, interpolates, resolves secrets for and validates a config.
// env and readFile are injected for testability. All validation problems are
// returned together (errors.Join).
func Parse(data []byte, env func(string) (string, bool), readFile func(string) ([]byte, error)) (*Config, error) {
	var c Config
	if err := yaml.UnmarshalWithOptions(data, &c, yaml.Strict()); err != nil {
		return nil, decodeError(err)
	}
	raw := rawSecrets(&c)
	if errs := interpolate(&c, env); len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	if errs := emptyInterpolated(&c, raw); len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	// GITMIRROR_STATE_DIR (when set and non-empty) overrides state_dir;
	// precedence is env > config file > default "./data".
	if v, ok := env(stateDirEnv); ok && v != "" {
		c.StateDir = v
	}
	c.applyDefaults()
	errs := c.resolveSecrets(readFile)
	if err := c.Validate(); err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return &c, nil
}

// stateDirEnv is the environment variable that overrides state_dir.
const stateDirEnv = "GITMIRROR_STATE_DIR"

var varRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)`)

func secretFields(c *Config) map[string]string {
	m := map[string]string{"server.webhook_secret": c.Server.WebhookSecret}
	for name, f := range c.Forges {
		m["forges."+name+".token"] = f.Token
	}
	return m
}

// rawSecrets returns the pre-interpolation secret values keyed by field path.
func rawSecrets(c *Config) map[string]string { return secretFields(c) }

// emptyInterpolated reports secrets that referenced ${VAR} but resolved to an
// empty or whitespace-only value. Only the variable name is reported.
func emptyInterpolated(c *Config, raw map[string]string) []error {
	cur := secretFields(c)
	var errs []error
	for path, orig := range raw {
		m := varRef.FindStringSubmatch(strings.ReplaceAll(orig, "$${", ""))
		if m == nil || strings.TrimSpace(cur[path]) != "" {
			continue
		}
		errs = append(errs, fmt.Errorf("%s: ${%s} resolved to an empty value", path, m[1]))
	}
	sort.Slice(errs, func(i, j int) bool { return errs[i].Error() < errs[j].Error() })
	return errs
}

// decodeError reduces a YAML error to its message and position; the
// library's default rendering quotes source lines, which could contain secrets.
func decodeError(err error) error {
	var ye yaml.Error
	if errors.As(err, &ye) {
		msg := ye.GetMessage()
		if tok := ye.GetToken(); tok != nil && tok.Position != nil {
			return fmt.Errorf("config: line %d, column %d: %s", tok.Position.Line, tok.Position.Column, msg)
		}
		return fmt.Errorf("config: %s", msg)
	}
	return errors.New("config: invalid YAML")
}

// applyDefaults fills defaults and normalises values. It never fails.
func (c *Config) applyDefaults() {
	if c.StateDir == "" {
		c.StateDir = defaultStateDir
	}
	if c.Concurrency == 0 {
		c.Concurrency = defaultConcurrency
	}
	if c.Server.Listen == "" {
		c.Server.Listen = defaultListen
	}
	c.Server.MetricsListen = strings.TrimSpace(c.Server.MetricsListen)
	switch {
	case c.Server.MetricsListen == "":
		c.Server.MetricsListen = "127.0.0.1:9090"
	case strings.EqualFold(c.Server.MetricsListen, "off"):
		c.Server.MetricsListen = "off"
	}
	for name, f := range c.Forges {
		f.Type = strings.ToLower(strings.TrimSpace(f.Type))
		f.URL = strings.TrimRight(strings.TrimSpace(f.URL), "/")
		c.Forges[name] = f
	}
	d := &c.Defaults
	if d.Interval == "" {
		d.Interval = defaultInterval
	}
	if d.Include == nil {
		d.Include = append([]string(nil), allFeatures...)
	}
	d.Include = dedupe(d.Include)
	if d.Visibility == "" {
		d.Visibility = "private"
	}
	if d.OnSourceDeleted == "" {
		d.OnSourceDeleted = "keep"
	}
	if d.DeletionGrace == "" {
		d.DeletionGrace = defaultGrace
	}
	if d.MaxRefDeletions == nil {
		n := DefaultMaxRefDeletions
		d.MaxRefDeletions = &n
	}
	if d.MaxRefDeletionsPercent == nil {
		n := DefaultMaxRefDeletionsPercent
		d.MaxRefDeletionsPercent = &n
	}
	for i := range c.Rules {
		r := &c.Rules[i]
		if r.Include != nil {
			r.Include = dedupe(r.Include)
		}
		if r.To.Repo == "" && r.To.Name == "" {
			r.To.Name = "{{.Name}}"
		}
		if r.From.Repo == "" {
			if r.From.Filter == nil {
				r.From.Filter = &Filter{}
			}
			f := r.From.Filter
			f.Forks = boolDefault(f.Forks, false)
			f.Archived = boolDefault(f.Archived, true)
			f.Private = boolDefault(f.Private, true)
			if f.Include == nil {
				f.Include = []string{"*"}
			}
		}
	}
}

func boolDefault(p *bool, def bool) *bool {
	if p != nil {
		return p
	}
	return &def
}

func dedupe(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// resolveSecrets reads *_file secrets into their value fields. Errors never
// contain secret values.
func (c *Config) resolveSecrets(readFile func(string) ([]byte, error)) []error {
	var errs []error
	for name, f := range c.Forges {
		loc := "forges." + name
		switch {
		case f.Token != "" && f.TokenFile != "":
			errs = append(errs, fmt.Errorf("%s: only one of token and token_file may be set", loc))
		case f.TokenFile != "":
			v, err := readSecret(readFile, f.TokenFile)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s.token_file: %w", loc, err))
				break
			}
			f.Token = v
			c.Forges[name] = f
		}
	}
	s := &c.Server
	switch {
	case s.WebhookSecret != "" && s.WebhookSecretFile != "":
		errs = append(errs, errors.New("server: only one of webhook_secret and webhook_secret_file may be set"))
	case s.WebhookSecretFile != "":
		v, err := readSecret(readFile, s.WebhookSecretFile)
		if err != nil {
			errs = append(errs, fmt.Errorf("server.webhook_secret_file: %w", err))
			break
		}
		s.WebhookSecret = v
	}
	return errs
}

func readSecret(readFile func(string) ([]byte, error), path string) (string, error) {
	b, err := readFile(path)
	if err != nil {
		return "", err
	}
	v := string(bytes.TrimSpace(b))
	if v == "" {
		return "", fmt.Errorf("file %q is empty", path)
	}
	return v, nil
}
