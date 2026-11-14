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

- Empty-but-set token env var gives "one of token or token_file is required" instead of naming the variable.
- git still reads the user's global/system git config (`url.insteadOf` could redirect a remote; a global `http.extraHeader` would be sent too). Setting `GIT_CONFIG_GLOBAL` in Docker does NOT work because gitx strips every `GIT_CONFIG*` var. Fix in gitx: isolate by default (`GIT_CONFIG_NOSYSTEM=1`, `GIT_CONFIG_GLOBAL=<empty file>`), with a config opt-out `git.use_host_config: true`; document that proxies/CAs then come from `HTTPS_PROXY` / `GIT_SSL_CAINFO` env.
- `${VAR}` inside YAML flow maps must be quoted; README examples must show `token: "${GITHUB_TOKEN}"`.
- gitx in-process lock map is never pruned (one entry per pair; fine at expected scale).
- Audit trail: `Store.DeletePair` erases the run that recorded a destination deletion. Keep a durable record (retire the pair instead of deleting it, or a separate audit table).
- Webhook bodies (up to 25 MiB) are read before signature verification; cap concurrent unauthenticated reads (semaphore) to bound memory.
- Scheduled interval counts from enqueue time, not job start (fine with coalescing; document).
- After the deletion grace, archive-fallback outcomes for unowned or already-gone destinations still record a run every reconcile cycle.

## Local notes

- No gcc, so `go test -race` only runs in CI. Local: `go vet ./... ; go test ./...`
- golangci-lint not installed locally; it runs in CI.
- Docker 29 available for the Gitea integration tests (M5).
- PowerShell splits `-flag=1.2.3` arguments; quote them (`go mod edit "-toolchain=go1.26.11"`).
