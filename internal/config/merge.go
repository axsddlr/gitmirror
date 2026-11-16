package config

import (
	"errors"
	"fmt"
	"slices"
	"sort"
)

// ErrNoRules is reported by commands that need at least one mirror rule
// (sync, plan, validate) when neither the config file nor the web UI
// defines any.
var ErrNoRules = errors.New("no mirror rules configured: add one in the web UI or in the config file")

// ErrFileDefined is wrapped by errors about a web UI change that names a
// forge or rule defined in the config file: those are read-only in the UI.
var ErrFileDefined = errors.New("defined in the config file")

// Origin tells where a forge or rule of an effective config is defined.
type Origin string

// Origin values.
const (
	// OriginFile is the config file (read-only in the web UI).
	OriginFile Origin = "file"
	// OriginUI is the web UI (stored in the state database).
	OriginUI Origin = "ui"
)

// ForgeOrigin reports where forge name is defined, or "" if it is not.
func (c *Config) ForgeOrigin(name string) Origin {
	if _, ok := c.Forges[name]; !ok {
		return ""
	}
	if c.uiForges[name] {
		return OriginUI
	}
	return OriginFile
}

// RuleOrigin reports where rule name is defined, or "" if it is not.
func (c *Config) RuleOrigin(name string) Origin {
	for _, r := range c.Rules {
		if r.Name == name {
			if c.uiRules[name] {
				return OriginUI
			}
			return OriginFile
		}
	}
	return ""
}

// Merge returns the effective config: file (as returned by Parse or Load)
// plus the forges and rules managed in the web UI. file and the arguments
// are not modified.
//
// UI names must not be used by the file (the error wraps ErrFileDefined and
// names the clash). UI forges carry their token directly (token_file is not
// supported) and get the same normalisation as file forges; UI rules get the
// same defaults as file rules and follow the file rules, sorted by name. The
// result must pass Validate as a whole, so loops, duplicate destinations and
// self-mirroring are checked across file and UI rules. The result reports
// each entry's origin through ForgeOrigin and RuleOrigin.
func Merge(file *Config, forges map[string]Forge, rules []Rule) (*Config, error) {
	out := *file
	out.Forges = make(map[string]Forge, len(file.Forges)+len(forges))
	for n, f := range file.Forges {
		out.Forges[n] = f
	}
	out.Rules = slices.Clone(file.Rules)
	out.uiForges = make(map[string]bool, len(forges))
	out.uiRules = make(map[string]bool, len(rules))

	var errs []error
	names := make([]string, 0, len(forges))
	for n := range forges {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if _, clash := file.Forges[n]; clash {
			errs = append(errs, fmt.Errorf("forge %q is %w and cannot also be managed in the web UI", n, ErrFileDefined))
			continue
		}
		f := normaliseForge(forges[n])
		if f.TokenFile != "" {
			errs = append(errs, fmt.Errorf("forges.%s: token_file is not supported for forges managed in the web UI", n))
		}
		out.Forges[n] = f
		out.uiForges[n] = true
	}

	ui := make([]Rule, 0, len(rules))
	for _, r := range rules {
		if file.RuleOrigin(r.Name) != "" {
			errs = append(errs, fmt.Errorf("rule %q is %w and cannot also be managed in the web UI", r.Name, ErrFileDefined))
			continue
		}
		r = CloneRule(r)
		applyRuleDefaults(&r)
		ui = append(ui, r)
		out.uiRules[r.Name] = true
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	sort.SliceStable(ui, func(i, j int) bool { return ui[i].Name < ui[j].Name })
	out.Rules = append(out.Rules, ui...)
	if err := out.Validate(); err != nil {
		return nil, err
	}
	return &out, nil
}

// CloneRule returns a copy of r that shares no mutable memory with it:
// modifying the copy's slices or filter never affects r. Pointer fields are
// shared; they are never written through.
func CloneRule(r Rule) Rule {
	r.Include = slices.Clone(r.Include)
	if r.From.Filter != nil {
		f := *r.From.Filter
		f.Include = slices.Clone(f.Include)
		f.Exclude = slices.Clone(f.Exclude)
		r.From.Filter = &f
	}
	return r
}
