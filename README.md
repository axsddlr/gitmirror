# gitmirror

gitmirror keeps a copy of your code projects on a second site, and keeps that copy up to date for you.

For example, it can:

* copy your GitHub projects to your own Gitea or Forgejo server, as a backup
* publish a project from your Gitea server to GitHub

You tell it what to copy and where. It then checks for changes on a schedule, or straight away when
you push, and copies them over.

## What gets copied

* All the code, including every branch and tag
* Large files stored with Git LFS
* The project's description, topics and visibility settings
* Releases and their downloads
* The project's wiki

Issues and pull requests are **not** copied.

## It won't wreck your data

* It only ever changes copies **it made itself**. It never touches a project you created by hand,
  unless you say it may.
* If the original suddenly looks empty or deleted, gitmirror stops instead of copying that over.
* Copies are **private** by default. It never makes a private project public.
* `gitmirror plan` shows exactly what it would do, without changing anything.

## Getting started

Follow the **[step-by-step guide](docs/getting-started.md)**. In short:

1. **Build gitmirror** (you need Go and git installed).
2. **Create an access token** on each site, so gitmirror can log in.
3. **Write a config file** that says what to copy and where. Start from
   [`config.example.yaml`](config.example.yaml).
4. **Check and preview:** `gitmirror validate`, then `gitmirror plan`.
5. **Copy:** `gitmirror sync` copies everything once. `gitmirror serve` keeps running and keeps the
   copies up to date.

Want to try it safely first? The guide has a 5-minute trial that uses a throwaway Gitea server on
your own computer.

## Everyday commands

| Command | What it does |
|---|---|
| `gitmirror validate` | Checks your config and tokens |
| `gitmirror plan` | Shows what would change, without changing anything |
| `gitmirror sync` | Copies everything once |
| `gitmirror serve` | Keeps running and copies changes as they happen |
| `gitmirror status` | Shows when each project was last copied, and whether it worked |

## Something went wrong?

Most error messages explain what to do. The longer explanations are in
[Troubleshooting](docs/reference.md#troubleshooting).

## More detail

* [Getting started](docs/getting-started.md): full setup, running it all the time, webhooks
* [Reference](docs/reference.md): safety rules, every command and option, token permissions,
  webhooks, monitoring, Docker and systemd, troubleshooting
* [Configuration](docs/configuration.md): every setting in the config file
* [Architecture](docs/architecture.md): how it works inside, and how to add another site such as
  GitLab
