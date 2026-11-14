package forge_test

import (
	"gitmirror/internal/forge"
	"gitmirror/internal/forge/fake"
	"gitmirror/internal/forge/gitea"
	"gitmirror/internal/forge/github"
)

// Every built-in provider must report its identity for `validate`.
var (
	_ forge.Identity = (*github.Provider)(nil)
	_ forge.Identity = (*gitea.Provider)(nil)
	_ forge.Identity = (*fake.Provider)(nil)
)

var _ forge.WikiInitializer = (*gitea.Provider)(nil)
