package config

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"
	"text/template"
	"time"
)

var (
	identRe    = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
	repoNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
)

// RepoRef is the template data for Dest names: the source repository.
type RepoRef struct {
	Owner string
	Name  string
}

// Validate checks the whole config and returns every problem at once
// (errors.Join), each prefixed with its location. Parse calls it.
//
// Safety checks (self-mirroring, duplicate destinations, mirror loops) only
// apply to literal `repo:` destinations; templated destinations can only be
// checked at runtime.
func (c *Config) Validate() error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if c.Concurrency < 1 || c.Concurrency > 64 {
		add("concurrency: %d is out of range 1..64", c.Concurrency)
	}

	names := make([]string, 0, len(c.Forges))
	for n := range c.Forges {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		c.validateForge(n, c.Forges[n], add)
	}

	validateSettings("defaults", c.Defaults.Interval, c.Defaults.Include, c.Defaults.Visibility, c.Defaults.OnSourceDeleted, c.Defaults.DeletionGrace, true, add)

	if len(c.Rules) == 0 {
		add("rules: at least one rule is required")
	}
	seen := map[string]int{}
	for i, r := range c.Rules {
		loc := fmt.Sprintf("rules[%d]", i)
		if r.Name != "" {
			loc += " (" + r.Name + ")"
		}
		addr := func(format string, a ...any) { add("%s: %s", loc, fmt.Sprintf(format, a...)) }
		c.validateRule(r, seen, i, addr)
	}
	c.validateSafety(add)
	return errors.Join(errs...)
}

func (c *Config) validateForge(name string, f Forge, add func(string, ...any)) {
	loc := "forges." + name
	if !identRe.MatchString(name) {
		add("%s: invalid forge name (must match %s)", loc, identRe)
	}
	switch {
	case f.Type == "":
		add("%s: type is required", loc)
	case !slices.Contains(KnownForgeTypes, f.Type):
		add("%s: unknown type %q (known: %s)", loc, f.Type, strings.Join(KnownForgeTypes, ", "))
	}
	if f.URL == "" {
		if f.Type == "gitea" {
			add("%s: url is required for gitea", loc)
		}
	} else if u, err := url.Parse(f.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		add("%s: url must be an absolute http or https URL", loc)
	}
	if f.Token == "" && f.TokenFile == "" {
		add("%s: one of token or token_file is required", loc)
	}
}

func validateSettings(loc, interval string, include []string, visibility, onDeleted, grace string, required bool, add func(string, ...any)) {
	if interval != "" || required {
		if d, err := time.ParseDuration(interval); err != nil {
			add("%s: interval %q is not a valid duration", loc, interval)
		} else if d < minInterval {
			add("%s: interval %s is below the minimum of %s", loc, interval, minInterval)
		}
	}
	if include != nil || required {
		validateInclude(loc, include, add)
	}
	if visibility != "" || required {
		if visibility != "private" && visibility != "match-source" {
			add("%s: visibility %q must be private or match-source", loc, visibility)
		}
	}
	if onDeleted != "" || required {
		if onDeleted != "keep" && onDeleted != "archive" && onDeleted != "delete" {
			add("%s: on_source_deleted %q must be keep, archive or delete", loc, onDeleted)
		}
	}
	if grace != "" || required {
		if d, err := time.ParseDuration(grace); err != nil {
			add("%s: deletion_grace %q is not a valid duration", loc, grace)
		} else if d < minGrace {
			add("%s: deletion_grace %s is below the minimum of %s", loc, grace, minGrace)
		}
	}
}

func validateInclude(loc string, include []string, add func(string, ...any)) {
	if len(include) == 0 {
		add("%s: include: must not be empty", loc)
		return
	}
	has := map[string]bool{}
	for _, f := range include {
		if !slices.Contains(allFeatures, f) {
			add("%s: include: unknown entry %q (known: %s)", loc, f, strings.Join(allFeatures, ", "))
			continue
		}
		has[f] = true
	}
	for _, f := range []string{FeatureLFS, FeatureReleases, FeatureWiki} {
		if has[f] && !has[FeatureGit] {
			add("%s: include: %q requires git", loc, f)
		}
	}
}

func (c *Config) validateRule(r Rule, seen map[string]int, idx int, add func(string, ...any)) {
	switch {
	case r.Name == "":
		add("name is required")
	case !identRe.MatchString(r.Name):
		add("invalid rule name (must match %s)", identRe)
	default:
		if prev, dup := seen[r.Name]; dup {
			add("duplicate rule name (also rules[%d])", prev)
		} else {
			seen[r.Name] = idx
		}
	}

	// Source.
	if _, ok := c.Forges[r.From.Forge]; !ok {
		add("from.forge %q is not defined", r.From.Forge)
	}
	n := 0
	for _, s := range []string{r.From.Owner, r.From.StarredBy, r.From.Repo} {
		if s != "" {
			n++
		}
	}
	if n != 1 {
		add("from: exactly one of owner, starred_by, repo must be set")
	}
	if r.From.Repo != "" {
		if _, _, ok := splitRepo(r.From.Repo); !ok {
			add("from.repo %q must be owner/name", r.From.Repo)
		}
		if r.From.Filter != nil {
			add("from.filter is only allowed with owner or starred_by")
		}
	}
	if f := r.From.Filter; f != nil && r.From.Repo == "" {
		for _, p := range []struct {
			field string
			pats  []string
		}{{"include", f.Include}, {"exclude", f.Exclude}} {
			for i, pat := range p.pats {
				if _, err := path.Match(pat, ""); err != nil {
					add("from.filter.%s[%d]: invalid pattern %q: %v", p.field, i, pat, err)
				}
			}
		}
	}

	// Destination.
	if _, ok := c.Forges[r.To.Forge]; !ok {
		add("to.forge %q is not defined", r.To.Forge)
	}
	switch {
	case (r.To.Owner == "") == (r.To.Repo == ""):
		add("to: exactly one of owner or repo must be set")
	case r.To.Repo != "":
		if r.To.Name != "" {
			add("to.name is not allowed with repo")
		}
		o, nm, ok := splitRepo(r.To.Repo)
		if !ok {
			add("to.repo %q must be owner/name", r.To.Repo)
		} else if _, _, err := (Dest{Repo: r.To.Repo}).Render(o, nm); err != nil {
			add("to.repo: %v", err)
		}
	default:
		sample := RepoRef{Owner: "owner", Name: "name"}
		nameTmpl := r.To.Name
		if nameTmpl == "" {
			nameTmpl = "{{.Name}}"
		}
		if _, err := renderField(r.To.Owner, sample, false); err != nil {
			add("to.owner: %v", err)
		}
		if _, err := renderField(nameTmpl, sample, true); err != nil {
			add("to.name: %v", err)
		}
	}

	validateSettings("", r.Interval, r.Include, r.Visibility, r.OnSourceDeleted, r.DeletionGrace, false, func(f string, a ...any) {
		// Strip the empty "loc: " prefix produced for rule-level settings.
		add("%s", strings.TrimPrefix(fmt.Sprintf(f, a...), ": "))
	})
}

func splitRepo(s string) (owner, name string, ok bool) {
	owner, name, ok = strings.Cut(s, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return "", "", false
	}
	return owner, name, true
}

func repoKey(forge, repo string) string { return forge + "\x00" + strings.ToLower(repo) }

// validateSafety checks literal-repo rules for self-mirroring, duplicate
// destinations and two-rule mirror loops.
func (c *Config) validateSafety(add func(string, ...any)) {
	loc := func(i int) string { return fmt.Sprintf("rules[%d] (%s)", i, c.Rules[i].Name) }
	literal := func(r Rule) bool { return r.From.Repo != "" && r.To.Repo != "" }
	dest := map[string]int{}
	for i, r := range c.Rules {
		if r.To.Repo != "" {
			k := repoKey(r.To.Forge, r.To.Repo)
			if j, dup := dest[k]; dup {
				add("%s: destination %s:%s is already the destination of rule %q", loc(i), r.To.Forge, r.To.Repo, c.Rules[j].Name)
			} else {
				dest[k] = i
			}
		}
		if !literal(r) {
			continue
		}
		if repoKey(r.From.Forge, r.From.Repo) == repoKey(r.To.Forge, r.To.Repo) {
			add("%s: source and destination are the same repo (%s:%s)", loc(i), r.From.Forge, r.From.Repo)
			continue
		}
		for j := 0; j < i; j++ {
			o := c.Rules[j]
			if !literal(o) {
				continue
			}
			if repoKey(o.From.Forge, o.From.Repo) == repoKey(r.To.Forge, r.To.Repo) &&
				repoKey(o.To.Forge, o.To.Repo) == repoKey(r.From.Forge, r.From.Repo) {
				add("%s: mirror loop with rule %q (%s:%s <-> %s:%s)", loc(i), o.Name, r.From.Forge, r.From.Repo, r.To.Forge, r.To.Repo)
			}
		}
	}
}

// renderField executes one template against data and validates the result as
// a repository owner/name component.
func renderField(tmpl string, data RepoRef, isName bool) (string, error) {
	t, err := template.New("").Option("missingkey=error").Parse(tmpl)
	if err != nil {
		return "", fmt.Errorf("invalid template: %w", err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("template: %w", err)
	}
	return buf.String(), checkComponent(buf.String(), isName)
}

func checkComponent(s string, isName bool) error {
	if strings.HasSuffix(s, ".git") {
		return fmt.Errorf("%q must not end in .git", s)
	}
	if !repoNameRe.MatchString(s) || s == "." || s == ".." {
		return fmt.Errorf("%q is invalid (must match %s and not be . or ..)", s, repoNameRe)
	}
	return nil
}

// Render computes the destination owner and repo name for a source repo.
// For the `repo: a/b` form the result is literal. Otherwise the Owner and
// Name templates (Name defaults to "{{.Name}}") are executed against
// {Owner, Name} of the source repo. Each rendered part must match
// ^[A-Za-z0-9._-]+$, not be "." or "..", and not end in ".git".
func (d Dest) Render(srcOwner, srcName string) (owner, name string, err error) {
	if d.Repo != "" {
		o, n, ok := splitRepo(d.Repo)
		if !ok {
			return "", "", fmt.Errorf("repo %q must be owner/name", d.Repo)
		}
		if err := checkComponent(o, false); err != nil {
			return "", "", fmt.Errorf("owner: %w", err)
		}
		if err := checkComponent(n, true); err != nil {
			return "", "", fmt.Errorf("name: %w", err)
		}
		return o, n, nil
	}
	data := RepoRef{Owner: srcOwner, Name: srcName}
	nameTmpl := d.Name
	if nameTmpl == "" {
		nameTmpl = "{{.Name}}"
	}
	if owner, err = renderField(d.Owner, data, false); err != nil {
		return "", "", fmt.Errorf("owner: %w", err)
	}
	if name, err = renderField(nameTmpl, data, true); err != nil {
		return "", "", fmt.Errorf("name: %w", err)
	}
	return owner, name, nil
}
