# Getting started

This guide takes you from nothing to a running mirror, step by step. It uses the most common
setup, **GitHub to a self-hosted Gitea**, and shows how the other directions differ along the way.
If you only want to see gitmirror work before touching your real accounts, start with
[Try it locally first](#try-it-locally-first).

Contents:

1. [What you need](#1-what-you-need)
2. [Get the gitmirror binary](#2-get-the-gitmirror-binary)
3. [Create access tokens](#3-create-access-tokens)
4. [Write the config file](#4-write-the-config-file)
5. [Give gitmirror the tokens](#5-give-gitmirror-the-tokens)
6. [Validate](#6-validate)
7. [Preview with `plan`](#7-preview-with-plan)
8. [Run the first sync](#8-run-the-first-sync)
9. [Check the status](#9-check-the-status)
10. [Keep it running](#10-keep-it-running)
11. [Webhooks for instant mirroring (optional)](#11-webhooks-for-instant-mirroring-optional)
12. [Day-to-day operation](#12-day-to-day-operation)

Commands are shown for **bash** (Linux, macOS, Git Bash on Windows) and **PowerShell** (Windows)
where they differ.

---

## 1. What you need

| Requirement | Why | Check |
|---|---|---|
| **git 2.32 or later** | gitmirror shells out to git for all transfers. | `git --version` |
| **git-lfs** (optional) | Only for mirroring LFS files. Git for Windows ships it. Without it the LFS step is skipped with a warning. | `git lfs version` |
| **Go 1.26** (to build) | There are no published binaries yet, so you build from source. Any Go 1.21+ downloads the 1.26 toolchain automatically. | `go version` |
| **Docker** (alternative) | Instead of Go + git: the image contains everything. | `docker version` |
| A **GitHub** account and a **Gitea or Forgejo** server you can log in to | The two ends of the mirror. | |

gitmirror needs network access to both forges' APIs and git over HTTPS. It does not use SSH.

## 2. Get the gitmirror binary

### Option A: build from source

From the repository root:

```sh
# bash
go build -trimpath -o gitmirror ./cmd/gitmirror
./gitmirror version
```

```powershell
# PowerShell
go build -trimpath -o gitmirror.exe ./cmd/gitmirror
.\gitmirror.exe version
```

You should see something like `gitmirror dev (commit e2ccfac..., go1.26.11)`. Put the binary
somewhere on your `PATH` (or call it by its full path). The rest of this guide writes `gitmirror`.

`make build` does the same with version information baked in and writes `bin/gitmirror`.

### Option B: Docker image

```sh
docker build -t gitmirror .
docker run --rm gitmirror version
```

When using Docker, every `gitmirror <command>` in this guide becomes
`docker run --rm -v "$PWD/config.yaml:/config/config.yaml:ro" -v gitmirror-data:/data -e GITHUB_TOKEN -e GITEA_TOKEN gitmirror <command>`.
Running it permanently is covered in [step 10](#10-keep-it-running).

## 3. Create access tokens

gitmirror talks to each forge with a token. The **source** token only needs to read; the
**destination** token creates repositories and pushes to them. Full permission tables are in the
reference under [Token permissions](reference.md#token-permissions).

### GitHub (source in this guide)

1. GitHub → your avatar → **Settings** → **Developer settings** → **Personal access tokens** →
   **Tokens (classic)** → **Generate new token (classic)**.
2. Name it `gitmirror`, choose an expiry, and tick **`repo`** (full control of private
   repositories). If every repository you mirror is public, `public_repo` is enough.
3. If GitHub will be a **destination** (Gitea → GitHub), also tick **`workflow`**, and
   **`delete_repo`** if you plan to use `on_source_deleted: delete`.
4. Copy the token now; GitHub shows it only once.

Classic tokens are the safe choice. Fine-grained tokens work for reading but are limited to one
account or organisation; see [Token permissions](reference.md#token-permissions) if you prefer them.

### Gitea / Forgejo (destination in this guide)

1. Gitea → your avatar → **Settings** → **Applications** → **Manage Access Tokens**.
2. Name it `gitmirror` and under **Select permissions** set:
   * **repository**: Read and Write
   * **user**: Read
   * **organization**: Read and Write (needed when mirrors go into an organisation, or when
     gitmirror should create that organisation for you)
3. **Generate Token** and copy it.

Creating organisations also requires your Gitea account to be allowed to create them (admins
always are). If it is not, create the destination organisation in the web UI first.

## 4. Write the config file

Create `config.yaml` next to the binary (or anywhere and pass `-c path/to/config.yaml`). Start
with this minimal config. It mirrors **every repository you own on GitHub** into an organisation
called `github-mirror` on your Gitea:

```yaml
# Where gitmirror keeps its database, git caches and lock file.
state_dir: ./data

forges:
  github:
    type: github
    token: "${GITHUB_TOKEN}"     # read from the environment, see step 5
  gitea:
    type: gitea
    url: https://git.example.com # your Gitea's base URL
    token: "${GITEA_TOKEN}"

rules:
  - name: my-github
    from:
      forge: github
      owner: your-github-username   # a user or an organisation
    to:
      forge: gitea
      owner: github-mirror          # created on Gitea if it does not exist
```

What this does, with the defaults that apply:

* **Which repos**: all repositories of `your-github-username`. When that is the token's own
  account, private repositories are included. Forks are excluded by default.
* **What is copied**: branches, tags, LFS files, description/homepage/topics/default branch,
  releases with their files, and the wiki (`include: [git, lfs, settings, releases, wiki]`).
* **Names**: each repo keeps its name (`github-mirror/<repo>`).
* **Visibility**: mirrors are created **private**.
* **Schedule**: every hour when running as a service (`interval: 1h`).
* **Source deleted**: the mirror is kept (`on_source_deleted: keep`).

### Common variations

Only some repositories, by name pattern:

```yaml
    from:
      forge: github
      owner: your-github-username
      filter:
        include: ["project-*", "dotfiles"]
        exclude: ["*-old"]
        forks: true          # also include forks
        archived: false      # skip archived repos
```

A single repository:

```yaml
  - name: one-repo
    from: { forge: github, repo: your-github-username/project }
    to:   { forge: gitea,  repo: github-mirror/project }
```

Everything a user has starred (owner prefix avoids name clashes):

```yaml
  - name: my-stars
    from: { forge: github, starred_by: your-github-username }
    to:   { forge: gitea, owner: stars, name: "{{.Owner}}-{{.Name}}" }
    include: [git, lfs]       # just the code
```

The reverse direction, **Gitea to GitHub**, is the same shape with `from` and `to` swapped. Note
that GitHub cannot create organisations through the API, so a GitHub destination owner must be
your own user or an organisation that already exists.

```yaml
  - name: publish
    from: { forge: gitea,  repo: me/project }
    to:   { forge: github, repo: your-github-username/project }
    visibility: match-source  # public on GitHub only if public on Gitea
```

Two directions at once are two rules on **different** repositories; a rule pair that would
mirror a repository back into itself is rejected.

### If the destination repository already exists

gitmirror refuses to write into a repository it did not create, so a typo can never overwrite real
work. If you do want to turn an existing repository into a mirror, add `adopt: true` to the rule.
Its branches and tags that do not exist at the source **will be deleted**.

Every key, with defaults and limits, is in [configuration.md](configuration.md), and
[`config.example.yaml`](../config.example.yaml) is a commented example of all of them.

> **YAML gotcha:** inside `{ ... }` an environment reference must be quoted:
> `{ type: github, token: "${GITHUB_TOKEN}" }`. Unquoted, it is invalid YAML.

## 5. Give gitmirror the tokens

Never paste tokens into `config.yaml`. Use one of these:

**Environment variables** (as in the config above):

```sh
# bash
export GITHUB_TOKEN='ghp_...'
export GITEA_TOKEN='...'
```

```powershell
# PowerShell (current window only)
$env:GITHUB_TOKEN = 'ghp_...'
$env:GITEA_TOKEN  = '...'
```

**Token files**, better for services, because nothing has to be in the environment:

```yaml
forges:
  github: { type: github, token_file: /etc/gitmirror/github_token }
  gitea:  { type: gitea, url: https://git.example.com, token_file: /etc/gitmirror/gitea_token }
```

The file holds just the token (surrounding whitespace is ignored). Make it readable only by the
account that runs gitmirror (`chmod 600` on Linux).

## 6. Validate

First check the config and the local tools without any network access:

```sh
gitmirror validate --offline
```

```
config: ok (2 forges, 1 rules)
git: 2.47.1.windows.1
git-lfs: 3.6.0
```

Then the full check, which logs in to each forge and lists every rule's repositories:

```sh
gitmirror validate
```

```
config: ok (2 forges, 1 rules)
git: 2.47.1.windows.1
git-lfs: 3.6.0
forge github: ok as your-github-username
forge gitea: ok as you
rule my-github: complete=yes pairs=12
```

`pairs=12` is the number of repositories the rule will mirror. If a line says something else,
fix it before continuing. Typical messages:

* `environment variable GITHUB_TOKEN is not defined`: step 5 was not done in this terminal.
* `forge gitea: ... 401`: wrong or expired token, or the wrong `url`.
* `pairs=0`: wrong `owner`, or the filter excludes everything.

The exit code is `0` when everything is fine and `2` for a config error.

## 7. Preview with `plan`

`plan` shows exactly what a sync would do and changes nothing, neither on the forges nor in the
state database:

```sh
gitmirror plan
```

```
ok       github:you/project → gitea:github-mirror/project  prepare:ok git:ok lfs:skipped settings:ok releases:unchanged wiki:skipped  666ms
    - create repository gitea:github-mirror/project (private)
    - push all 1 refs
    - update settings of gitea:github-mirror/project: default branch
1 pairs: 1 ok, 0 partial, 0 failed, 0 aborted; 0 rule errors; 0 reconcile (dry run)
```

Each line is one **pair** (source → destination) followed by what each step would do. Read the
`- create repository` lines carefully on the first run: they tell you where every mirror will
appear. Limit it with `--rule NAME` or `--repo OWNER/NAME`.

## 8. Run the first sync

```sh
gitmirror sync
```

```
ok       github:you/project → gitea:github-mirror/project  prepare:ok git:ok lfs:unchanged settings:unchanged releases:unchanged wiki:skipped  4.616s
1 pairs: 1 ok, 0 partial, 0 failed, 0 aborted; 0 rule errors; 0 reconcile
```

The first run copies everything and can take a while for large repositories. Later runs only
transfer what changed; a run with no changes reports `git:unchanged` and takes a few seconds.

How to read the result:

| Pair status | Meaning |
|---|---|
| `ok` | Everything that applies was mirrored. |
| `partial` | Some steps worked but one (for example releases or the wiki) failed. Retried next run. |
| `failed` | The pair could not be mirrored (for example the destination could not be created or the push was rejected). The error is on the line. |
| `aborted` | A safety guard stopped the run before anything was changed (see [Troubleshooting](reference.md#troubleshooting)). |

Step values: `ok` (changed something), `unchanged`, `skipped` (not applicable, e.g. the source has
no wiki or LFS), `failed`.

Exit codes for scripts: `0` all ok, `1` something failed or was aborted, `2` config or usage
error, `3` partial.

Sync only part of the config with `--rule NAME` or `--repo OWNER/NAME` (both repeatable). Add
`--json` to any command for machine-readable output.

## 9. Check the status

```sh
gitmirror status --runs 5
```

```
PAIR                                              LAST SUCCESS         FAILURES  LAST ERROR
my-github: github:you/project → gitea:github-mirror/project  2026-11-14 11:42:29  0

recent runs (2):
ID  STARTED              TRIGGER  STATUS  PAIR                                              ERROR
2   2026-11-14 11:42:26  cli      ok      my-github: github:you/project → gitea:github-mirror/project
1   2026-11-14 11:42:21  cli      ok      my-github: github:you/project → gitea:github-mirror/project
```

`status` reads only the local state database, so it works offline. `--audit 20` also lists the
last 20 times gitmirror created, adopted, archived or deleted a destination.

## 10. Keep it running

Pick **one** of these. Only one gitmirror process can use a state directory at a time; a second
one fails with `another gitmirror process is using state dir ...`.

### A. `serve` in a terminal (quick test)

```sh
gitmirror serve
```

It syncs every rule at its `interval` (default hourly; the first runs start within 30 seconds of
startup, staggered across rules), receives webhooks,
and answers `GET /healthz` and `GET /readyz` on `server.listen` (default `:8080`). Prometheus
metrics are on `127.0.0.1:9090/metrics`. Stop it with Ctrl+C; running syncs get up to 10 minutes to
finish.

If port 8080 is already in use you get `listen on :8080: ... bind: ...`. Pick another port:

```yaml
server:
  listen: "127.0.0.1:18080"     # or ":18080" to accept connections from other machines
  metrics_listen: "127.0.0.1:19090"   # or "off"
```

### B. Docker Compose (recommended for servers)

The compose file in [`deploy/`](../deploy/docker-compose.yml) builds on Docker secrets, so tokens
live in files rather than environment variables.

```sh
cd deploy
# 1. Use the locally built image: in docker-compose.yml comment out the
#    `image: ghcr.io/OWNER/...` line and uncomment the `build:` block.
# 2. Token files (readable by the container user, uid 10001):
mkdir -p secrets
printf '%s' 'ghp_...'        > secrets/github_token
printf '%s' 'gitea-token'    > secrets/gitea_token
printf '%s' 'a-long-random-string' > secrets/webhook_secret
chmod 0444 secrets/*
# 3. Config: copy your config.yaml here and point it at the secret files:
#      forges:
#        github: { type: github, token_file: /run/secrets/github_token }
#        gitea:  { type: gitea, url: "https://git.example.com", token_file: /run/secrets/gitea_token }
#      server:
#        webhook_secret_file: /run/secrets/webhook_secret
cp ../config.yaml config.yaml
# 4. Check, then start:
docker compose run --rm gitmirror validate
docker compose up -d
docker compose logs -f gitmirror
```

Notes:

* `state_dir` in the config is ignored in the container; state lives in the `gitmirror-data`
  volume at `/data`. Do not delete that volume (see [step 12](#12-day-to-day-operation)).
* Port 8080 is published for webhooks and health checks. Put a TLS reverse proxy (Caddy, nginx,
  Traefik) in front of it before exposing it to the internet.
* For metrics inside Docker set `server.metrics_listen: ":9090"` and scrape it from your monitoring
  network. Do **not** publish port 9090: metrics are unauthenticated and name your repositories.
* `docker compose run --rm gitmirror plan` / `sync` / `status` work while the service is stopped.
  While it is running, use `docker compose exec gitmirror gitmirror status` instead (it reads the
  database without taking the lock).

### C. systemd (Linux, without Docker)

[`deploy/systemd/gitmirror.service`](../deploy/systemd/gitmirror.service) runs `serve` as a
hardened service under its own `gitmirror` user. The setup commands are at the top of that file:
install the binary to `/usr/local/bin`, create the user, put the config in
`/etc/gitmirror/config.yaml` with `state_dir: /var/lib/gitmirror`, put token files in
`/etc/gitmirror/credentials/`, then:

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now gitmirror
journalctl -u gitmirror -f
```

If you prefer periodic one-shot runs over a daemon (no webhooks), use
`gitmirror-sync.timer` + `gitmirror-sync.service` from the same folder **instead of** the
service. Edit `OnCalendar=` in the timer to change the schedule.

### D. Windows: scheduled task

On Windows without Docker, the simplest permanent setup is a scheduled `sync`:

1. Put `gitmirror.exe`, `config.yaml` and token files in one folder, e.g. `C:\gitmirror`.
2. In the config use **absolute** paths, because scheduled tasks do not start in that folder:
   `state_dir: C:/gitmirror/data`, `token_file: C:/gitmirror/github_token`, and so on.
3. Create an hourly task that runs as your user:

   ```powershell
   schtasks /Create /SC HOURLY /TN gitmirror /TR "C:\gitmirror\gitmirror.exe sync -c C:\gitmirror\config.yaml --log-format json"
   ```

The task's last result is gitmirror's exit code (`0` ok, `1` failed, `3` partial). Running
`gitmirror serve` as a Windows service needs a wrapper such as NSSM or WinSW; it has not been
tested.

## 11. Webhooks for instant mirroring (optional)

Without webhooks, a push reaches the mirror at the next scheduled run. With them, it arrives within
seconds. Webhooks need `serve` (option A, B or C above) reachable from the **source** forge.

1. Set a secret in the config (or `webhook_secret_file`):

   ```yaml
   server:
     webhook_secret: "${WEBHOOK_SECRET}"   # a long random string
   ```

2. Make `https://mirror.example.com/webhook/<forge>` reach gitmirror's `server.listen`, where
   `<forge>` is the source forge's key under `forges:` (in this guide: `/webhook/github`).
3. On the source forge add a webhook. On GitHub this goes on each repository, or once for a whole
   organisation (organisation **Settings → Webhooks**):
   * **Payload URL**: the URL from step 2
   * **Content type**: `application/json`
   * **Secret**: the same secret
   * **Events**: "Let me select individual events": Pushes, Branch or tag creation, Branch or tag
     deletion, Releases, Repositories, Wiki

   On Gitea/Forgejo: repository or organisation **Settings → Webhooks → Add Webhook → Gitea**, with
   the same URL, `POST`, `application/json`, the secret, and the Push, Create, Delete, Release,
   Repository and Wiki events.
4. Use the forge's "Redeliver" / "Test delivery" button. gitmirror answers `202` for an accepted
   event and `204` for an ignored one; `401` means the secret does not match, `404` that the forge
   name in the URL is wrong or no secret is configured.

**Gitea as the source on your own network:** Gitea blocks webhooks to private addresses by default.
If gitmirror runs on the same machine or LAN, set `[webhook] ALLOWED_HOST_LIST = private` in Gitea's
`app.ini` (or `GITEA__webhook__ALLOWED_HOST_LIST=private` in its container), otherwise deliveries
fail with a "not allowed" error in Gitea.

## 12. Day-to-day operation

* **Adding repositories**: with an `owner` or `starred_by` rule, new repositories are picked up
  automatically on the next run. For a new literal `repo`, add a rule.
* **Changing the config**: run `gitmirror validate` and `gitmirror plan`, then restart `serve`
  (it reads the config at startup).
* **When a run fails**: `gitmirror status` shows the last error per pair; the
  [Troubleshooting](reference.md#troubleshooting) section of the reference explains each message. A
  failed or partial pair is retried on the next run; you do not need to clean anything up.
* **If the safety guard trips** ("sync would delete N of M destination refs"): check the source. If
  the deletions are real, run `gitmirror sync --allow-prune --repo OWNER/NAME` once.
* **Removed source repositories** are kept by default. To archive or delete their mirrors, set
  `on_source_deleted: archive` or `delete` on the rule. Either only happens after the source has
  been missing for `deletion_grace` (default 24h), and `delete` only removes repositories gitmirror
  created itself.
* **Backups**: back up `state_dir/state.db`. It records which destination repositories gitmirror
  owns. If it is lost, gitmirror treats its own mirrors as foreign and refuses to update them until
  you set `adopt: true` on the rules. The git caches next to it can be rebuilt and need no backup.
* **Upgrading**: stop gitmirror, replace the binary or rebuild the image, start it again. The
  database is migrated automatically on start.
* **Monitoring**: alert on `gitmirror_pair_consecutive_failures > 2`, or on
  `time() - gitmirror_pair_last_success_timestamp_seconds` exceeding three intervals. All metrics are
  listed in the [reference](reference.md#endpoints-and-metrics).

---

## Try it locally first

This runs a throwaway Gitea in Docker and mirrors a repository between two accounts on it, so you
can see every command work without touching GitHub or your real server. Requires Docker and bash
(Git Bash works on Windows).

```sh
# 1. A throwaway Gitea on http://localhost:3311
docker run -d --name gitmirror-trial -p 127.0.0.1:3311:3000 \
  -e GITEA__security__INSTALL_LOCK=true \
  -e GITEA__server__ROOT_URL=http://localhost:3311/ \
  -e GITEA__database__DB_TYPE=sqlite3 \
  gitea/gitea:28.3.0
until curl -sf http://localhost:3311/api/healthz >/dev/null; do sleep 1; done

# 2. An admin user, a token, and a source repository with one commit
docker exec -u git gitmirror-trial gitea admin user create --admin \
  --username alice --password 'Trial-pass-123' --email alice@example.com --must-change-password=false
docker exec -u git gitmirror-trial gitea admin user generate-access-token \
  --username alice --token-name gitmirror --scopes all --raw > trial-token.txt
curl -s -H "Authorization: token $(cat trial-token.txt)" -H 'Content-Type: application/json' \
  -d '{"name":"demo","auto_init":true}' http://localhost:3311/api/v1/user/repos >/dev/null

# 3. Mirror alice/demo into a new organisation "mirrors"
cat > trial.yaml <<'EOF'
state_dir: ./trial-data
forges:
  home: { type: gitea, url: "http://localhost:3311", token_file: ./trial-token.txt }
rules:
  - name: demo
    from: { forge: home, repo: alice/demo }
    to:   { forge: home, repo: mirrors/demo }
EOF
gitmirror -c trial.yaml validate
gitmirror -c trial.yaml plan
gitmirror -c trial.yaml sync
gitmirror -c trial.yaml status --runs 5
```

Open http://localhost:3311/mirrors/demo (log in as `alice`) to see the private mirror. Push a
commit to `alice/demo` and run `sync` again to see it arrive.

Clean up:

```sh
docker rm -f gitmirror-trial
rm -rf trial-data trial-token.txt trial.yaml
```
