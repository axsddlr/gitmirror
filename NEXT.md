# NEXT

Spec and build plan: `C:\Users\aincrad\.claude\plans\i-have-a-idea-tender-swing.md`
Branch: `build/foundations` (local only, nothing pushed).

## State

- M0 scaffold: done.
- M1 foundations: done and verified (`internal/config`, `internal/state`, `internal/forge/fake`).
- M2 git engine: done and verified (`internal/gitx`).
- M3 providers: done and verified (`httpx`, `github`, `gitea`, incl. RepoDeleter).
- M4: done and verified (`internal/resolve`, `internal/engine`, deletion grace period, gitx raw-stdout fix).
- M5 CLI: done and verified. Real-Gitea integration tests in progress (`internal/integration`, build tag `integration`).
- M6 releases + wiki: built into the engine (M4); real-server proof comes from the integration tests.
- M7 daemon: done and verified (`internal/server`, `serve`, OS-level state-dir lock, read-only `plan`).

## Decisions

- Go 1.26 (toolchain go1.26.11 pinned in go.mod; Go 1.24 is out of support and current go-github needs 1.26).
- Gitea provider uses net/http directly, not the Gitea SDK (SDK context is client-wide, not per call).
- Providers take an injected `*http.Client`; the CLI wraps it with `httpx.NewClient` (retries live there only).
- gitx runs LFS before the ref push so a failed LFS transfer is retried on the next run.

## Open items for hardening (M9)

- `${VAR}` inside YAML flow maps must be quoted; README examples must show `token: "${GITHUB_TOKEN}"`.
- gitx in-process lock map is never pruned (one entry per pair; fine at expected scale).
- Scheduled interval counts from enqueue time, not job start (fine with coalescing; document).

## Local notes

- No gcc, so `go test -race` only runs in CI. Local: `go vet ./... ; go test ./...`
- golangci-lint not installed locally; it runs in CI.
- Docker 29 available for the Gitea integration tests (M5).
- PowerShell splits `-flag=1.2.3` arguments; quote them (`go mod edit "-toolchain=go1.26.11"`).
