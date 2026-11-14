package config

import (
	"strings"
	"testing"
)

func TestGitUseHostConfig(t *testing.T) {
	if c := mustParse(t, doc("", okRule), nil, nil); c.Git.UseHostConfig {
		t.Fatal("git.use_host_config defaults to true")
	}
	if c := mustParse(t, doc("git: {use_host_config: false}", okRule), nil, nil); c.Git.UseHostConfig {
		t.Fatal("use_host_config: false parsed as true")
	}
	if c := mustParse(t, doc("git:\n  use_host_config: true", okRule), nil, nil); !c.Git.UseHostConfig {
		t.Fatal("use_host_config: true not parsed")
	}
	if _, err := parse(t, doc("git: {use_host_cfg: true}", okRule), nil, nil); err == nil || !strings.Contains(err.Error(), "use_host_cfg") {
		t.Fatalf("unknown git key: err = %v", err)
	}
}
