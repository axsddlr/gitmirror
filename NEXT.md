# NEXT

Spec and build plan: `C:\Users\aincrad\.claude\plans\i-have-a-idea-tender-swing.md`

## State

- M0 scaffold: done (module `gitmirror`, forge contract in `internal/forge/forge.go`, CI workflow).
- M1 foundations (config, fake provider, state store): in progress.

## Local notes

- No gcc on this machine, so `go test -race` only runs in CI. Local command: `go vet ./... ; go test ./...`
- golangci-lint is not installed locally; it runs in CI.
- Docker is available for the Gitea integration tests (M5 onward).
