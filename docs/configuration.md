# Configuration reference

gitmirror reads one YAML file (default `config.yaml`; override with `-c/--config` or the
`GITMIRROR_CONFIG` environment variable). A fully commented sample is in
[`config.example.yaml`](../config.example.yaml).

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
| `rules` | list | none | at least one | One-way mirror rules. |

## `forges.<name>`

| Key | Type | Default | Validation | Description |
|---|---|---|---|---|
| `type` | string | none (required) | lower-cased and trimmed; one of `github`, `gitea` (`config.KnownForgeTypes`). Use `gitea` for Forgejo. | Provider type. |
| `url` | string | none | Required for `gitea`. If set: absolute `http`/`https` URL with a host. Trailing `/` trimmed. | Base URL. For `github` the provider default is `https://api.github.com` (set `url` for GitHub Enterprise). The normalised URL is recorded with the ownership of every destination created or adopted on this forge; changing it to another server makes those destinations unowned (they need `adopt: true` again). |
| `token` | string | none | Exactly one of `token` / `token_file` is required; both set is an error. | API token; also used for git over HTTPS. |
| `token_file` | string | none | File must exist and be non-empty after trimming. | Path of a file holding the token (Docker/Compose secrets, systemd credentials). |
| `username` | string | `x-access-token` (github), `git` (gitea) | none | Username for git-over-HTTP basic auth. |

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
| `webhook_secret` | string | none | Mutually exclusive with `webhook_secret_file` | Shared secret used to verify webhook signatures. If neither is set, `POST /webhook/<forge>` answers 404 and a warning is logged at start. |
| `webhook_secret_file` | string | none | File must exist and be non-empty after trimming | File holding the secret. |
| `ui_listen` | string | `127.0.0.1:8090` | `host:port` or `off`; must not use the same address as `listen` or `metrics_listen` (port `0` is exempt) | Listen address of the web UI, on its own listener. `off` disables the web UI. On first start (no admin password yet) `serve` logs a one-time setup code at WARN: open `<url>/setup` and enter it to choose the admin password. Keep it on `127.0.0.1` unless it is behind HTTPS. |
| `external_url` | string | none | Absolute `http`/`https` URL with a host; no user info, query or fragment. Trailing `/` trimmed. | The address people and forges use to reach gitmirror, e.g. `https://mirror.example.com`. Used for webhook URLs shown in the web UI and in the setup message; when it is `https`, web UI cookies are marked `Secure`. |

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
| `GITMIRROR_CONFIG` | Default for `-c/--config`. |
| `GITMIRROR_STATE_DIR` | Overrides `state_dir` when non-empty (env > config > default `./data`). Applied in `config.Parse`/`config.Load`. Set to `/data` in the Docker image. |
| any `${VAR}` named in the config | Interpolation. |

Git is run with a sanitised environment (`GIT_CONFIG*`, `GIT_DIR`, `GIT_ASKPASS`, tracing
variables and similar are removed from the inherited environment, terminal prompts are
disabled). By default git also ignores the system and global git configuration
(`GIT_CONFIG_NOSYSTEM=1`, `GIT_CONFIG_GLOBAL=<empty file>`, set after the stripping); see
[`git.use_host_config`](#git). Variables git reads directly, such as `HTTPS_PROXY`, `HTTP_PROXY`,
`NO_PROXY` and `GIT_SSL_CAINFO`, are passed through.
