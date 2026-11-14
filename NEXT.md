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
- Review batch 2 (data-loss + concurrency) implemented, uncommitted: reconcile skips stale pairs whose
  destination a live pair targets and re-checks the source under the pair lock; ownership bound to
  destination repo id + forge URL (migration 0006, TOFU backfill); release tag-match only for created
  destinations or `adopt`, only mapped assets deleted; per-asset idle (2m) / total timeouts; deletion
  grace restarts after an observation gap > max(2 x interval, 1h); persisted "unarchived" marker;
  `allow_publish` for adopted destinations. Integration subtest `ReplacedDestination` added.
- Follow-ups from batch 2 (not done):
  - Ownership is still keyed by forge *name*: renaming a forge entry in the config orphans ownership
    (fails safe: destinations become unowned and need `adopt`).
  - TOFU backfill trusts whatever repository exists on the first run after upgrading; a repo replaced
    before the upgrade is adopted silently.
  - The asset idle timer measures reads by the uploader, so a destination that applies backpressure
    for longer than `AssetIdleTimeout` also aborts the copy (counted as a failure, retried next run).
  - Replacing a same-name destination asset with a different size still deletes it even when it is
    not in the asset map (only extra assets are protected); applies to releases gitmirror manages.
  - `delete_missing_releases` deletes a mapped release including any assets attached to it by hand.
  - The unarchived marker is per pair; two pairs into one destination (only possible across a
    transfer) could re-archive based on the other pair's marker.

## Local notes

- No gcc, so `go test -race` only runs in CI. Local: `go vet ./... ; go test ./...`
- golangci-lint not installed locally; it runs in CI.
- Docker 29 available for the Gitea integration tests (M5).
- PowerShell splits `-flag=1.2.3` arguments; quote them (`go mod edit "-toolchain=go1.26.11"`).
