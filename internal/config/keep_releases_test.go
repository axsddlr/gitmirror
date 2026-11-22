package config

import (
	"strings"
	"testing"
)

func TestKeepReleases(t *testing.T) {
	c := mustParse(t, doc("", okRule), nil, nil)
	if k := c.Rules[0].Effective(c.Defaults).KeepReleases; k != 0 {
		t.Fatalf("default keep_releases = %d, want 0 (all)", k)
	}
	c = mustParse(t, doc("", okRule+"    keep_releases: 3\n"), nil, nil)
	if k := c.Rules[0].Effective(c.Defaults).KeepReleases; k != 3 {
		t.Fatalf("keep_releases = %d, want 3", k)
	}
	_, err := parse(t, doc("", okRule+"    keep_releases: -1\n"), nil, nil)
	if err == nil || !strings.Contains(err.Error(), "rules[0] (r1): keep_releases -1 must be >= 0") {
		t.Fatalf("negative keep_releases: err = %v", err)
	}
}
