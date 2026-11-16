# gitmirror

gitmirror keeps a copy of your code projects on a second site, and keeps that copy up to date for you.

For example, it can:

* copy your GitHub projects to your own Gitea or Forgejo server, as a backup
* publish a project from your Gitea server to GitHub

You set it up in your web browser: add your sites, then choose what to copy and where. It then
checks for changes on a schedule, or straight away when you push, and copies them over.

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
* Before you save a mirror, it shows you exactly what it would do.

## Getting started

Follow the **[step-by-step guide](docs/getting-started.md)**. In short:

1. **Start gitmirror**, either the program (`gitmirror serve`) or the Docker image. The guide shows
   how to build both.
2. **Open http://localhost:8090** in your browser.
3. **Enter the setup code** from gitmirror's log (the line that starts with `web UI setup`), and
   choose a password.
4. **Add your sites** on the **Sites** page. You need an access token from each site.
5. **Set up a mirror** on the **Mirrors** page. You see what it would do before anything is saved.

The **Dashboard** then shows how every copy is doing.

Want to try it safely first? The guide has a short trial that uses a throwaway Gitea server on
your own computer.

You do not need a config file. If you prefer one, see [Configuration](docs/configuration.md).

## Everyday commands

| Command | What it does |
|---|---|
| `gitmirror serve` | Runs gitmirror and its web pages, and keeps the copies up to date |
| `gitmirror status` | Shows when each project was last copied, and whether it worked |
| `gitmirror reset-admin` | Removes a forgotten web password (stop `serve` first) |
| `gitmirror plan` | Shows what would change, without changing anything |
| `gitmirror sync` | Copies everything once |
| `gitmirror validate` | Checks your setup and tokens |

## Something went wrong?

Most messages explain what to do. The longer explanations are in
[Troubleshooting](docs/reference.md#troubleshooting).

## More detail

* [Getting started](docs/getting-started.md): full setup, running it all the time, webhooks, backups
* [Reference](docs/reference.md): the web pages and their security, safety rules, every command,
  token permissions, webhooks, monitoring, Docker and systemd, troubleshooting
* [Configuration](docs/configuration.md): the optional config file, setting by setting
* [Architecture](docs/architecture.md): how it works inside, and how to add another site such as
  GitLab
