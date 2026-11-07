# ekPanel

**An open-source hosting control panel for everyone, everywhere.** One panel for classic web
hosting (like cPanel and aaPanel), Git and Docker app deployment (like Coolify), and later
email, DNS and game servers, with a modern UI in your own language.

> **Status: early development (phase 0 of 6).** Do not use it for production hosting yet.
> See [PLAN.md](PLAN.md) for the full roadmap.

> **ek** (এক) is Bangla for "one": one panel for everything.

## What works today

- One-time setup with a setup token, so nobody else can claim a fresh install
- Sign-in with argon2id password hashing, optional two-factor authentication (TOTP),
  login rate limiting, and "sign out other devices"
- Add Linux servers: run one command on the server, and its agent connects to the panel
  (outbound only, so it works behind NAT) over **mutual TLS** with a pinned private CA
- Live CPU, memory, disk, network, load and uptime per server, plus **30 days of history**
- See and restart a server's services (Caddy, PostgreSQL, cron…) through background
  **tasks** that show their progress in the UI
- **Websites**: static or PHP (7.4 to 8.5 side by side), free automatic HTTPS, extra
  domains, domains in any script. Each site runs as its own Linux user with its own PHP
  pool. Changes apply with zero downtime and broken configuration repairs itself
- **Databases**: MariaDB and PostgreSQL with an account per database. Passwords are shown
  once and never stored, only the database engine's own hash
- **One-click WordPress** in 14 languages, with its own database. The admin password is shown
  once and never stored
- **Files**: a browser file manager with uploads, code editor, zip/unzip and trash, and SFTP
  per site. Both are locked into the site's own folder
- **Backups**: encrypted, deduplicated backups of every website and database to the server
  or S3-compatible storage, on a schedule with retention, and one-click restores. The
  encryption key is shown once and stays on the server
- **Docker apps**: run any image or Compose file, with a domain and free HTTPS, variables,
  kept folders, limits, logs and one-click image updates. App ports stay private to the
  server, so Docker cannot open holes in the firewall
- **Logs, traffic and notifications**: live request logs, PHP errors and traffic charts per
  website and app; alerts for offline servers, full disks, failed backups and more, by email,
  Telegram, Discord, Slack or signed webhooks, in the recipient's language
- **Cron jobs** for websites and apps, with presets or cron expressions in your time zone,
  a preview of the next runs, time limits, output and "run now"
- **Customers and packages**: host other people. Each customer signs in to see only their own
  websites, databases and apps, within a package's limits on counts, disk, memory and CPU;
  memory and CPU are enforced by the kernel (cgroups) for their PHP and containers
- **Firewall**: turn on ufw with SSH, web and agent ports always open, then open ports or
  block addresses from the server page
- An append-only audit log of every change (the database itself refuses edits and deletes)
- A web UI in **English** and **বাংলা (Bangla)**, ready for right-to-left languages, with
  light/dark themes, a `Ctrl+K` command palette and phone-friendly layouts

Coming next, in order: then Git deploys, an app store, customers and packages. [PLAN.md](PLAN.md) lists
every phase.

## How it fits together

```
Browser ──HTTPS──► ekpanel (Go: API + embedded web UI) ──► PostgreSQL
                          ▲
                          │ WebSocket, opened by each agent
              ekpanel-agent on every managed server (Go, single binary)
```

The panel stores what should exist; agents do the work on each server and report back.
Agents only accept typed operations; there is no "run this shell command" endpoint.

## Try it locally

You need Go 1.26+ (older Go downloads it automatically), Node.js 22+ and Linux.

```bash
make ui      # build the web UI
make dev     # panel on http://127.0.0.1:8080 with a throwaway embedded PostgreSQL in .data/
```

The log prints a **setup token**. Open http://127.0.0.1:8080, paste it, and create your admin.
Then use **Add server**, copy the token from the dialog, and connect this machine:

```bash
make dev-agent TOKEN=<token> PIN=<ca pin>    # both are in the dialog's command
```

For UI work with hot reload, run `make dev` and `make dev-ui` together, then open
http://localhost:5173. See [CONTRIBUTING.md](CONTRIBUTING.md) for details.

## Install on a server

On a fresh **Ubuntu 24.04+ or Debian 12+** server (amd64 or arm64), as root:

```bash
make release                            # on your machine: dist/ekpanel-<version>-linux-<arch>.tar.gz
tar -xzf ekpanel-*-linux-amd64.tar.gz && cd ekpanel-*-linux-amd64   # on the server
sudo ./install.sh --local-dir . --domain panel.example.com
```

The installer sets up PostgreSQL and Caddy, runs the panel as an unprivileged, sandboxed
systemd service, connects the server's own agent, and prints the one-time setup token.
Agents on other servers connect to the panel on TCP port 7443.
Leave out `--domain` to use the server's IP address with a self-signed certificate. Running it
again upgrades the binaries and keeps your data. One-line installs from release downloads come
with the first release.

## Languages

The panel is built to be translated. All UI text lives in `web/src/i18n/locales/`, and dates,
numbers and units are formatted for each language automatically. **Adding your language is
one of the most valuable contributions.** See [docs/TRANSLATING.md](docs/TRANSLATING.md).

| Language | Status |
| --- | --- |
| English | complete (source) |
| বাংলা (Bangla) | complete |

## Security

A hosting panel runs as root on servers, so security is the first priority. Please report
vulnerabilities privately as described in [SECURITY.md](SECURITY.md), never in public issues.

## License

ekPanel is free software under the [GNU Affero General Public License v3.0](LICENSE)
(`AGPL-3.0-only`). You can use, study, change and share it. If you run a modified version as
a service for others, you must offer them your changes under the same license.
