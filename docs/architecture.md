# Architecture

gitmirror is a single Go binary (`cmd/gitmirror`, module `gitmirror`). Every rule describes a
**one-way** mirror from a source forge to a destination forge; "two-way" setups are two
independent rules in opposite directions on different repositories.

## Package map

| Package | Responsibility |
|---|---|
| `cmd/gitmirror` | CLI (cobra): `sync`, `plan`, `validate`, `status`, `serve`, `version`; exit codes; logging setup; signal handling. |
| `internal/config` | Strict YAML load, `${VAR}` interpolation, defaults, secret resolution (`token_file`), validation, destination name templates (`Dest.Render`), per-rule effective settings (`Rule.Effective`). Secrets are redacted in every printer. |
| `internal/app` | Composition root. Builds providers (each wrapped in the retrying HTTP client), opens the state store, takes the OS-level state-dir lock, creates the engine; implements the shared `Sync` workflow (resolve, sync, reconcile). |
| `internal/resolve` | Expands rules into concrete source-to-destination **pairs** by listing the source (owner / starred / single repo), applying filters, rendering destination names, then runtime safety checks (self-mirror, duplicate destination, mirror loop). Reports whether each rule's listing was complete. |
| `internal/engine` | The per-pair pipeline (below), release mirroring, and reconcile (source-deletion policy). Holds the data-loss safeguards. |
| `internal/gitx` | Runs `git` / `git-lfs`: sanitised environment isolated from the host's system/global git config (unless `git.use_host_config`), per-remote credentials via origin-scoped `http.extraHeader`, persistent bare cache per pair, mirror fetch/push with prune, LFS, `ls-remote`, cache lock. |
| `internal/state` | sqlite store (pure-Go `modernc.org/sqlite`, embedded migrations): destination ownership, per-pair state (last ref count and digest, consecutive failures, missing-since), run history, release id mapping, append-only audit log of destination create/adopt/archive/delete (kept when a pair is forgotten). |
| `internal/forge` | Provider interfaces and registry (`forge.Register`, `forge.New`). |
| `internal/forge/github`, `internal/forge/gitea` | Providers. `gitea` also serves Forgejo. |
| `internal/forge/httpx` | HTTP transport with retries and rate-limit waits that never repeats a non-idempotent write. |
| `internal/forge/fake` | In-memory provider used in tests. |
| `internal/server` | `serve` daemon: per-rule scheduler, two job lanes (scheduled, webhook) with coalescing, webhook receiver, `/healthz`, `/readyz`, `/metrics`, graceful shutdown. |
| `internal/integration` | Build-tag `integration` tests against a real Gitea in Docker. |

## Commands and the state-dir lock

`sync` and `serve` open the state directory through `internal/app`, which
takes an OS file lock (`<state_dir>/gitmirror.lock`, `flock` / `LockFileEx`, released by the OS if
the process dies). A second gitmirror process on the same state dir fails with "state dir is
locked" unless `--lock-timeout` makes it wait. `plan` and `status` are read-only and do not take the lock.

## The sync workflow (`app.Sync`)

1. **Resolve** every selected rule into pairs (`internal/resolve`). Per-rule listing failures are
   reported as rule errors and never abort other rules.
2. **Sync** the pairs in a worker pool (`concurrency`). Each pair gets one run record.
3. **Reconcile** pairs known to the state store that no longer resolve (source deleted, or
   filtered out): see "Deletion handling".

## Per-pair pipeline (`internal/engine/sync.go`)

```
prepare -> git (+lfs) -> settings -> releases -> wiki -> finalize
```

Later steps run only when `include` lists them and the git step completed; otherwise they are recorded as skipped.
A run ends `ok`, `partial` (a non-fatal step failed), `failed` or `aborted`.

1. **prepare**: fetch the source repo (`not found` fails the pair); clear any "missing" mark;
   compute the wanted visibility (`private` unless the rule says `match-source` *and* the source is
   public); look up the destination.
   * Destination missing: ensure the owner/org exists (`forge.OrgProvider`), create it, and record
     ownership `created`.
   * Destination exists and gitmirror owns it: continue.
   * Destination exists and gitmirror does **not** own it: the pair fails unless the rule sets
     `adopt: true` (then ownership `adopted` is recorded).
   * An archived destination is unarchived for the duration of the sync when anything is written.
2. **git / lfs**: mirror `refs/heads/*` and `refs/tags/*` through a persistent bare cache
   (force-updates and deletions are propagated: the push prunes). LFS objects are fetched and
   pushed **before** the ref push, so a failed LFS transfer is retried on the next run instead of
   leaving refs that point at missing objects. If `git-lfs` is not installed, the lfs step is
   recorded as skipped and git is mirrored without it.
   * **Empty-source guard**: if the source suddenly has no branches or tags but previously had some,
     the run is `aborted` and nothing is pruned. A source that was always empty is allowed.
   * Rejected refs (for example protected branches or hooks) are reported with the ref names.
3. **settings**: description, homepage, topics, default branch (only when that branch exists in the
   source refs), visibility (never widened beyond the wanted value). Archiving is deferred to finalize.
4. **releases** (`internal/engine/releases.go`): create/update releases and upload their assets;
   release ids are mapped in the state store. Draft releases are skipped. Assets larger than 2 GiB are
   skipped with a note. Only releases gitmirror created are updated or (with `delete_missing_releases`) deleted;
   other destination releases are left untouched.
5. **wiki**: mirrors `<repo>.wiki.git` when the source has a wiki and both providers support it. A
   first push can fail because the destination wiki repository does not exist yet; the error says
   to create one page in the destination UI and retry.
6. **finalize**: applies the final archive state (archive when the source is archived and `settings` is included;
   re-archive when the destination was archived before the sync).

`--dry-run` / `plan` run the same pipeline with all writes (forge and state store) suppressed
and print the actions that would be taken.

## Safeguards

| Safeguard | Where |
|---|---|
| Never write to a destination gitmirror did not create unless `adopt: true` | `engine.prepare`, `state.Owned` |
| Empty-source guard before pruning | `engine.syncGit`, `gitx.ErrEmptySource` |
| `deletion_grace` before applying `on_source_deleted` (only a positive "not found" counts; any other error leaves the pair alone; the clock restarts when the source reappears) | `engine/reconcile.go`, `state.MarkMissing` |
| After the grace, a no-op outcome (destination not owned or already gone) is recorded as a run once per missing period, not every cycle | `engine/reconcile.go`, `state.SetMissingReported` |
| Durable audit record of every destination create, adopt, archive and delete (real runs only); `DeletePair` does not remove it; shown by `status --audit N` | `engine/audit.go`, `state.Audit` / `state.AuditLog` |
| Repo deletion only for destinations gitmirror created, and only when the rule opts in with `on_source_deleted: delete`; otherwise archive | `engine/reconcile.go` |
| Pairs dropped by safety checks are not treated as deleted | `resolve.Result.Dropped`, `engine.Reconcile` |
| Visibility never widens | `engine.prepare` / `syncSettings` |
| Self-mirror / duplicate destination / loop checks (static for literal rules, at runtime for templates) | `config/validate.go`, `resolve.safety` |
| One gitmirror process per state dir | `app/lock*.go` |
| Credentials are passed to git only through the child environment (origin-scoped `http.extraHeader`), never argv or config files; the inherited git environment is sanitised | `gitx/env.go`, `config` redaction |
| git ignores the host's `/etc/gitconfig` and `~/.gitconfig` (`GIT_CONFIG_NOSYSTEM=1`, `GIT_CONFIG_GLOBAL=<empty file>`, set after the `GIT_CONFIG*` stripping) unless `git.use_host_config: true` | `gitx/env.go` (`isolationEnv`), `gitx.Runner.UseHostConfig` |
| Webhook signature verified (HMAC-SHA256), unknown forges and bodies over 25 MiB rejected | `server/webhook.go`, providers' `ParseWebhook` |

## Daemon (`internal/server`)

* **Scheduler**: each rule is enqueued every `interval` (jittered +-10%, first run staggered over up to 30s).
  Run history is pruned daily (last 100 runs per pair are kept).
* **Lanes**: a *scheduled* lane (full rule sync: resolve, sync, reconcile) and a *webhook* lane (targeted sync of
  one repo, no reconcile). Jobs for the same target coalesce; webhook jobs are debounced (5 s).
* **Webhooks**: `POST /webhook/<forge-name>`. A push/release/wiki/repository event for a repo found in
  a rule's last resolve triggers a targeted sync of that repo; an event for an unknown repo of an `owner`
  rule's owner (or any `starred_by` rule on that forge) triggers a full rule sync so new repos are picked up.
* **Shutdown**: on SIGINT/SIGTERM readiness fails, scheduling stops, webhooks answer 503, the HTTP server
  drains (10 s), running syncs get `--shutdown-timeout` (default 10 m) to finish before they are cancelled; exit 0.

## Adding a provider

A provider is a package under `internal/forge/<name>` that talks to one hosting platform.

1. **Implement `forge.Provider`** (`internal/forge/forge.go`): `Type`, `GetRepo`, `ListRepos`, `CreateRepo`,
   `UpdateRepoSettings`, `GitURL`, `GitAuth`, `RateLimit`. Return `forge.ErrNotFound` for missing repositories
   (the deletion logic depends on it) and `forge.ErrUnsupported` where a platform cannot do something.
2. **Implement the optional interfaces** the platform supports; the engine type-asserts them and skips the
   step otherwise:
   * `forge.ReleaseProvider` (releases and assets),
   * `forge.WikiProvider` (`WikiGitURL`),
   * `forge.StarProvider` (enables `starred_by` rules),
   * `forge.OrgProvider` (`EnsureOrg`: create the destination owner on demand),
   * `forge.Identity` (`Login`, used by `validate` to check credentials),
   * `forge.RepoDeleter` (needed for `on_source_deleted: delete`),
   * `forge.WebhookProvider` (`ParseWebhook`: verify the signature, return a `forge.Event`; reject an empty secret).
3. **Register it** from the package `init`: `forge.Register("<type>", factory)`, taking `forge.Options` (URL,
   username, token and an injected `*http.Client`; retries live in the client wrapper, not in the provider).
4. **Add the type name to `config.KnownForgeTypes`** and blank-import the package in `internal/app/app.go`
   (as `github` and `gitea` are).
5. **Test** against the shared fake-driven engine tests, plus an `httptest` server for the provider itself.
   Add an integration test (build tag `integration`) if the platform can run in Docker.
