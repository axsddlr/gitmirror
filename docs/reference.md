# Reference

Detailed reference for gitmirror. For a first setup, follow [getting-started.md](getting-started.md); for every config key, see [configuration.md](configuration.md).

## Safety model

gitmirror is built so that a bad config, a revoked token or an outage at the source does not
destroy data.

* **Ownership and `adopt`.** gitmirror records every destination repository it creates. It refuses
  to write to a pre-existing destination it did not create; set `adopt: true` on the rule to
  allow it (the adopted repository is then pruned like any mirror: refs missing at the source are removed).
* **Ownership is bound to the repository, not just its name.** With ownership gitmirror records the
  destination's repository id and the destination forge's base URL. If you delete a mirror and
  re-create it by hand (or restore it from a backup) under the same name, or point
  `forges.<name>.url` at another server, the repository is no longer treated as owned: syncs refuse
  to write to it (until `adopt: true`, which records the new id) and `on_source_deleted` never
  archives or deletes it. Ownership recorded by versions before this check has no id yet; it is
  filled in from the current destination on the next run (trust on first use).
* **Live destinations are never reconciled away.** When a source is transferred or renamed, or a rule
  is re-pointed, the old pair is never archived or deleted while another configured pair still
  mirrors into the same destination. The source is also checked again just before acting.
* **Releases and assets gitmirror did not make are left alone.** A destination release with the same
  tag as a source release is only updated when gitmirror created it, created the repository, or the
  rule has `adopt: true`; otherwise it is skipped (noted in the releases step). Destination assets
  are only deleted when gitmirror uploaded them; assets attached by hand stay.
* **Stalled downloads time out.** A release asset whose source download delivers no data for 2
  minutes, or takes longer than max(10 min, 1 s per MiB), is abandoned (the releases step is marked
  failed and retried next run) so one stalled server cannot block other repositories.
* **Empty-source guard.** If a source that used to have branches or tags suddenly reports none, the
  run is aborted and the destination is not pruned.
* **Mass ref deletion guard.** A run that would delete more than `max_ref_deletions` (default 5)
  destination branches/tags **and** more than `max_ref_deletions_percent` (default 50) percent of
  them is aborted before anything is pushed. If the source really dropped them, run
  `gitmirror sync --allow-prune` once (logged, and noted in the git step detail).
* **Mirrored content cannot steer git-lfs.** LFS transfers are pinned to the source's and
  destination's own LFS endpoints; a `.lfsconfig` in a mirrored repository is ignored.
* **No cross-host redirects.** git does not follow HTTP redirects (`http.followRedirects=false`),
  so a redirecting forge cannot receive the token meant for another host; git-lfs drops the
  `Authorization` header when it is redirected to another host.
* **`deletion_grace`.** A source repository must keep reporting "not found" for the grace period
  (default 24h, minimum 1h) before `on_source_deleted` (`keep` / `archive` / `delete`) is applied,
  because forges also answer "not found" when a token merely lost access. The observation must be
  continuous: if gitmirror did not check the source for more than max(2 x the rule's interval, 1h)
  (downtime, failing rule resolution) the grace period starts again. `delete` only ever removes
  repositories gitmirror itself created; adopted ones are archived instead. The default is `keep`.
* **Archived destinations stay archived.** An archived destination is unarchived only for the
  duration of a sync. gitmirror records that before unarchiving, so if the process dies before
  re-archiving, the next run re-archives it.
* **Visibility never widens.** Destinations are created `private`. With `visibility: match-source`
  a destination is public only if its source is public; it is never made more public than the source.
  An adopted private destination is never made public unless the rule also sets `allow_publish: true`.
* **Collision and loop checks.** Self-mirroring, two rules writing to one destination and two rules
  mirroring each other are rejected at config time for literal repos and dropped at runtime for
  templated destinations.
* **Dry run.** `gitmirror plan` (and `sync --dry-run`) show exactly what would change and write
  nothing, neither to the forges nor to the state store.
* **Audit log.** Every destination gitmirror creates, adopts, archives or deletes is recorded in an
  append-only audit log in the state database that outlives the pair (`gitmirror status --audit N`).
* **Host git config ignored.** git runs without the host's `/etc/gitconfig` and `~/.gitconfig`, so a
  `url.<x>.insteadOf`, `http.extraHeader` or hook setting there cannot redirect or alter mirroring
  (opt out with `git: {use_host_config: true}`).
* **State-dir lock.** Only one gitmirror process can use a state directory at a time (OS file lock).
  The state directory and cache directories are created with mode `0700`, the lock file `0600`.
* **Secret redaction.** Tokens are never printed in logs, errors or config dumps; error messages from
  `${VAR}` interpolation name the variable, never its value. `token_file` / `webhook_secret_file` keep
  secrets out of the config file. Tokens entered in the web UI are stored encrypted (see
  [Web UI](#web-ui)) and never shown again.

## Web UI

`gitmirror serve` also serves a web UI for setting gitmirror up without a config file. The UI
calls forges **sites** and rules **mirrors**.

* **Listener.** `server.ui_listen`, default `127.0.0.1:8090`, on its own listener (never the
  webhook port). `off` disables it. The environment variable `GITMIRROR_UI_LISTEN` overrides it
  (env > config file > default); the Docker image sets it to `:8090` and exposes port 8090.
* **First start.** While no admin password exists, `serve` logs at WARN
  `web UI setup: open <url>/setup and enter setup code XXXX-XXXX-XXXX` and every page redirects to
  `/setup`. Enter the code there and choose the password (at least 12 characters). The code is kept
  only in memory: a restart before setup logs a new one.
* **Pages.** **Dashboard** (status of every copy, **Sync now**, **Sync everything now**, **Details**
  with recent runs and steps), **Sites** (add a GitHub, Gitea or Forgejo site; the token is tested
  before it is saved), **Mirrors** (a five-step form: **Copy from**, **Repositories**, **Copy to**,
  **Options**, **Check and save**, which previews what would happen before **Save mirror**),
  **Webhooks** (**Public address**, **Webhook secret**, **Set up webhooks for me**) and **Settings**
  (**Change password**).
* **Changes apply at once.** Sites and mirrors saved in the web UI are used immediately; no restart
  is needed.
* **Together with the config file.** The config file is optional. Forges and rules from the file
  and from the web UI are merged and validated together (unique names; loop, duplicate-destination
  and self-mirror checks across both); file entries are shown read-only ("From config file — edit
  it there"). See [configuration.md](configuration.md).
* **Forgotten password.** Stop `serve`, run `gitmirror reset-admin` (same config and state dir),
  start `serve` again and use the new setup code. Sites, mirrors and history are kept; every
  session is signed out.

Security model:

* **Local only by default.** The default listener is `127.0.0.1`. To use the UI from other machines,
  put an HTTPS reverse proxy in front of it (and set `server.external_url` or the **Public address**
  to the `https://` URL) rather than listening on all addresses. In Docker, publish it as
  `127.0.0.1:8090:8090`.
* **Setup code.** Until a password exists, only someone who can read the log can claim the UI.
* **Password.** One admin password, hashed with PBKDF2-SHA256 (600,000 iterations, random salt).
* **Sessions.** A random token in an `HttpOnly`, `SameSite=Strict` cookie (`Secure` over TLS or with
  an `https` `external_url`); only its SHA-256 is stored. Sessions expire after 24 hours idle or 7
  days. Changing the password signs out every other session.
* **CSRF.** Every form is protected by a per-session token and an `Origin` check.
* **Rate limit.** Wrong sign-in, setup and password-change attempts are limited to 5 per address and
  100 in total per 15 minutes ("Too many wrong attempts...").
* **Tokens at rest.** Site tokens and the web UI's webhook secret are sealed with AES-256-GCM under a
  random key in `<state_dir>/secret.key` (mode `0600`). Back up `secret.key` together with
  `state.db`: without it the saved tokens cannot be read and gitmirror stops with an error naming
  the site.
* **Strict Content-Security-Policy.** No inline scripts or styles, nothing loaded from other hosts,
  and the pages cannot be framed.

## Install

### Binary

No releases have been published yet, so build from source (Go 1.26; an older Go 1.21+ downloads
the toolchain automatically):

```sh
go build -trimpath -o gitmirror ./cmd/gitmirror        # Windows: -o gitmirror.exe
```

Once releases exist: download an archive from the GitHub releases page (`linux`, `darwin`,
`windows` for `amd64` and `arm64`), verify it against `checksums.txt`, and put `gitmirror` on your
`PATH`.

`git` 2.32 or later must be installed; `git-lfs` is needed for the `lfs` feature. `gitmirror
validate` checks both.

### Docker

```sh
docker build -t gitmirror .          # or: docker pull ghcr.io/axsddlr/gitmirror:main
docker run -d --name gitmirror --restart unless-stopped \
  --stop-timeout 660 \
  -p 8080:8080 -p 127.0.0.1:8090:8090 \
  -v gitmirror-data:/data \
  gitmirror
docker logs gitmirror                # shows the web UI setup code
```

Then open http://localhost:8090. With a config file, add
`-v "$PWD/config.yaml:/config/config.yaml:ro"` (and `-e GITHUB_TOKEN -e GITEA_TOKEN` for `${VAR}`
tokens).

The image is Alpine-based, runs as uid/gid `10001` (user `gitmirror`), contains `git`, `git-lfs`
and `tini`, and uses `/data` as `HOME`. Its working directory is `/config`, and it does **not** set
`GITMIRROR_CONFIG`, so `/config/config.yaml` is read when mounted and the built-in defaults are used
when it is not (a path given through `GITMIRROR_CONFIG` or `-c` must exist). The image sets:

* `GITMIRROR_STATE_DIR=/data`, which overrides `state_dir` (env > config file > default `./data`),
  so no `state_dir` setting is needed. A bind-mounted `/data` must be writable by uid 10001.
* `GITMIRROR_UI_LISTEN=:8090`, which overrides `server.ui_listen`, so the web UI is reachable
  through a published port. Publish it on `127.0.0.1` of the host, or behind an HTTPS reverse proxy.

Ports: `8080` (webhooks, `/healthz`, `/readyz`) and `8090` (web UI) are exposed; do not publish the
metrics port. `--stop-timeout 660` lets running syncs finish after `docker stop` (Docker's default
is 10 s; `serve` allows 10 minutes). The image has a `HEALTHCHECK` on `/healthz`. Other commands
work too: `docker run --rm ... gitmirror plan`.

### Docker Compose

[`docker-compose.yml`](../docker-compose.yml) at the top of the repository runs the published image
`ghcr.io/axsddlr/gitmirror:main` (`docker compose pull && docker compose up -d`) with a data volume,
the web UI published on `127.0.0.1:8090`, webhooks on `8080`, `restart: unless-stopped`, a long
`stop_grace_period` and a healthcheck. It needs no source code: paste it into a stack manager such
as Dockhand, or use it on a server or Raspberry Pi (amd64, arm64 and 32-bit arm/v7 images are
published). CI publishes the image after every passing build of `main`, also tagged `sha-<commit>`
for pinning. The image is public, so no registry login is needed. The host ports can be changed with `GITMIRROR_UI_PORT` and `GITMIRROR_WEBHOOK_PORT`, and
`GITMIRROR_UI_BIND` (default `127.0.0.1`) controls where the web pages listen (environment or a
`.env` file). A config bind mount and Docker `secrets:` mapped to `token_file` are included as
commented "advanced" options. It also contains a commented Gitea service for local trials.

To build from the source instead, add the [`compose.build.yml`](../compose.build.yml) override:
`docker compose -f docker-compose.yml -f compose.build.yml up -d --build`.

### systemd

[`deploy/systemd/gitmirror.service`](../deploy/systemd/gitmirror.service) runs `serve` as a hardened
unit (`User=gitmirror`, `StateDirectory=gitmirror`, `ProtectSystem=strict`,
credentials via `LoadCredential=` and `token_file: "${CREDENTIALS_DIRECTORY}/..."`). If you would rather
run one-shot syncs on a schedule, use [`gitmirror-sync.timer`](../deploy/systemd/gitmirror-sync.timer) with
[`gitmirror-sync.service`](../deploy/systemd/gitmirror-sync.service) **instead of** the daemon, not
alongside it: the state-dir lock makes the second process fail. The unit passes
`-c /etc/gitmirror/config.yaml`, so that file must exist (for a web-UI-only setup it can contain just
`state_dir: /var/lib/gitmirror`; remove the `LoadCredential=` lines you do not use, because systemd
refuses to start when a credential file is missing). The web UI then listens on `127.0.0.1:8090` of
the server: reach it through an SSH tunnel (`ssh -L 8090:127.0.0.1:8090 server`) or an HTTPS
reverse proxy.

## Commands

```
gitmirror mirrors repositories one-way between git forges (GitHub, Gitea).

Exit codes:
  0  everything ok
  1  any pair failed or was aborted, or a fatal runtime error
  2  usage or configuration error
  3  partial: some pair was partial or a rule had errors, but nothing failed

Usage:
  gitmirror [command]

Available Commands:
  help        Help about any command
  plan        Show what a sync would do, without changing anything
  reset-admin Forget the web UI admin password and sign everyone out
  serve       Run the daemon: scheduled syncs, webhooks, /healthz, /readyz and /metrics (own listener)
  status      Show the last known state of every pair (no network)
  sync        Mirror every pair now
  validate    Check the config, git, forge credentials and rule listings
  version     Print version information

Flags:
  -c, --config string       path to the config file (env GITMIRROR_CONFIG); without it a missing config.yaml means built-in defaults (default "config.yaml")
  -h, --help                help for gitmirror
      --json                machine-readable JSON output on stdout
      --log-format string   log format: text or json (default "text")
      --log-level string    log level: debug, info, warn, error (default "info")
```

Command-specific flags:

| Command | Flags |
|---|---|
| `sync` | `--allow-prune` (this run only: bypass the mass ref deletion guard), `--dry-run`, `--lock-timeout <duration>`, `--repo OWNER/NAME` (repeatable; skips reconcile), `--rule NAME` (repeatable) |
| `plan` | `--repo OWNER/NAME` (repeatable), `--rule NAME` (repeatable); read-only, does not take the state-dir lock |
| `validate` | `--offline` (config and local tools only, no network) |
| `status` | `--runs N` (also list the N most recent runs), `--audit N` (also list the N most recent audit events: destination create, adopt, archive, delete) |
| `serve` | `--lock-timeout <duration>`, `--shutdown-timeout <duration>` (default 10m: how long running syncs may finish after SIGINT/SIGTERM before they are aborted; exits 0) |
| `reset-admin` | `--lock-timeout <duration>`. Removes the web UI admin password and every web UI session; the next `serve` logs a new setup code. Sites, mirrors and the rest of the state are kept. Stop `serve` first: it refuses while another process holds the state dir. |
| `version` | none |

`--lock-timeout` waits for another gitmirror process to release the state dir (`0` fails at once).

Every command works with sites and mirrors from the web UI as well as from the config file, as long
as it uses the same state dir. `sync`, `plan` and `validate` (without `--offline`) exit with code
2 when there is no mirror rule at all; `serve` starts anyway and waits for rules to be added.

### Reading the output

`plan` shows exactly what a sync would do and changes nothing, neither on the forges nor in the
state database:

```
ok       github:you/project → gitea:github-mirror/project  prepare:ok git:ok lfs:skipped settings:ok releases:unchanged wiki:skipped  666ms
    - create repository gitea:github-mirror/project (private)
    - push all 1 refs
    - update settings of gitea:github-mirror/project: default branch
1 pairs: 1 ok, 0 partial, 0 failed, 0 aborted; 0 rule errors; 0 reconcile (dry run)
```

Each line is one **pair** (source → destination) followed by what each step would do. Read the
`- create repository` lines carefully on the first run: they tell you where every mirror will
appear.

`sync` prints the same kind of line for what it did:

```
ok       github:you/project → gitea:github-mirror/project  prepare:ok git:ok lfs:unchanged settings:unchanged releases:unchanged wiki:skipped  4.616s
1 pairs: 1 ok, 0 partial, 0 failed, 0 aborted; 0 rule errors; 0 reconcile
```

The first run copies everything and can take a while for large repositories. Later runs only
transfer what changed; a run with no changes reports `git:unchanged` and takes a few seconds.

| Pair status | Meaning |
|---|---|
| `ok` | Everything that applies was mirrored. |
| `partial` | Some steps worked but one (for example releases or the wiki) failed. Retried next run. |
| `failed` | The pair could not be mirrored (for example the destination could not be created or the push was rejected). The error is on the line. |
| `aborted` | A safety guard stopped the run before anything was changed (see [Troubleshooting](#troubleshooting)). |

Step values: `ok` (changed something), `unchanged`, `skipped` (not applicable, e.g. the source has
no wiki or LFS), `failed`. Exit codes for scripts: `0` all ok, `1` something failed or was aborted,
`2` config or usage error, `3` partial. Limit `plan` and `sync` with `--rule NAME` or
`--repo OWNER/NAME` (both repeatable). Add `--json` to any command for machine-readable output.

`status` reads only the local state database, so it works offline:

```
PAIR                                              LAST SUCCESS         FAILURES  LAST ERROR
my-github: github:you/project → gitea:github-mirror/project  2026-11-14 11:42:29  0

recent runs (2):
ID  STARTED              TRIGGER  STATUS  PAIR                                              ERROR
2   2026-11-14 11:42:26  cli      ok      my-github: github:you/project → gitea:github-mirror/project
1   2026-11-14 11:42:21  cli      ok      my-github: github:you/project → gitea:github-mirror/project
```

That is `gitmirror status --runs 5`. `--audit 20` also lists the last 20 times gitmirror created,
adopted, archived or deleted a destination.

## Webhooks

Webhooks make a push show up at the destination within seconds instead of at the next interval.
They are configured on the **source** forge and POST to gitmirror's `server.listen` port (default
`8080`), never to the web UI port. The source forge must be able to reach that address; for GitHub
that means from the internet.

The web UI's **Webhooks** page shows the values below for each source site, and its **Set up
webhooks for me** button creates the webhooks through the forge API: one organisation webhook for a
mirror that copies a whole organisation, one per repository otherwise, skipping ones that already
exist. That needs the webhook token permissions in [Token permissions](#token-permissions). By hand:

* URL: `<public address>/webhook/<forge-name>`, where the public address is `server.external_url`
  or the web UI's **Public address** (for example `https://mirror.example.com`), and `<forge-name>`
  is the site's short name, the key under `forges:` (for example `/webhook/github`). Put a TLS
  reverse proxy in front of port 8080. If one public name serves both, route `/webhook/` to
  `server.listen` and everything else to `server.ui_listen`.
* Secret: the value of `server.webhook_secret` (or the contents of `webhook_secret_file`), or else
  the secret the web UI created (Webhooks page, **Show the secret**). Without any secret, webhooks
  are disabled and the endpoint answers 404.
* Content type: `application/json`.
* Events to send:

| Forge | Events | Signature header |
|---|---|---|
| GitHub | `push` (also `create`, `delete`), `release`, `repository`, `gollum` (wiki). In the UI choose "Let me select individual events": Pushes, Branch or tag creation, Branch or tag deletion, Releases, Repositories, Wiki. | `X-Hub-Signature-256` |
| Gitea / Forgejo | Push, Create, Delete, Release, Repository, Wiki (custom events). | `X-Gitea-Signature` / `X-Forgejo-Signature` |

Other events are acknowledged and ignored. A delivery for a repo that a rule already resolved
triggers a targeted sync of that repo (debounced 5 s); a delivery for an unknown repo of a rule's
owner (or on a forge with a `starred_by` rule) triggers a full rule sync, which picks up new repositories.
Responses: `202` accepted, `204` ignored event, `401` bad signature, `404` unknown forge or webhooks
disabled, `413` body over 25 MiB, `503` shutting down or busy (more than 8 webhook bodies being read at once; `Retry-After: 1`). Webhooks are optional; the scheduler alone keeps
everything in sync.

## Endpoints and metrics

`serve` listens on `server.listen` (default `:8080`):

| Endpoint | Description |
|---|---|
| `GET /healthz` | `200 ok` while the process is up. |
| `GET /readyz` | `200 ready` once started, `503` during shutdown. |
| `POST /webhook/<forge>` | Webhook receiver. |

The web UI is served on its own listener, `server.ui_listen` (default `127.0.0.1:8090`); see
[Web UI](#web-ui).

`GET /metrics` (Prometheus text format, plus standard Go and process collectors) is **not** served
on that listener, because it is unauthenticated and its labels contain repository and rule names.
It is served on its own listener, `server.metrics_listen` (default `127.0.0.1:9090`; `off` disables
metrics). In Docker set `metrics_listen: ":9090"` and keep port 9090 unpublished (reachable only
from your monitoring network).

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `gitmirror_sync_runs_total` | counter | `rule`, `status` | Pair sync runs by final status. |
| `gitmirror_sync_duration_seconds` | histogram | `rule` | Duration of one pair sync. |
| `gitmirror_pair_last_success_timestamp_seconds` | gauge | `rule`, `src`, `dst` | Unix time of the pair's last ok or partial run (0 = never). |
| `gitmirror_pair_consecutive_failures` | gauge | `rule`, `src`, `dst` | Failed or aborted runs since the last success. |
| `gitmirror_queue_depth` | gauge | `lane` | Jobs waiting in the `scheduled` or `webhook` lane. |
| `gitmirror_webhooks_total` | counter | `forge`, `result` | Deliveries by result (`accepted`, `ignored`, `unauthorized`, `bad_request`, `too_large`, `disabled`, `busy`). |
| `gitmirror_rule_errors_total` | counter | `rule` | Rule-level errors (listing failures, safety drops, failed jobs). |
| `gitmirror_forge_rate_limit_remaining` | gauge | `forge` | Remaining API quota, when the forge reports one. |

A useful alert: `time() - gitmirror_pair_last_success_timestamp_seconds > 3 * <interval>` or
`gitmirror_pair_consecutive_failures > 2`.

## Token permissions

Give each forge a token with the least access its role needs. "Source" means the forge is only read from; "destination" means gitmirror creates and writes mirrors there. A forge used in both roles needs both sets.

**GitHub, classic personal access token** (recommended when the destination is a personal account)

| Role | Scopes |
|---|---|
| Source | `repo` (or `public_repo` if every source repo is public) |
| Destination | `repo`, plus `workflow` if mirrored history changes files under `.github/workflows/` |
| Destination with `on_source_deleted: delete` | add `delete_repo` |
| Source, with **Set up webhooks for me** | add `admin:repo_hook` (and `admin:org_hook` for organisation webhooks) |
| Organisations | GitHub has no API to create organisations; create them on GitHub first |

**GitHub, fine-grained personal access token**

| Role | Repository permissions |
|---|---|
| Source | Metadata: read, Contents: read (and Starring: read for `starred_by` rules) |
| Destination | Contents: write, Administration: write, Workflows: write |

A fine-grained token is limited to one resource owner (one user or one organisation), and an organisation may require an admin to approve it before it can read private repos.

**Gitea / Forgejo scoped token** (Gitea 1.19 or later)

| Role | Scopes |
|---|---|
| Source | `read:repository`, `read:user`, `read:organization` |
| Destination | `write:repository`, `read:user`, plus `write:organization` when mirroring into an organisation or letting gitmirror create one |
| Destination with `on_source_deleted: delete` | `write:repository`; the token's user must be an owner or admin of the repository |
| Source, with **Set up webhooks for me** | `write:repository` (`write:organization` for organisation webhooks); the token's user must be an admin of the repository |

Creating organisations also requires the account to be allowed to: it must be an admin, or have "may create organisations" enabled (`DEFAULT_ALLOW_CREATE_ORGANIZATION`) on a server without `DISABLE_REGULAR_ORG_CREATION`. Git over HTTPS uses the same repository scope: read for fetching, write for pushing.

Sources: GitHub [OAuth scopes](https://docs.github.com/en/apps/oauth-apps/building-oauth-apps/scopes-for-oauth-apps), [fine-grained token permissions](https://docs.github.com/en/rest/authentication/permissions-required-for-fine-grained-personal-access-tokens), [managing personal access tokens](https://docs.github.com/en/authentication/keeping-your-account-and-data-secure/managing-your-personal-access-tokens); Gitea source `routers/api/v1/api.go` (`tokenRequiresScopes`), `models/auth/access_token_scope.go`, `routers/web/repo/githttp.go`; [Forgejo token scopes](https://forgejo.org/docs/latest/user/token-scope/).

Not yet confirmed: whether GitHub rejects a push that touches `.github/workflows/` when the token lacks `workflow` (the docs describe the scope as needed to "add and update" workflow files), and whether a fine-grained token can create repositories in a personal account. If in doubt, use a classic token for GitHub destinations.

## Troubleshooting

* **Cannot open http://localhost:8090.** Check the log for `web UI: http://...` (where it listens)
  or `web UI is disabled`. In Docker the UI must listen on `:8090` inside the container (the image
  sets `GITMIRROR_UI_LISTEN=:8090`) and the port must be published (`-p 127.0.0.1:8090:8090`). On a
  server it only listens on `127.0.0.1`: use an SSH tunnel or a reverse proxy.
* **`listen for the web UI on ...: bind`.** Another program uses port 8090. Set `server.ui_listen`
  or `GITMIRROR_UI_LISTEN` to another address.
* **"That setup code is not right."** Use the code from the newest `web UI setup` log line; a
  restart creates a new one.
* **"Too many wrong attempts."** Wait 15 minutes, then try again.
* **Forgotten web UI password.** Stop `serve`, run `gitmirror reset-admin`, start `serve` and use the
  new setup code.
* **A site's token cannot be decrypted at start.** `state.db` was restored or moved without its
  `secret.key`. Restore the matching `secret.key` into the state dir.
* **"destination ... already exists and was not created by gitmirror"**: the destination repo exists
  and the rule does not allow adopting it. Pick another destination or set `adopt: true` (this prunes
  the repo to match the source).
* **Wiki push fails on the first run.** The destination wiki repository may not exist until the
  wiki has one page. Create a page in the destination's web UI, then run again.
* **Pushing workflow files is rejected on GitHub.** Pushing `.github/workflows/*` to GitHub requires a
  token with the `workflow` scope (classic PAT) or the equivalent workflow permission; without it the push
  of those refs is rejected and the pair is reported as failed.
* **`git-lfs` not found.** If `include` has `lfs` but `git-lfs` is not installed, the lfs step is
  recorded as skipped (a warning is logged) and only git is mirrored. Install `git-lfs` (it is in the
  Docker image) or remove `lfs` from `include`. `gitmirror validate` reports the versions it finds.
  Once LFS works again, the next run copies the missing LFS objects even if no ref changed (the
  state store remembers for which refs LFS last completed).
* **"sync would delete N of M destination refs ... refusing to push".** The mass ref deletion guard
  tripped. Check the source; if the deletions are intended, run `gitmirror sync --allow-prune` once
  (optionally with `--repo OWNER/NAME`), or raise `max_ref_deletions` / `max_ref_deletions_percent`.
* **"source has no refs but previously had N; refusing to prune destination".** The empty-source guard
  tripped (outage, lost access or a truly emptied repo). Investigate; the destination was not touched.
* **`forges.<name>.token: ${VAR} resolved to an empty value`** (same for `server.webhook_secret`): the
  variable is defined but empty or whitespace. Set it, or use `token_file`.
* **`another gitmirror process is using state dir ...`.** Another gitmirror (a running `serve`, or a timer-triggered `sync`) holds
  the state directory. Use only one, or pass `--lock-timeout`.
* **Permission denied on `/data` in Docker.** The container user is uid 10001; chown the bind mount or
  use a named volume.
* **Git prompts or a user git config interfere.** gitmirror disables terminal prompts, ignores
  `GIT_CONFIG*` variables and, by default, the host's `/etc/gitconfig` and `~/.gitconfig`.
* **`listen on :8080: ... bind`.** Another program already uses port 8080. Set `server.listen`
  (e.g. `"127.0.0.1:18080"`) and, if 9090 is also taken, `server.metrics_listen`.
* **TLS or proxy errors that a plain `git clone` does not hit.** The host git config is ignored by
  default: set `HTTPS_PROXY` / `HTTP_PROXY` / `NO_PROXY` and `GIT_SSL_CAINFO` in gitmirror's environment,
  or set `git: {use_host_config: true}` if git needs host settings (e.g. `http.sslBackend=schannel` on Windows).

## Limitations and roadmap

* Issues, pull requests, discussions, stars, forks and project boards are **not** mirrored.
* Draft releases are not mirrored; release assets above 2 GiB are skipped.
* Providers today: GitHub and Gitea/Forgejo (`type: gitea`). GitLab and Bitbucket are possible through
  the provider interface but are not implemented.
* Rules are one-way; there is no conflict resolution, the destination is overwritten to match the source.
* `sync` ignores per-rule `interval`; it is used by `serve`.
* No `LICENSE` file has been added yet; add one before publishing.

### Adding a provider

1. Implement `forge.Provider` (and whichever of `ReleaseProvider`, `WikiProvider`, `StarProvider`,
   `OrgProvider`, `Identity`, `RepoDeleter`, `WebhookProvider` the platform supports) in
   `internal/forge/<name>`.
2. Call `forge.Register("<type>", factory)` from the package's `init` and blank-import the package in
   `internal/app/app.go`.
3. Add the type to `config.KnownForgeTypes`.

Details in [architecture.md](architecture.md#adding-a-provider).

## Development

```sh
go vet ./... && go test ./...                                      # unit tests
go test -tags integration -count=1 -timeout 30m ./internal/integration/   # real Gitea in Docker
```

`make` targets are listed in the [Makefile](../Makefile).
