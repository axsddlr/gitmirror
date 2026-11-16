# Configuration reference

The config file is **optional**. Sites (forges) and mirrors (rules) can be managed entirely in the
web UI (see [getting-started.md](getting-started.md)); the file is for settings the web UI does not
show, for keeping a setup in version control, and for tokens kept in files.

gitmirror reads one YAML file: `config.yaml` in the working directory by default, or the path
given with `-c/--config` or the `GITMIRROR_CONFIG` environment variable.

* When neither `-c` nor `GITMIRROR_CONFIG` is given and `config.yaml` does not exist, gitmirror
  uses the built-in defaults, exactly as for an empty file (environment overrides such as
  `GITMIRROR_STATE_DIR` included). A path given with `-c` or `GITMIRROR_CONFIG` must exist.
* The Docker image does **not** set `GITMIRROR_CONFIG`. Its working directory is `/config`, so it
  reads `/config/config.yaml` when you mount one and uses the defaults when you do not.
* Forges and rules from the file and from the web UI are **merged and validated together**: names
  must be unique across both (a web UI entry cannot reuse a name from the file), and the loop,
  duplicate-destination and self-mirror checks below cover both. Entries from the file are shown
  read-only in the web UI. The `defaults` section applies to rules from both.
* The file is read when a command starts; restart `serve` after changing it. Changes made in the
  web UI apply at once.

A fully commented sample is in [`config.example.yaml`](../config.example.yaml). A step-by-step
example is in [Walkthrough: a first config file](#walkthrough-a-first-config-file).

Rules of the loader (`internal/config`):

1. **Strict decoding.** Unknown keys are errors.
2. **Interpolation** of every decoded string value: `${VAR}` (error if `VAR` is unset),
   `${VAR:-default}` (default used when unset or empty), `$${` for a literal `${`. Because
   expansion happens after YAML parsing, a value can never inject YAML structure. In YAML flow
   maps (`{ ... }`) you must quote the value: `token: "${GITHUB_TOKEN}"`; an unquoted `${...}`
   inside `{ }` is invalid YAML.
3. **Defaults** are applied, then `token_file` / `webhook_secret_file` are read (contents are
   whitespace-trimmed; an empty file is an error).
4. **Validation** reports every problem at once, each prefixed with its location.
5. Secrets never appear in error messages, `String()` output or logs.

## Top level

| Key | Type | Default | Validation | Description |
|---|---|---|---|---|
| `state_dir` | string | `./data` | none | Directory for the sqlite database, the bare git cache and the lock file (`gitmirror.lock`). Relative paths resolve against the working directory. The environment variable `GITMIRROR_STATE_DIR`, when set and non-empty, overrides this (precedence: env > config > default `./data`); the Docker image sets it to `/data`. Use `/var/lib/gitmirror` under systemd. |
| `concurrency` | int | `4` (also when `0` or omitted) | 1..64 | Number of repo pairs processed in parallel. |
| `forges` | map name -> forge | none | names match `^[a-z0-9][a-z0-9_-]*$` | Forge connections referenced by rules. |
| `defaults` | object | see below | see below | Settings every rule inherits. |
| `server` | object | see below | see below | HTTP listener / webhook receiver for `serve`. |
| `git` | object | see below | see below | How git and git-lfs are run. |
| `rules` | list | none | may be empty | One-way mirror rules. Rules can also come from the web UI. `serve` idles (with a warning) while there are none; `sync`, `plan` and `validate` (without `--offline`) exit with code 2 and `no mirror rules configured: add one in the web UI or in the config file`. |

## `forges.<name>`

| Key | Type | Default | Validation | Description |
|---|---|---|---|---|
| `type` | string | none (required) | lower-cased and trimmed; one of `github`, `gitea` (`config.KnownForgeTypes`). Use `gitea` for Forgejo (the web UI's "Forgejo" kind is stored as `gitea` too). | Provider type. |
| `url` | string | none | Required for `gitea`. If set: absolute `http`/`https` URL with a host. Trailing `/` trimmed. | Base URL. For `github` the provider default is `https://api.github.com` (set `url` for GitHub Enterprise). The normalised URL is recorded with the ownership of every destination created or adopted on this forge; changing it to another server makes those destinations unowned (they need `adopt: true` again). |
| `token` | string | none | Exactly one of `token` / `token_file` is required; both set is an error. | API token; also used for git over HTTPS. |
| `token_file` | string | none | File must exist and be non-empty after trimming. | Path of a file holding the token (Docker/Compose secrets, systemd credentials). |
| `username` | string | `x-access-token` (github), `git` (gitea) | none | Username for git-over-HTTP basic auth. |

Forges added in the web UI ("sites") are stored in the state database with their token encrypted
(AES-256-GCM, key in `<state_dir>/secret.key`); `token_file` is not available for them.

## `defaults`

All keys can be overridden per rule.

| Key | Type | Default | Validation | Description |
|---|---|---|---|---|
| `interval` | duration string | `1h` | Go duration, >= `1m` | How often `serve` runs each rule (jittered +-10%; first runs are staggered over up to 30s). `sync` ignores it. |
| `include` | list of string | `[git, lfs, settings, releases, wiki]` | non-empty; each one of `git`, `lfs`, `settings`, `releases`, `wiki`; `lfs`, `releases`, `wiki` require `git`; duplicates removed | What to mirror. |
| `visibility` | string | `private` | `private` or `match-source` | Visibility of destination repos. `private` keeps destinations private; `match-source` is public only if the source is public. A destination is never made more public than its source. |
| `on_source_deleted` | string | `keep` | `keep`, `archive`, `delete` | Action once a source has been gone for `deletion_grace`. `delete` only deletes destinations gitmirror created (adopted ones, and providers that cannot delete, are archived instead). |
| `deletion_grace` | duration string | `24h` | Go duration, >= `1h` | The source must continuously report "not found" this long before `on_source_deleted` is applied. If two consecutive checks are more than max(2 x `interval`, 1h) apart the grace period restarts. |
| `delete_missing_releases` | bool | `false` | none | Delete destination releases (ones gitmirror created) that no longer exist at the source. |
| `max_ref_deletions` | int | `5` | `>= 0` | Mass ref deletion guard, count part: a run is aborted (status `aborted`, nothing pushed) when it would delete more than this many destination branches/tags **and** more than `max_ref_deletions_percent` of them. `sync --allow-prune` bypasses the guard for one run. |
| `max_ref_deletions_percent` | int | `50` | `1..100` | Mass ref deletion guard, percentage part (of the destination's branches and tags before the push). |

## `server`

| Key | Type | Default | Validation | Description |
|---|---|---|---|---|
| `listen` | string | `:8080` | none (bind errors surface at start) | Listen address of `serve`. |
| `webhook_secret` | string | none | Mutually exclusive with `webhook_secret_file` | Shared secret used to verify webhook signatures. If neither is set and the web UI has none either, `POST /webhook/<forge>` answers 404 and a warning is logged at start. Opening the web UI's Webhooks page creates a random secret (stored encrypted in the state database) when the file has none; when the file sets one, the web UI uses it and does not show it. |
| `webhook_secret_file` | string | none | File must exist and be non-empty after trimming | File holding the secret. |
| `ui_listen` | string | `127.0.0.1:8090` | `host:port` or `off` (case-insensitive); must not use the same address as `listen` or `metrics_listen` (port `0` is exempt) | Listen address of the web UI, on its own listener. `off` disables the web UI. The environment variable `GITMIRROR_UI_LISTEN`, when set and non-empty, overrides this (precedence: env > config > default); the Docker image sets it to `:8090` so the UI is reachable through a published port. On first start (no admin password yet) `serve` logs a one-time setup code at WARN: open `<url>/setup` and enter it to choose the admin password. Keep it on `127.0.0.1` unless it is behind HTTPS. See [Web UI](reference.md#web-ui). |
| `external_url` | string | none | Absolute `http`/`https` URL with a host; no user info, query or fragment. Trailing `/` trimmed. | The address people and forges use to reach gitmirror, e.g. `https://mirror.example.com`. Used for webhook URLs (`<external_url>/webhook/<forge>`) shown in the web UI and in the setup message; when it is `https`, web UI cookies are marked `Secure`. The web UI's **Public address** (Webhooks page) sets the same value, stored in the state database, when the file has none; when the file sets it, the web UI shows it read-only. |

## `git`

| Key | Type | Default | Validation | Description |
|---|---|---|---|---|
| `use_host_config` | bool | `false` | none | When `false`, every git and git-lfs command runs with `GIT_CONFIG_NOSYSTEM=1` and `GIT_CONFIG_GLOBAL` pointing at an empty file, so the host's `/etc/gitconfig`, `~/.gitconfig` and XDG git config are ignored (a host `url.<x>.insteadOf` cannot redirect a remote, a global `http.extraHeader` is not sent to the forges, `core.hooksPath` / `credential.helper` cannot run code). With isolation on, configure proxies through the `HTTPS_PROXY` / `HTTP_PROXY` / `NO_PROXY` environment variables and a custom CA bundle through `GIT_SSL_CAINFO` (these are passed through). Set `true` to let git read the host configuration again, e.g. when it needs `http.sslBackend=schannel` on a Windows corporate setup. |

## `rules[]`

| Key | Type | Default | Validation | Description |
|---|---|---|---|---|
| `name` | string | none (required) | `^[a-z0-9][a-z0-9_-]*$`, unique | Rule name; used by `--rule`, metric labels, logs. |
| `from` | object | required | see below | Source selection. |
| `to` | object | required | see below | Destination. |
| `interval` | duration string | `defaults.interval` | as above | Override. |
| `include` | list | `defaults.include` | as above (an explicitly empty list is an error) | Override. |
| `visibility` | string | `defaults.visibility` | as above | Override. |
| `on_source_deleted` | string | `defaults.on_source_deleted` | as above | Override. |
| `deletion_grace` | duration string | `defaults.deletion_grace` | as above | Override. |
| `delete_missing_releases` | bool | `defaults.delete_missing_releases` | none | Override (an explicit `false` overrides a `true` default). |
| `max_ref_deletions` | int | `defaults.max_ref_deletions` | as above | Override (an explicit `0` overrides the default). |
| `max_ref_deletions_percent` | int | `defaults.max_ref_deletions_percent` | as above | Override. |
| `adopt` | bool | `false` | none (rule-level only; not in `defaults`) | Allow writing into destination repos that gitmirror did not create. Without it such a destination makes the pair fail. Adopted destinations are pruned like any mirror (refs missing at the source are removed) and are archived rather than deleted. Also needed to keep mirroring into a destination that was replaced (deleted and re-created, restored from a backup) or whose forge `url` changed; the new repository id is then recorded. With `adopt`, destination releases with a source release's tag are updated even if gitmirror did not create them. |
| `allow_publish` | bool | `false` | none (rule-level only; not in `defaults`) | With `visibility: match-source`, allow an adopted destination that is private to be made public when its source is public. Without it an adopted destination's visibility is only ever narrowed. Destinations gitmirror created are not affected. |

### `rules[].from`

| Key | Type | Default | Validation | Description |
|---|---|---|---|---|
| `forge` | string | none (required) | must be a key of `forges` | Source forge. |
| `owner` | string | none | exactly one of `owner`, `starred_by`, `repo` | Every repository of this user/organisation. |
| `starred_by` | string | none | as above | Every repository starred by this user. |
| `repo` | string | none | as above; must be `owner/name` | A single repository. `filter` is not allowed with it. |
| `filter` | object | see below | only with `owner` / `starred_by` | Narrows a multi-repo selection. |

### `rules[].from.filter`

| Key | Type | Default | Validation | Description |
|---|---|---|---|---|
| `forks` | bool | `false` | none | Include forks. |
| `archived` | bool | `true` | none | Include archived repositories. |
| `private` | bool | `true` | none | Include private repositories. |
| `include` | list of glob | `["*"]` | valid `path.Match` patterns | Repo-name patterns; a repo must match one of them. |
| `exclude` | list of glob | none | valid `path.Match` patterns | Repo-name patterns that remove a repo. |

### `rules[].to`

| Key | Type | Default | Validation | Description |
|---|---|---|---|---|
| `forge` | string | none (required) | must be a key of `forges` | Destination forge. |
| `owner` | string (template) | none | exactly one of `owner` / `repo` | Destination owner. `text/template` with `{{.Owner}}` and `{{.Name}}` (the source repo). |
| `name` | string (template) | `{{.Name}}` | only with `owner`; not allowed with `repo` | Destination repo name. |
| `repo` | string | none | `owner/name`, literal | Destination repository (use with `from.repo` for exactly one pair). |

Each rendered owner/name must match `^[A-Za-z0-9._-]+$`, not be `.` or `..`, and not end in `.git`.
Missing template keys are errors.

## Static safety checks

The config loader also checks, for rules where **both** `from.repo` and `to.repo` are literal:

* source and destination are the same repo (compared case-insensitively per forge),
* two rules write to the same literal destination,
* two rules mirror each other in opposite directions (a loop).

Templated destinations (`to.owner`) can collide only at runtime; the resolver drops such pairs
(self-mirror, duplicate destination, loop) with a rule error, and reconcile never treats a
dropped pair as a deleted source.

## Environment variables read by gitmirror

| Variable | Used for |
|---|---|
| `GITMIRROR_CONFIG` | Default for `-c/--config`. A path given this way must exist. Not set in the Docker image (it runs in `/config` and reads `config.yaml` from there when present). |
| `GITMIRROR_STATE_DIR` | Overrides `state_dir` when non-empty (env > config > default `./data`). Applied in `config.Parse`/`config.Load`, also when there is no config file. Set to `/data` in the Docker image. |
| `GITMIRROR_UI_LISTEN` | Overrides `server.ui_listen` when non-empty (env > config > default `127.0.0.1:8090`); `off` disables the web UI. Applied like `GITMIRROR_STATE_DIR`. Set to `:8090` in the Docker image. |
| any `${VAR}` named in the config | Interpolation. |

Git is run with a sanitised environment (`GIT_CONFIG*`, `GIT_DIR`, `GIT_ASKPASS`, tracing
variables and similar are removed from the inherited environment, terminal prompts are
disabled). By default git also ignores the system and global git configuration
(`GIT_CONFIG_NOSYSTEM=1`, `GIT_CONFIG_GLOBAL=<empty file>`, set after the stripping); see
[`git.use_host_config`](#git). Variables git reads directly, such as `HTTPS_PROXY`, `HTTP_PROXY`,
`NO_PROXY` and `GIT_SSL_CAINFO`, are passed through.

## Walkthrough: a first config file

This is the config-file way of doing what [getting-started.md](getting-started.md) does in the web
UI. Tokens come from [getting-started.md, step 5](getting-started.md#5-create-access-tokens).

### A minimal config

Create `config.yaml` in the folder gitmirror starts in (or anywhere, and pass
`-c path/to/config.yaml`). This one mirrors **every repository you own on GitHub** into an
organisation called `github-mirror` on your Gitea:

```yaml
# Where gitmirror keeps its database, git caches and lock file.
state_dir: ./data

forges:
  github:
    type: github
    token: "${GITHUB_TOKEN}"     # read from the environment, see below
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

The reverse direction, **Gitea to GitHub**, is the same shape with `from` and `to` swapped. GitHub
cannot create organisations through the API, so a GitHub destination owner must be your own user or
an organisation that already exists.

```yaml
  - name: publish
    from: { forge: gitea,  repo: me/project }
    to:   { forge: github, repo: your-github-username/project }
    visibility: match-source  # public on GitHub only if public on Gitea
```

Two directions at once are two rules on **different** repositories; a rule pair that would mirror a
repository back into itself is rejected.

**If the destination repository already exists:** gitmirror refuses to write into a repository it
did not create, so a typo can never overwrite real work. To turn an existing repository into a
mirror, add `adopt: true` to the rule. Its branches and tags that do not exist at the source
**will be deleted**.

> **YAML gotcha:** inside `{ ... }` an environment reference must be quoted:
> `{ type: github, token: "${GITHUB_TOKEN}" }`. Unquoted, it is invalid YAML.

### Giving gitmirror the tokens

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

### Validate

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

`pairs=12` is the number of repositories the rule will mirror. If a line says something else, fix
it before continuing. Typical messages:

* `environment variable GITHUB_TOKEN is not defined`: the token was not exported in this terminal.
* `forge gitea: ... 401`: wrong or expired token, or the wrong `url`.
* `pairs=0`: wrong `owner`, or the filter excludes everything.

The exit code is `0` when everything is fine and `2` for a config error. Then preview with
`gitmirror plan`, copy once with `gitmirror sync`, and check with `gitmirror status`; what they
print is explained in the reference under [Reading the output](reference.md#reading-the-output).
Run `serve` to keep the mirrors up to date.

### Docker Compose with token files

[`docker-compose.yml`](../docker-compose.yml) has the config mount and Docker
secrets commented out. To use them, in the repository folder:

```sh
# 1. Token files (readable by the container user, uid 10001):
mkdir -p secrets
printf '%s' 'ghp_...'        > secrets/github_token
printf '%s' 'gitea-token'    > secrets/gitea_token
printf '%s' 'a-long-random-string' > secrets/webhook_secret
chmod 0444 secrets/*
# 2. Config: copy your config.yaml here and point it at the secret files:
#      forges:
#        github: { type: github, token_file: /run/secrets/github_token }
#        gitea:  { type: gitea, url: "https://git.example.com", token_file: /run/secrets/gitea_token }
#      server:
#        webhook_secret_file: /run/secrets/webhook_secret
cp config.example.yaml config.yaml    # then edit it
# 3. In docker-compose.yml uncomment the config.yaml volume line, the service's
#    `secrets:` list and the top-level `secrets:` block.
# 4. Check, then start:
docker compose run --rm gitmirror validate
docker compose up -d --build
```

`state_dir` in the config is ignored in the container (`GITMIRROR_STATE_DIR=/data` wins).
`docker compose run --rm gitmirror plan` / `sync` / `status` work while the service is stopped;
while it runs, use `docker compose exec gitmirror gitmirror status` (it reads the database without
taking the lock).

### systemd with credentials

[`deploy/systemd/gitmirror.service`](../deploy/systemd/gitmirror.service) expects the config at
`/etc/gitmirror/config.yaml` with `state_dir: /var/lib/gitmirror`, and token files in
`/etc/gitmirror/credentials/` that systemd hands to the service (`LoadCredential=`; refer to them
as `token_file: "${CREDENTIALS_DIRECTORY}/github_token"`). The setup commands are at the top of
that file. For periodic one-shot runs instead of a daemon (no webhooks, no web UI), use
`gitmirror-sync.timer` + `gitmirror-sync.service` from the same folder **instead of** the service,
and edit `OnCalendar=` in the timer to change the schedule.

### Windows: scheduled sync

Instead of running `serve` (see [getting-started.md](getting-started.md#c-windows-start-at-sign-in)),
you can run a one-shot `sync` every hour (no webhooks, and no web UI while it is not running):

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
