# NEXT

Spec and build plan: `C:\Users\aincrad\.claude\plans\i-have-a-idea-tender-swing.md`
Branch: `build/foundations` (local only, nothing pushed).

## State

- M0 scaffold: done.
- M1 foundations: done and verified (`internal/config`, `internal/state`, `internal/forge/fake`).
- M2 git engine: done and verified (`internal/gitx`).
- M3 providers: done and verified (`httpx`, `github`, `gitea`, incl. RepoDeleter).
- M4: done and verified (`internal/resolve`, `internal/engine`, deletion grace period, gitx raw-stdout fix).
- M5 CLI: done and verified. Real-Gitea integration suite (`internal/integration`, build tag `integration`): 13 scenarios against two Gitea 28.3.0 containers, green.
- M6 releases + wiki: done; proven against real Gitea (assets byte-identical, wiki auto-initialised).
- M7 daemon: done and verified (`internal/server`, `serve`, OS-level state-dir lock, read-only `plan`, metrics on a separate loopback listener).
- M8 packaging: done (Dockerfile verified: non-root, healthy, graceful stop; goreleaser check + snapshot build; compose; systemd; README with verified token permissions; config reference). Not run: CI workflows, `deploy/Dockerfile.goreleaser`, the systemd units.
- M9 hardening + independent review (security, data-loss, concurrency lenses): all P1/P2 findings fixed and committed through `052a162`.

## Before a first release (needs the user)

- Add a LICENSE (goreleaser has `- LICENSE` commented out).
- Replace the `OWNER` placeholders (goreleaser, compose, systemd, README).
- Push to a remote so CI runs `-race`, golangci-lint (no `.golangci` config yet) and govulncheck — none of these have run.
- Optional live smoke test against real GitHub with a throwaway repo and token.
- No GitHub-side integration test exists (GitHub provider is covered by httptest fakes only).

## Decisions

- Go 1.26 (toolchain go1.26.11 pinned in go.mod; Go 1.24 is out of support and current go-github needs 1.26).
- Gitea provider uses net/http directly, not the Gitea SDK (SDK context is client-wide, not per call).
- Providers take an injected `*http.Client`; the CLI wraps it with `httpx.NewClient` (retries live there only).
- gitx runs LFS before the ref push so a failed LFS transfer is retried on the next run.

## Open items for hardening (M9)

- `${VAR}` inside YAML flow maps must be quoted; README examples must show `token: "${GITHUB_TOKEN}"`.
- gitx in-process lock map is never pruned (one entry per pair; fine at expected scale).
- Scheduled interval counts from enqueue time, not job start (fine with coalescing; document).
- `serve` cannot bypass the mass-deletion guard; a tripped pair keeps aborting until `sync --allow-prune` or raised limits.
- git no longer follows any HTTP redirect, so a renamed repo whose URL redirects fails until the config/API name catches up.
- The wiki has no mass-deletion guard (only the empty-source guard).
- Known limitations (P3, accepted for now):
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
