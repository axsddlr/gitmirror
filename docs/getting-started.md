# Getting started

This guide takes you from nothing to a running mirror, step by step. You do everything in your web
browser, on gitmirror's own web pages. The example copies **GitHub to a self-hosted Gitea**; the
other directions work the same way.

If you only want to see gitmirror work before you touch your real accounts, start with
[Try it locally first](#try-it-locally-first).

Two words are used throughout:

* A **site** is a place where your code lives: GitHub, or your own Gitea or Forgejo server.
* A **mirror** copies repositories from one site to another, one way, and keeps the copies up to
  date.

(The optional config file calls them *forges* and *rules*.)

Contents:

1. [What you need](#1-what-you-need)
2. [Get gitmirror](#2-get-gitmirror)
3. [Start gitmirror](#3-start-gitmirror)
4. [Choose your password](#4-choose-your-password)
5. [Create access tokens](#5-create-access-tokens)
6. [Add your sites](#6-add-your-sites)
7. [Set up a mirror](#7-set-up-a-mirror)
8. [Watch the dashboard](#8-watch-the-dashboard)
9. [Webhooks for instant copies (optional)](#9-webhooks-for-instant-copies-optional)
10. [Keep it running](#10-keep-it-running)
11. [Backups](#11-backups)
12. [Forgot your password?](#12-forgot-your-password)
13. [When something goes wrong](#13-when-something-goes-wrong)

Commands are shown for **bash** (Linux, macOS, Git Bash on Windows) and **PowerShell** (Windows)
where they differ.

---

## 1. What you need

| Requirement | Why | Check |
|---|---|---|
| **git 2.32 or later** | gitmirror uses git to copy the code. | `git --version` |
| **git-lfs** (optional) | Only for large files stored with Git LFS. Git for Windows includes it. Without it, large files are skipped with a warning. | `git lfs version` |
| **Go 1.26** (to build) | There are no ready-made downloads yet, so you build gitmirror yourself. Any Go 1.21 or later downloads Go 1.26 by itself. | `go version` |
| **Docker** (instead of Go and git) | The Docker image contains everything. | `docker version` |
| A **GitHub** account and a **Gitea or Forgejo** server you can sign in to | The two ends of the mirror. | |

gitmirror needs to reach both sites over HTTPS. It does not use SSH.

## 2. Get gitmirror

Pick **one** of these.

### Option A: build the program

In the folder where you downloaded gitmirror's source code:

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

You should see something like `gitmirror dev (commit e2ccfac..., go1.26.11)`. Put the program
somewhere on your `PATH`, or call it by its full path. The rest of this guide writes `gitmirror`.

`make build` does the same with version information included and writes `bin/gitmirror`.

### Option B: build the Docker image

```sh
docker build -t gitmirror .
docker run --rm gitmirror version
```

## 3. Start gitmirror

### If you built the program

Make a folder for gitmirror, go into it, and start it:

```sh
# bash
mkdir -p ~/gitmirror && cd ~/gitmirror
gitmirror serve
```

```powershell
# PowerShell
New-Item -ItemType Directory -Force C:\gitmirror; Set-Location C:\gitmirror
gitmirror serve
```

gitmirror keeps everything it needs in a `data` folder inside the folder you started it from.
Always start it from the same folder. Leave the window open; Ctrl+C stops it.

### If you use Docker Compose (recommended for servers)

In the folder you downloaded gitmirror into:

```sh
docker compose up -d --build
docker compose logs gitmirror
```

Compose builds gitmirror from the code you downloaded. It publishes the web pages on
`127.0.0.1:8090`, so only this computer can open them, and webhooks on port `8080`. If either port
is already used on your computer, pick others, for example
`GITMIRROR_UI_PORT=8091 GITMIRROR_WEBHOOK_PORT=8085 docker compose up -d --build`, or put those two
lines in a `.env` file next to `docker-compose.yml`.
Everything you set up is kept in the `gitmirror-data` volume. To update later: `git pull`, then
`docker compose up -d --build` again.

### If you use plain Docker

```sh
docker run -d --name gitmirror --restart unless-stopped --stop-timeout 660 \
  -p 127.0.0.1:8090:8090 -p 8080:8080 \
  -v gitmirror-data:/data \
  gitmirror
docker logs gitmirror
```

### Find the setup code

The first time gitmirror starts, it writes a one-time **setup code** to its log. Look for the line
that starts with `web UI setup`:

```
level=WARN msg="web UI setup: open http://127.0.0.1:8090/setup and enter setup code ABCD-EFGH-JKMN"
```

In Docker the address in that line looks like `http://[::]:8090/setup`. Ignore the address and use
`http://localhost:8090` in your browser.

The code lives only as long as gitmirror runs. If you restart gitmirror before you finish the next
step, it writes a new code; use the newest one.

> **Port already in use?** If the log says `listen on :8080: ... bind: ...`, another program uses
> port 8080. If it says `listen for the web UI on 127.0.0.1:8090: ...`, port 8090 is taken. See
> [Use other ports](#use-other-ports).

## 4. Choose your password

1. Open **http://localhost:8090** in your browser. You see **Welcome to gitmirror**.
2. In **Setup code**, enter the code from the log.
3. In **New password**, choose a password of at least 12 characters, and type it again in
   **Type the password again**.
4. Click **Save password and continue**.

You are now signed in. The menu at the top has **Dashboard**, **Sites**, **Mirrors**, **Webhooks**
and **Settings**. Next time, sign in on the **Sign in** page with this password.

To change the password later, open **Settings** and use **Change password**. Every other browser
that is signed in is then signed out.

## 5. Create access tokens

gitmirror signs in to each site with an access token. A site you only copy **from** needs read
access. A site you copy **to** needs write access, because gitmirror creates and updates the copies
there. The **Add a site** page shows the same list under **Which token permissions are needed**.
Full tables are in the reference under [Token permissions](reference.md#token-permissions).

### GitHub (copy from, in this guide)

1. GitHub → your avatar → **Settings** → **Developer settings** → **Personal access tokens** →
   **Tokens (classic)** → **Generate new token (classic)**.
2. Name it `gitmirror`, choose an expiry, and tick **`repo`**. If every repository you copy is
   public, `public_repo` is enough.
3. If GitHub is a site you copy **to** (Gitea to GitHub), also tick **`workflow`**, and
   **`delete_repo`** if you plan to use `on_source_deleted: delete` in the config file.
4. If you want gitmirror to set up webhooks for you (step 9), also tick **`admin:repo_hook`**, and
   **`admin:org_hook`** for organisations.
5. Copy the token now. GitHub shows it only once.

Classic tokens are the safe choice. Fine-grained tokens work for reading but are limited to one
account or organisation; see [Token permissions](reference.md#token-permissions) if you prefer them.

### Gitea or Forgejo (copy to, in this guide)

1. Gitea → your avatar → **Settings** → **Applications** → **Manage Access Tokens**.
2. Name it `gitmirror` and under **Select permissions** set:
   * **repository**: Read and Write
   * **user**: Read
   * **organization**: Read and Write (needed when copies go into an organisation, or when
     gitmirror should create that organisation for you)
3. **Generate Token** and copy it.

Creating organisations also requires your Gitea account to be allowed to create them (admins
always are). If it is not, create the organisation on Gitea first.

## 6. Add your sites

1. Open **Sites** and click **Add a site**.
2. Under **Kind of site**, choose **GitHub**, **Gitea** or **Forgejo**.
3. **Address**: for GitHub leave it empty (or enter your GitHub Enterprise address). For Gitea and
   Forgejo enter the address you open in the browser, like `https://git.example.com`.
4. **Token**: paste the token from step 5.
5. **Short name (optional)**: leave it empty, and gitmirror picks one from the address, like
   `github` or `git-example-com`.
6. Click **Test connection and save**. gitmirror signs in with the token before it saves anything.
   If that fails, the page says why.

Do this for each site. The **Sites** page then lists them, with the account each token signs in as
under **Signed in as**.

Tokens are stored encrypted in gitmirror's data folder, next to a key file called `secret.key`. See
[Backups](#11-backups).

## 7. Set up a mirror

Open **Mirrors** and click **New mirror**. The form has five steps. **Back** takes you to the
previous step; nothing is saved until the last one.

1. **Copy from**
   * **Site to copy from**: for example `github`.
   * **What to copy**: **Choose repositories** (pick from a list), **Everything from an account or
     organisation** (also copies repositories added there later) or **Everything starred by an
     account**.
   * **Account or organisation (optional)**: whose repositories to copy. Leave it empty to use the
     account the token signs in as.
   * Click **Next**.
2. **Repositories**
   * With **Choose repositories**: tick the repositories you want. **Search by name** and
     **Show forks** / **Show archived repositories** help you find them.
   * Otherwise: choose whether to **Include forks** and **Include archived repositories**.
   * Click **Next**.
3. **Copy to**
   * **Site to copy to**: for example your Gitea site. It can also be the same site.
   * **Account or organisation to copy into (optional)**: for example `github-mirror`. Leave it
     empty to use the account the token signs in as. On Gitea and Forgejo a missing organisation is
     created.
   * **Names of the copies**: **Keep the same names**, or **Prefix with the original owner** (avoids
     clashes when copying from several owners). With one picked repository you can also choose
     **A name of my own**.
   * Click **Next**.
4. **Options**. The safe choices are already selected.
   * **What to copy**: the code is always copied. Untick **Large files (LFS)**, **Description and
     settings**, **Releases** or **Wiki** if you do not want them.
   * **Who can see new copies**: **Private** (the default), or **Same as the original** (never more
     public than the original).
   * **How often to copy**: for example **Every hour**.
   * **Existing repositories**: leave **Allow copying into repositories that already exist** off
     unless you mean it. When it is on, a repository with the same name at the destination is
     replaced to match the original, and changes made only there are lost.
   * **Name of this mirror**: a short name, like `my-github`.
   * Click **Preview**.
5. **Check and save**. gitmirror does a trial run and lists, for each repository, what would
   happen: **Will be created**, **Will be updated**, **Will be skipped**, **Replaces existing** or
   **Conflict**. Nothing has been copied yet. If it looks right, click **Save mirror**.

The mirror starts copying straight away; you do not have to restart gitmirror. Changes on the
**Sites** and **Mirrors** pages always apply at once.

## 8. Watch the dashboard

The **Dashboard** shows every mirror and each copy in it: the **Original**, the **Copy**, its
**Status** and when it was **Last copied**.

* **Sync now** copies one mirror at once, without waiting for the schedule. **Sync everything now**
  does all of them.
* **Details** shows the recent copies of one repository, step by step. If something went wrong,
  the problem is explained in plain words; **Technical details** shows the exact error.
* The first copy of a large repository can take a while. Later copies only transfer what changed.

From the command line, `gitmirror status` shows the same information (run it from the folder
gitmirror runs in, or `docker exec gitmirror gitmirror status` in Docker).

## 9. Webhooks for instant copies (optional)

Without webhooks, a push reaches the copy at the next scheduled time. With webhooks, the site tells
gitmirror about the push and the copy is updated within seconds.

Webhooks need one thing first: the site you copy **from** must be able to reach gitmirror.

* For **GitHub**, that means from the internet. Usually you put an HTTPS reverse proxy (Caddy,
  nginx, Traefik) in front of gitmirror's webhook port and give it a name like
  `https://mirror.example.com`.
* For a Gitea or Forgejo server on your own network, an address on that network is enough.
* Webhooks go to gitmirror's **webhook port**, `8080` by default, not to port 8090 of the web pages.

Then:

1. Open **Webhooks**.
2. Under **Public address**, enter the address the site can reach, like
   `https://mirror.example.com`, and click **Save address**.
3. Under **Webhook secret**, gitmirror has already created a secret. You only need it if you add
   webhooks by hand (**Show the secret**).
4. Under **Sites that mirrors copy from**, click **Set up webhooks for me** for each site. gitmirror
   adds one webhook per organisation when a mirror copies all of it, otherwise one per repository,
   and skips webhooks that already exist. The token needs the webhook permissions from step 5.
5. The result page lists each repository or organisation and what happened. If some say
   **Not done yet**, go back and click **Set up webhooks for me** again.

To add a webhook by hand instead, the **Webhooks** page shows what to enter for each site:
**Address to send to (Payload URL)**, **Content type (choose JSON)**, the secret and **Events to turn
on**. More detail is in the reference under [Webhooks](reference.md#webhooks).

**Gitea as the source on your own network:** Gitea blocks webhooks to private addresses by default.
If gitmirror runs on the same machine or network, set `[webhook] ALLOWED_HOST_LIST = private` in
Gitea's `app.ini` (or `GITEA__webhook__ALLOWED_HOST_LIST=private` in its container). Otherwise
deliveries fail with a "not allowed" error in Gitea.

## 10. Keep it running

`gitmirror serve` has to keep running to copy on schedule and receive webhooks. Pick **one** way.
Only one gitmirror can use a data folder at a time; a second one fails with
`another gitmirror process is using state dir ...`.

### A. Docker Compose (recommended)

If you started gitmirror with Docker Compose in step 3, you are done: `restart: unless-stopped`
starts it again after a reboot. Useful commands, from the `deploy` folder:

```sh
docker compose logs -f gitmirror                   # follow the log
docker compose exec gitmirror gitmirror status     # status from the command line
docker compose build && docker compose up -d       # upgrade after updating the source code
```

* The web pages are published on `127.0.0.1:8090` only. To use them from another computer, put an
  HTTPS reverse proxy in front of port 8090 instead of publishing it on all addresses.
* Port 8080 (webhooks, health checks) is published on all addresses. Put a reverse proxy with HTTPS
  in front of it before you expose it to the internet.
* Do **not** publish port 9090. Metrics are not protected and show your repository names.

### B. systemd (Linux, without Docker)

[`deploy/systemd/gitmirror.service`](../deploy/systemd/gitmirror.service) runs `serve` as a
hardened service under its own `gitmirror` user. The setup commands are at the top of that file.
If you set everything up in the web pages:

1. Install the program to `/usr/local/bin` and create the `gitmirror` user, as shown in the file.
2. The service is started with `-c /etc/gitmirror/config.yaml`, so that file must exist. It only
   needs one line: `state_dir: /var/lib/gitmirror`.
3. Remove the two `LoadCredential=` lines from the service (they are only for token files in the
   config file; systemd refuses to start the service when those files are missing).
4. Start it and read the setup code from the log:

   ```sh
   sudo systemctl daemon-reload
   sudo systemctl enable --now gitmirror
   journalctl -u gitmirror | grep "web UI setup"
   ```

The web pages listen on `127.0.0.1:8090` of the server. To open them from your own computer, use an
SSH tunnel, then open http://localhost:8090:

```sh
ssh -L 8090:127.0.0.1:8090 you@your-server
```

### C. Windows: start at sign-in

1. Put `gitmirror.exe` in `C:\gitmirror` and finish steps 3 to 7 by running `gitmirror serve` from
   that folder. Stop it with Ctrl+C.
2. Open PowerShell with **Run as administrator** and create a task that starts it when you sign in:

   ```powershell
   schtasks /Create /SC ONLOGON /TN gitmirror /TR "cmd /c cd /d C:\gitmirror && gitmirror.exe serve"
   ```

The task starts gitmirror in `C:\gitmirror`, so it finds the same `data` folder. A console window
opens when you sign in; closing it stops gitmirror. Remove the task with
`schtasks /Delete /TN gitmirror`.

### Use other ports

If a port is taken, or you want the web pages on another address, create a file `config.yaml` in
the folder gitmirror starts in (in Docker, mount it at `/config/config.yaml`):

```yaml
server:
  listen: "127.0.0.1:18080"            # webhooks and health checks (default :8080)
  ui_listen: "127.0.0.1:18090"         # the web pages (default 127.0.0.1:8090), or "off"
  metrics_listen: "127.0.0.1:19090"    # metrics (default 127.0.0.1:9090), or "off"
```

The environment variable `GITMIRROR_UI_LISTEN` does the same as `ui_listen`; the Docker image sets
it to `:8090`.

## 11. Backups

Back up gitmirror's data folder (`data` next to where you start it, `/data` in Docker, the
`gitmirror-data` volume with Compose, `/var/lib/gitmirror` with systemd). Two files in it matter:

* `state.db`: your sites, mirrors, password and history, and which copies gitmirror made. If it is
  lost, gitmirror treats its own copies as foreign and refuses to update them.
* `secret.key`: the key that unlocks the saved tokens and webhook secret. **Back it up together with
  `state.db`.** Without the matching key, gitmirror cannot read the tokens and stops with an error
  naming the site.

Keep the backup private: together, these two files give access to your tokens. The `cache` folder
can be rebuilt and needs no backup.

To upgrade, stop gitmirror, replace the program or rebuild the image, and start it again. The
database is updated automatically on start.

## 12. Forgot your password?

1. Stop gitmirror (`docker compose stop gitmirror`, `sudo systemctl stop gitmirror`, or Ctrl+C).
2. Run `gitmirror reset-admin` with the same data folder:
   * Program: from the folder gitmirror runs in, `gitmirror reset-admin`.
   * Docker Compose: `docker compose run --rm gitmirror reset-admin`.
   * systemd: `sudo -u gitmirror gitmirror -c /etc/gitmirror/config.yaml reset-admin`.
3. Start gitmirror again. It writes a new setup code to the log. Continue with
   [step 4](#4-choose-your-password).

Your sites, mirrors and copies are kept. Everyone who was signed in is signed out.

## 13. When something goes wrong

* The **Dashboard** and its **Details** pages explain most problems in plain words.
* The [Troubleshooting](reference.md#troubleshooting) section of the reference explains each
  message. A failed copy is tried again at the next scheduled time; you do not need to clean up.
* If the safety guard stops a copy ("sync would delete N of M destination refs"), check the
  original. If the deletions are real, stop `serve` and run
  `gitmirror sync --allow-prune --repo OWNER/NAME` once.
* Originals that are deleted are kept as copies by default. To archive or delete their copies, set
  `on_source_deleted` under `defaults:` in the config file (see
  [configuration.md](configuration.md#defaults)); it applies to mirrors from the web pages too.
* For monitoring, alert on `gitmirror_pair_consecutive_failures > 2`, or on
  `time() - gitmirror_pair_last_success_timestamp_seconds` exceeding three intervals. All metrics are
  listed in the [reference](reference.md#endpoints-and-metrics).

---

## Try it locally first

This runs a throwaway Gitea in Docker and copies a repository between two accounts on it, so you
can see gitmirror work without touching GitHub or your real server. You need Docker and bash
(Git Bash works on Windows), and gitmirror built as in [step 2, option A](#option-a-build-the-program).

1. Start a throwaway Gitea on http://localhost:3311, with a user `alice`, a token and a repository
   `demo`:

   ```sh
   docker run -d --name gitmirror-trial -p 127.0.0.1:3311:3000 \
     -e GITEA__security__INSTALL_LOCK=true \
     -e GITEA__server__ROOT_URL=http://localhost:3311/ \
     -e GITEA__database__DB_TYPE=sqlite3 \
     gitea/gitea:28.3.0
   until curl -sf http://localhost:3311/api/healthz >/dev/null; do sleep 1; done

   docker exec -u git gitmirror-trial gitea admin user create --admin \
     --username alice --password 'Trial-pass-123' --email alice@example.com --must-change-password=false
   docker exec -u git gitmirror-trial gitea admin user generate-access-token \
     --username alice --token-name gitmirror --scopes all --raw > trial-token.txt
   curl -s -H "Authorization: token $(cat trial-token.txt)" -H 'Content-Type: application/json' \
     -d '{"name":"demo","auto_init":true}' http://localhost:3311/api/v1/user/repos >/dev/null
   cat trial-token.txt     # you paste this token in step 4
   ```

2. Start gitmirror with its own trial data folder:

   ```sh
   GITMIRROR_STATE_DIR=./trial-data gitmirror serve
   ```

3. Open http://localhost:8090, enter the setup code from the log and choose a password.
4. **Sites** → **Add a site**: **Gitea**, **Address** `http://localhost:3311`, **Token** from
   `trial-token.txt`. Click **Test connection and save**. The site is called `localhost-3311`.
5. **Mirrors** → **New mirror**:
   1. **Copy from**: site `localhost-3311`, **Choose repositories**, account `alice`. **Next**.
   2. **Repositories**: tick `demo`. **Next**.
   3. **Copy to**: site `localhost-3311`, account `mirrors`, **Keep the same names**. **Next**.
   4. **Options**: **Name of this mirror** `demo`. **Preview**.
   5. **Check and save**: `demo` shows **Will be created**. **Save mirror**.
6. The **Dashboard** shows the copy as **OK** after a few seconds. Open
   http://localhost:3311/mirrors/demo (sign in as `alice`, password `Trial-pass-123`) to see the
   private copy. Push a commit to `alice/demo` and click **Sync now** to see it arrive.

Clean up: stop gitmirror with Ctrl+C, then:

```sh
docker rm -f gitmirror-trial
rm -rf trial-data trial-token.txt
```

---

## Advanced: configure with a file

Everything in this guide can also be written in a YAML file, `config.yaml`. You do not need one,
but it is useful if you want to keep your setup in version control, keep tokens in files, or change
settings the web pages do not show (such as `on_source_deleted`, `concurrency` or the safety guard
limits).

* gitmirror reads `config.yaml` from the folder it starts in, or the file named by `-c` or
  `GITMIRROR_CONFIG`. In Docker, mount it at `/config/config.yaml`.
* Sites and mirrors from the file show up in the web pages marked **From config file — edit it
  there**. They cannot be changed in the browser. Mirrors from the file and from the web pages are
  checked together, so they cannot copy into the same repository or copy each other back and forth.
* Changes to the file apply when you restart `serve`. Check them first with `gitmirror validate`
  and `gitmirror plan`.

A walkthrough with a complete example, tokens in environment variables or files, and what
`validate` prints is in [configuration.md](configuration.md#walkthrough-a-first-config-file). Every
setting is listed in the same file, and [`config.example.yaml`](../config.example.yaml) is a
commented example. What `plan`, `sync` and `status` print is explained in the reference under
[Reading the output](reference.md#reading-the-output).
