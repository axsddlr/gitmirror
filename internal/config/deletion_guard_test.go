package config

import (
	"strings"
	"testing"
)

func TestRefDeletionGuardDefaults(t *testing.T) {
	c := mustParse(t, doc("", okRule), nil, nil)
	e := c.Rules[0].Effective(c.Defaults)
	if e.MaxRefDeletions != 5 || e.MaxRefDeletionsPercent != 50 {
		t.Fatalf("effective guard = %d refs / %d%%, want 5 / 50", e.MaxRefDeletions, e.MaxRefDeletionsPercent)
	}
	if c.Defaults.MaxRefDeletions == nil || *c.Defaults.MaxRefDeletions != 5 ||
		c.Defaults.MaxRefDeletionsPercent == nil || *c.Defaults.MaxRefDeletionsPercent != 50 {
		t.Fatalf("defaults not filled: %v %v", c.Defaults.MaxRefDeletions, c.Defaults.MaxRefDeletionsPercent)
	}
}

func TestRefDeletionGuardOverrides(t *testing.T) {
	rules := okRule + `
  - name: r2
    from: {forge: gh, owner: other}
    to: {forge: home, owner: mirrors2}
    max_ref_deletions: 0
    max_ref_deletions_percent: 100
`
	c := mustParse(t, doc("defaults: {max_ref_deletions: 20, max_ref_deletions_percent: 10}", rules), nil, nil)
	e1 := c.Rules[0].Effective(c.Defaults)
	if e1.MaxRefDeletions != 20 || e1.MaxRefDeletionsPercent != 10 {
		t.Fatalf("r1 = %d / %d, want defaults 20 / 10", e1.MaxRefDeletions, e1.MaxRefDeletionsPercent)
	}
	e2 := c.Rules[1].Effective(c.Defaults)
	if e2.MaxRefDeletions != 0 || e2.MaxRefDeletionsPercent != 100 {
		t.Fatalf("r2 = %d / %d, want overrides 0 / 100", e2.MaxRefDeletions, e2.MaxRefDeletionsPercent)
	}
}

func TestRefDeletionGuardValidation(t *testing.T) {
	bad := []struct{ top, rule, want string }{
		{"defaults: {max_ref_deletions_percent: 0}", "", "defaults: max_ref_deletions_percent 0 is out of range 1..100"},
		{"defaults: {max_ref_deletions_percent: 101}", "", "defaults: max_ref_deletions_percent 101 is out of range 1..100"},
		{"defaults: {max_ref_deletions: -1}", "", "defaults: max_ref_deletions -1 must be >= 0"},
		{"", "    max_ref_deletions_percent: 0\n", "rules[0] (r1): max_ref_deletions_percent 0 is out of range 1..100"},
		{"", "    max_ref_deletions: -3\n", "rules[0] (r1): max_ref_deletions -3 must be >= 0"},
	}
	for _, b := range bad {
		_, err := parse(t, doc(b.top, okRule+b.rule), nil, nil)
		if err == nil || !strings.Contains(err.Error(), b.want) {
			t.Errorf("top=%q rule=%q: err = %v, want %q", b.top, b.rule, err, b.want)
		}
	}
	mustParse(t, doc("defaults: {max_ref_deletions: 0, max_ref_deletions_percent: 1}", okRule), nil, nil)
}
