# Hosting Panel — Master Plan

> Goal: one self-hosted panel that combines the best of **cPanel / aaPanel** (classic web hosting),
> **Coolify / Dokploy** (Git + Docker app deployment) and **Pterodactyl** (game/process servers),
> with a modern UI, a real API, and multi-server support.

Name: **ekPanel**. *ek* (এক) is Bangla for "one": one panel for everything.

---

## 1. Honest scope check

cPanel has 25+ years of work behind it, and Coolify has hundreds of contributors. Doing everything at once
will not work. The plan below builds a **solid core that every feature plugs into**, ships a usable
MVP early, and adds feature modules phase by phase. Every phase ends with something you can install
on a real server and use.

---

## 2. Architecture

```
                 ┌──────────────────────────── Control plane ────────────────────────────┐
 Browser ──────► │  Web UI (React)  ──►  Panel API (Go)  ──►  PostgreSQL                 │
 CLI / API ────► │                         │   task queue (in Postgres)                  │
                 └─────────────────────────┼─────────────────────────────────────────────┘
                                           │ mutual TLS WebSocket on :7443 (agent dials out → works behind NAT)
              ┌────────────────────────────┼────────────────────────────┐
              ▼                            ▼                            ▼
        ┌──────────┐                 ┌──────────┐                 ┌──────────┐
        │  Agent   │  server 1       │  Agent   │  server 2       │  Agent   │  server N
        │  (Go,    │                 │          │                 │          │
        │  root)   │                 │          │                 │          │
        └────┬─────┘                 └──────────┘                 └──────────┘
             │ manages
   Caddy (edge proxy + auto SSL) · PHP-FPM pools · Docker · MariaDB/Postgres/Redis
   PowerDNS · Stalwart mail · nftables + CrowdSec · restic backups · systemd/cgroups
```

**Key ideas**

1. **Panel and agent are separate.** The panel holds state and the UI; the agent is a small
   root binary on each server that only performs *typed operations* (`CreateSite`,
   `CreateDatabase`, `DeployContainer` …). The API never forwards raw shell commands.
   Single-server installs run both on one box, and the same code scales to many servers.
2. **Desired state + reconcile loop** (like Kubernetes, simplified). The panel stores what *should*
   exist, and the agent converges the server to it and reports drift. This makes restores,
   migrations and "repair this site" buttons almost free.
3. **One edge proxy for everything.** Caddy owns ports 80/443 on each server, issues certificates
   automatically and routes each domain to a PHP-FPM site, a static folder, a Docker container
   or an app port. It is configured live through its admin API, so no config files are rewritten and no reloads are needed.
4. **Two runtimes, one "Site" concept.**
   - *Native*: a Linux user per account, a PHP-FPM pool per site and cgroup v2 limits per account.
     Cheap and dense, which suits shared hosting and WordPress.
   - *Container*: Docker per app. Isolated, for Node/Python/Go apps, Git deploys and one-click services.
5. **Modules.** Each feature area (mail, DNS, game servers …) is a module with its own API
   routes, UI pages, agent operations and an on/off switch, so admins install only what they need.

---

## 3. Tech stack (recommended)

| Layer | Choice | Why |
|---|---|---|
| Agent | **Go** | Single static binary, low RAM, excellent at system/Docker work (same as Pterodactyl Wings). |
| Panel API | **Go** (chi, pgx) | Same language as the agent: shared types, one toolchain. |
| Database | **PostgreSQL** | Robust; also holds the task queue and metric history (no Redis needed). |
| UI | **React + TypeScript**, Vite, TanStack Router/Query, shadcn/ui, Tailwind | Fast, modern, huge ecosystem. Built into the Go binary as static files. |
| Realtime | WebSocket (logs, terminal, metrics, deploy output) | |
| Edge proxy | **Caddy** | Auto HTTPS (incl. wildcard via DNS-01), live API config. |
| DNS | **PowerDNS** (+ Cloudflare API integration) | API-driven authoritative DNS. |
| Mail | **Stalwart** (SMTP/IMAP/JMAP, spam filter, DKIM, one binary) + SnappyMail webmail | Far simpler to automate than Postfix+Dovecot+Rspamd. |
| Security | nftables, CrowdSec, Coraza WAF (in Caddy), ClamAV | |
| Backups | **restic** → local / S3 / B2 / SFTP / Google Drive | Encrypted, incremental, deduplicated. |
| Target OS | Ubuntu 24.04+/Debian 13 first, then Alma/Rocky | |

**API-first:** every UI action goes through the public REST API (OpenAPI spec), which also
generates the TypeScript client and a `panel` CLI.

Local machine note: Go here is 1.22 (we'll want a current Go) and Docker isn't installed. For
development we need **a test VPS or a local VM** (Ubuntu), because the agent manages a real Linux server.

---

## 4. Feature map (what we combine)

**Core / platform**
- Login with 2FA (TOTP), passkeys, session management, API tokens
- Roles: **Admin → Reseller → Customer** (+ sub-users with fine-grained permissions)
- Hosting packages/plans with quotas (disk, bandwidth, sites, DBs, mailboxes, CPU/RAM)
- Audit log of every action, activity feed, notifications (email, Telegram, Discord, Slack, webhook)
- Languages: English + বাংলা from day one (i18n), light/dark theme, mobile-friendly UI
- One-line installer, self-update, white-labelling (logo, colors, panel domain)

**Servers** (aaPanel / Webmin)
- Add a server with one command (agent auto-enrolls)
- Live and historical CPU/RAM/disk/network, services, processes
- Web terminal, OS updates, swap, timezone, SSH keys

**Websites** (cPanel / aaPanel)
- Domains, subdomains, aliases/parked domains, redirects
- Static, PHP (multiple versions side by side, per-site php.ini), reverse proxy to a port
- Free SSL (Let's Encrypt/ZeroSSL), wildcard, custom certificates, forced HTTPS
- **File manager** (upload, edit, zip/unzip, permissions, trash), FTP/SFTP accounts
- Cron jobs, access/error logs, simple traffic stats
- **WordPress toolkit**: 1-click install, clone/staging, updates, security hardening, maintenance mode
- App installer (Laravel, Joomla, Drupal, Ghost …)

**Apps & Docker** (Coolify / Dokploy)
- Deploy from Git (GitHub/GitLab/Gitea apps + webhooks), Dockerfile, Nixpacks, Docker image, Compose
- Env vars and secrets, build logs, rollbacks, zero-downtime deploys, preview deploys per PR
- 1-click services (Postgres, Redis, n8n, Uptime Kuma, Plausible, Minio …) from a template catalog
- Container logs, shell, resource limits, health checks

**Databases**
- MariaDB/MySQL, PostgreSQL, Redis, MongoDB — create DBs and users, grants, remote access
- Built-in DB browser (or embedded Adminer/phpMyAdmin), import/export, scheduled dumps

**DNS & domains**
- Built-in authoritative DNS with zone editor and templates; Cloudflare sync
- Automatic SPF/DKIM/DMARC records for mail

**Email**
- Mailboxes, forwarders, aliases, catch-all, autoresponders, quotas
- Spam filtering, webmail, deliverability checker

**Security**
- Firewall UI, CrowdSec/fail2ban bans, WAF rules, IP allow/deny, country blocking
- Malware scan, per-account isolation (separate Linux users, open_basedir, cgroups)

**Backups & migration**
- Scheduled per-site/DB/account and full-server backups to many destinations; one-click restore
- **Importers**: cPanel backup (cpmove), aaPanel, Coolify, plain WordPress; server-to-server move

**Game & process servers** (Pterodactyl), later phase
- Minecraft and other game servers in containers, console, file access, startup variables

**Business** (later)
- WHMCS / Blesta / Paymenter integration modules; optional built-in billing
  (Stripe, bKash, SSLCommerz)

---

## 5. Roadmap

Each phase ends with something installable and usable.

### Phase 0 — Foundations
- Repo layout, Go + React toolchain, CI, lint, tests
- Panel ↔ agent protocol (enrolment token + CA pin → mutual TLS, outbound WebSocket, typed operations)
- Auth (users, sessions, 2FA), RBAC skeleton, audit log
- Task queue for background work; desired-state reconcile engine arrives with websites in phase 1
- `install.sh` that installs panel + agent + Caddy on a fresh Ubuntu server
- **Done when:** fresh VPS → one command → login page over HTTPS → server shows live metrics.

### Phase 1 — MVP "I can host real sites"
- Websites: static + PHP (FPM pools, multi-version), domains, auto SSL
- Databases: MariaDB + PostgreSQL (create DB/user, Adminer link)
- File manager + SFTP
- Docker apps: deploy from image or Compose, logs, env vars, domains
- Basic backups (restic → local/S3) and restore
- **Done when:** a WordPress site and a Node app run side by side with SSL, are backed up, and are restorable.

### Phase 2 — "Coolify level"
- Git deployments (GitHub app, webhooks, Nixpacks/Dockerfile builds), rollbacks, previews
- One-click service template catalog
- Cron, web terminal, logs viewer, notifications
- Multi-user: packages, quotas, customer accounts, cgroup resource limits

### Phase 3 — "cPanel level"
- Built-in DNS (PowerDNS) + Cloudflare
- Email (Stalwart + webmail), auto DKIM/SPF/DMARC
- WordPress toolkit, app installer
- Security suite (firewall UI, CrowdSec, WAF, malware scan)
- FTP, PHP extensions/ini editor, redirects, hotlink protection

### Phase 4 — "Beyond"
- Multi-server: place sites/apps on any node, move between nodes
- Reseller accounts, white-label, billing integrations
- Importers (cPanel, aaPanel, Coolify)
- Game servers module
- Public API docs site, `panel` CLI, Terraform provider

---

## 6. Repository layout (planned)

```
/cmd/panel           panel server (API + serves UI)
/cmd/agent           server agent
/cmd/cli             `panel` CLI
/internal/api        HTTP handlers (OpenAPI)
/internal/core       users, rbac, audit, jobs, reconcile
/internal/modules/   sites, apps, databases, dns, mail, backups, security, files …
/internal/agentops   typed operations the agent can perform
/internal/agentproto panel ↔ agent messages and typed operations
/web                 React UI
/deploy              install.sh, systemd units, packaging
/docs                user + developer docs
```

---

## 7. Security principles (non-negotiable)

A hosting panel runs as root on customers' servers, so it is a prime target.

- Agent accepts only typed, validated operations; no "run this shell string" endpoint
- Every customer = separate Linux user; PHP-FPM pools run as that user; cgroups enforce limits
- mTLS between panel and agents; enrolment tokens are single-use and short-lived
- Secrets encrypted at rest; passwords hashed with argon2id; 2FA available to everyone
- CSRF, rate limiting, strict CSP on the panel; audit log of every mutation
- Signed releases and self-updates; security review before every phase release

---

## 8. Decisions

Decided (2026-11-07):

- **Name: ekPanel** (binaries `ekpanel` and `ekpanel-agent`, paths `/etc/ekpanel` and
  `/var/lib/ekpanel`, environment variables `EKPANEL_*`).
- **Open source**, built for people all over the world to use and contribute to.
- **Multilingual from day one.** English is the source language; Bangla is the first full
  translation. Rules every feature follows:
  - no user-facing text in code: every string is a translation key, API errors carry a
    stable `code` that the UI translates;
  - numbers, sizes, rates, dates and relative times go through `Intl` for the user's locale;
  - layouts use logical (start/end) CSS so right-to-left languages mirror automatically;
  - plural forms follow CLDR rules per language;
  - fonts are self-hosted (Noto family) so every script renders the same on every OS, and
    a self-hosted panel never calls out to font CDNs;
  - a translation checker runs in CI; a Weblate instance comes before the first release.
- **Stack** as in section 3: Go panel and agent, React + TypeScript UI, PostgreSQL.
- **Phase 1 order**: PHP/WordPress hosting first, then Docker apps.
- <a id="license"></a>**License: AGPL-3.0-only** for the panel and agent. Companies that run
  ekPanel as a hosted service must share their changes (the model used by Plausible and
  Grafana); MIT/Apache would allow closed forks. All current dependencies (MIT, BSD, Apache-2.0,
  OFL fonts) are compatible.
- **Branches**: all work happens on `dev`; `prod` holds production code and changes only
  through a pull request from `dev`.
- **Test server**: a local Ubuntu 24.04 VM (QEMU/KVM) on the development machine, stopped
  when not in use.

## 9. Phase 0 progress

- [x] Repository layout, Makefile, CI (Go tests with a real PostgreSQL, typecheck, i18n check)
- [x] Panel ↔ agent link: one-time enrolment token → agent credential; agent dials out over
      WebSocket and reconnects with backoff
- [x] Auth: first-admin setup guarded by a setup token, argon2id passwords, sessions,
      TOTP 2FA, login rate limiting, sign out other devices
- [x] Roles (admin / reseller / customer) and admin-only routes
- [x] Append-only audit log, enforced by a database trigger
- [x] Live server dashboard: CPU, memory, disk, network, load, uptime (about 20 minutes in memory)
- [x] Web UI shell: English + Bangla, RTL-ready, light/dark, `Ctrl+K` palette, mobile layout
- [x] Mutual TLS between panel and agents: the panel runs a private CA; agents pin it at
      enrolment (`--ca-pin`), get a 90-day client certificate, renew it automatically, and
      re-enrolling revokes the old certificate at once. Agents use a separate port (7443).
- [x] Typed panel → agent operations over the same connection (`services.status`,
      `services.restart` with a fixed allowlist); agents reject anything else
- [x] Task queue in PostgreSQL (`FOR UPDATE SKIP LOCKED`, leases, retries with backoff,
      crash recovery), visible in the UI. Decision: our own ~200-line queue instead of River,
      because tasks are user-visible records we need anyway and it adds no dependency
- [x] Metric history in PostgreSQL: 1-minute buckets for 48 h, rolled up hourly for 31 days;
      UI ranges Live / 1 h / 24 h / 7 d / 30 d
- [x] Services card (live systemd state, restart) and task tray in the UI
- [x] Desired-state / reconcile engine: delivered in phase 1a together with websites
- [x] `deploy/install.sh` for Ubuntu 24.04+/Debian 12+: PostgreSQL, Caddy (domain → Let's Encrypt,
      no domain → self-signed for the IP), sandboxed systemd units, auto-connects the server's own
      agent, prints the setup token; re-runnable as an upgrade. Tested in a local VM incl. reboot
- [x] `scripts/test-vm.sh`: disposable Ubuntu 24.04 VM (QEMU/KVM) for testing server changes
- [ ] One-line install from release downloads (after the first release)
- [x] `make release`: linux amd64/arm64 archives with SHA256SUMS
- [ ] Signed releases from CI (needs the GitHub repository)

## 10. Phase 1 progress

Phase 1 is delivered in parts; each is tested in the VM and committed separately.

**1a. Websites — done**
- [x] Desired-state engine: the panel sends a server's complete list of sites (`state.apply`);
      the agent converges, reports per site, and is re-run on every change, on agent
      reconnect and every 10 minutes (self-healing: a deleted config is restored)
- [x] Static and PHP sites; PHP 7.4–8.5 side by side from Ondřej Surý's packages, installed
      on first use; switching versions has zero downtime (new FPM pool and socket first,
      then Caddy, then the old pool — verified under load: 457/457 requests answered)
- [x] Isolation: one Linux account per site (`/home/<user>/public_html`), PHP-FPM pool per
      site running as that account with `open_basedir`; ACLs let only Caddy read site files
- [x] Caddy: one file per site, validated (as the caddy user) before a graceful reload;
      a rejected config is rolled back and reported on that site only
- [x] Free automatic HTTPS (Let's Encrypt) or self-signed per site; dotfiles such as
      `.env` and `.git` are never served
- [x] Domains in any script (IDNA → punycode), extra domains, `www.` offered by default,
      every host name unique across the panel
- [x] Safety: the agent validates everything again, only touches Linux accounts in the
      `ekpanel-sites` group, and only deletes data on an explicit delete
- [x] UI: websites list, add dialog, site page (settings, status, retry, delete), sites on
      the server page, command palette entries; English and Bangla

**Next**
**1b. Databases — done**
- [x] MariaDB and PostgreSQL, installed on first use; one owner account per database with
      access to that database only (MariaDB grants; PostgreSQL ownership and
      `REVOKE ALL … FROM PUBLIC`), local access only
- [x] Passwords are generated, shown once and never stored: the panel keeps only the
      engine's own hash (MariaDB native hash, PostgreSQL SCRAM-SHA-256 verifier), so
      neither the panel database nor the agent ever holds a password
- [x] SQL reaches the database through stdin, never the process list; names and hashes are
      validated on both sides; system names are refused
- [x] The agent keeps a registry of what it created and refuses to change or drop any
      database or account it did not create; deletes happen only on request
- [x] Customer PostgreSQL accounts cannot connect to the panel's own database
- [x] UI: databases list, add dialog with the one-time password and connection details,
      database page (connect info, size, password reset, delete), cards on the server page
- [ ] Database browser in the panel (Adminer-style), import and export: later
**1c. File manager and SFTP — done**
- [x] File manager: browse, upload in 512 KB pieces (drag and drop, progress, replace),
      download, code editor (CodeMirror, loaded on demand, Ctrl+S), new file/folder, rename,
      permissions, zip/unzip (.zip, .tar, .tar.gz), trash with restore and empty
- [x] Every file action runs in a separate worker that chroots into the site's home folder
      and drops to the site's account, so symlinks or `..` cannot reach other files (tested
      with links to /etc/shadow, /root and ../../etc/passwd); zip slip is refused, links in
      archives are never created, extraction is capped at 10 GB and 200,000 entries
- [x] SFTP per site: OpenSSH `internal-sftp` chrooted to the home folder, no shell, no
      forwarding; password shown once and stored only as SHA-512 crypt (100,000 rounds);
      SSH keys normalized (options such as `command=` stripped) in a root-owned file;
      sshd settings are checked with `sshd -t` before reload and removed if rejected
- [x] Site home folders are now root-owned with the site's group (required by the chroot);
      existing sites are converted automatically on the next reconcile
- [ ] Per-user disk quotas: phase 2 (with packages)
**1d. One-click WordPress — done**
- [x] One click on a PHP site, or a checkbox when adding a site: creates a MariaDB database
      (`wp_<site account>`), downloads WordPress with WP-CLI, writes `wp-config.php`,
      installs, and sets up the admin account; about 25 seconds in the test VM
- [x] 14 WordPress languages offered in their own scripts; the default follows the panel
      language (Bangla panel → bn_BD WordPress)
- [x] The admin and database passwords exist only in memory: the API returns the admin
      password once, the task row never holds it, and WP-CLI receives both through stdin.
      If the panel restarts before the task runs, the task fails cleanly
- [x] WP-CLI is downloaded once and checked against its published SHA-512
- [x] `wp-config.php` is readable only by the site's account (600); `FS_METHOD direct`, so
      plugins and updates need no FTP details; installs only into an empty folder
- [ ] WordPress toolkit (staging, bulk updates, hardening): phase 3
- [ ] 1e. Backups and restore (restic), basic firewall
- [ ] 1f. Docker apps (image and Compose)
