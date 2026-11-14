# NEXT

Spec and build plan: `C:\Users\aincrad\.claude\plans\i-have-a-idea-tender-swing.md`
Branch: `build/foundations` (local only, nothing pushed).

## State

- M0 scaffold: done.
- M1 foundations: done and verified (`internal/config`, `internal/state`, `internal/forge/fake`).
- M2 git engine: done and verified (`internal/gitx`).
- M3 providers: done and verified (`httpx`, `github`, `gitea`, incl. RepoDeleter).
- M4: done and verified (`internal/resolve`, `internal/engine`, deletion grace period, gitx raw-stdout fix).

## Decisions

- Go 1.26 (toolchain go1.26.11 pinned in go.mod; Go 1.24 is out of support and current go-github needs 1.26).
- Gitea provider uses net/http directly, not the Gitea SDK (SDK context is client-wide, not per call).
- Providers take an injected `*http.Client`; the CLI wraps it with `httpx.NewClient` (retries live there only).
- gitx runs LFS before the ref push so a failed LFS transfer is retried on the next run.

## Open items for hardening (M9)

- Empty-but-set token env var gives "one of token or token_file is required" instead of naming the variable.
- git still reads the user's global git config (`url.insteadOf`, global `http.extraHeader`). Decide: set `GIT_CONFIG_GLOBAL` to an empty file in the Docker image, document for bare installs.
- `${VAR}` inside YAML flow maps must be quoted; README examples must show `token: "${GITHUB_TOKEN}"`.
- gitx in-process lock map is never pruned (one entry per pair; fine at expected scale).
- Audit trail: `Store.DeletePair` erases the run that recorded a destination deletion. Keep a durable record (retire the pair instead of deleting it, or a separate audit table).
- **Do in M7:** `app.Build` calls `AbandonRunning`, so a cron `sync` started while `serve` is mid-sync would mark the server's live runs aborted. Take an exclusive lock on `<state_dir>` in Build (second process exits with a clear "another gitmirror is using this state dir" error); `plan` must not call AbandonRunning.
- After the deletion grace, archive-fallback outcomes for unowned or already-gone destinations still record a run every reconcile cycle.

## Local notes

- No gcc, so `go test -race` only runs in CI. Local: `go vet ./... ; go test ./...`
- golangci-lint not installed locally; it runs in CI.
- Docker 29 available for the Gitea integration tests (M5).
- PowerShell splits `-flag=1.2.3` arguments; quote them (`go mod edit "-toolchain=go1.26.11"`).
