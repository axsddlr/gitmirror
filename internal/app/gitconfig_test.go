package app

import (
	"context"
	"testing"
)

func TestBuildPassesUseHostConfigToRunner(t *testing.T) {
	for _, want := range []bool{false, true} {
		f := newFixture(t)
		cfg := f.config()
		cfg.Git.UseHostConfig = want
		a, err := Build(context.Background(), cfg, f.options())
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		got := a.Engine.Git.UseHostConfig
		a.Close()
		if got != want {
			t.Fatalf("Runner.UseHostConfig = %v, want %v", got, want)
		}
	}
}
