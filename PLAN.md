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
**1e. Backups and a basic firewall — done**
- [x] Backups with restic: encrypted and deduplicated, to the server's own disk or any
      S3-compatible storage (AWS S3, Backblaze B2, Cloudflare R2, Wasabi, MinIO…). Every
      website (home folder, without tmp and trash) and every database (`mariadb-dump
      --single-transaction`, `pg_dump -Fc`) on the server, as one snapshot each
- [x] The encryption password is a recovery key shown once; it and the S3 keys live only on
      the agent (root, 600). The panel never stores either. An S3 location can be connected
      again with the recovery key, for example after rebuilding a server
- [x] Schedule (hourly, daily, weekly or manual) in the user's local time, retention (daily,
      weekly and monthly copies, pruned after each run), "Back up now", last result with
      partial failures named. Database dumps stream into restic; a dump that fails half way
      is not kept
- [x] Restore a website into a new folder next to `public_html`, or replace `public_html`
      (the current files go to the site's trash); restore a database in place, after typing
      its name
- [x] Long tasks keep their lease with a heartbeat, so a backup of many hours is not run twice
- [x] Firewall (ufw): off by default ("not managed", so an existing firewall is left alone).
      When on, SSH (on the ports sshd really uses), HTTP, HTTPS (TCP and UDP) and the agent
      port always stay open; users open ports or ranges (optionally only from an address) and
      block addresses. Blocks go on top. Only the differences are applied, and rules added by
      hand are kept. The panel refuses to block the address of the person using it
- [x] Tested in the VM: local and S3 backups (S3 against a local test server), dedup across
      runs, all three restore kinds for sites, MariaDB (WordPress) and PostgreSQL restores,
      reconnecting with the recovery key, wrong key refused; firewall on/off with SSH, panel
      and agent still reachable
- [ ] Single-file restore and download of a backup: later. Restore to another server: later
- [ ] Firewall: rate limiting and fail2ban-style blocking: phase 4 (security center)
**1f. Docker apps — done**
- [x] Run any Docker image (with variables, kept folders, memory and CPU limits) or any
      Compose file. Every app is a Compose project in `/srv/ekpanel/apps/<name>` (root only),
      kept in line by the same desired-state engine as sites: drift runs bring removed
      containers back, and a stopped app stays stopped
- [x] Docker and Compose v2 come from the distribution's packages on first use; container
      logs are capped (10 MB × 3) before Docker first starts
- [x] Ports stay private: the app's port is published on 127.0.0.1 only, and a domain with
      free HTTPS (Caddy) points at it. Compose files that publish ports on public addresses or
      use the host network are refused unless the user allows it, because Docker opens such
      ports past the firewall
- [x] Variables never appear on a command line: image apps get them in the generated Compose
      file (written as JSON, so no value can change its structure, `$` kept literally); Compose
      apps read them as `${NAME}`
- [x] Containers and their state, logs (last 100–5000 lines), restart, stop/start, and
      "update image" (pull and recreate) as a task
- [x] Delete keeps volumes and the app folder by default; deleting the data too is a separate
      choice. Host names stay unique across sites and apps
- [x] Quick start for Uptime Kuma, n8n and Nginx; English and Bangla
- [x] Tested in the VM (Docker 29, Compose 2.40 from Ubuntu): Docker installed with the first
      app in 37 s; HTTPS domain to nginx; kept folder survived recreation; public port refused
      and nothing listened on it; Compose app with two services and `${VAR}`; stop, start,
      restart, logs, image update; both kinds of delete
- [ ] Git deploys, builds (Dockerfile, Nixpacks), shell into a container and an app catalogue:
      phase 2 and 3

## 11. Phase 2 progress

Agreed order (2026-11-07): customers first, so every later part is built with ownership and
limits from the start. Decisions: customers run Docker images only (no Compose files); disk
limits are measured and enforced by the panel, real filesystem quotas come later as an option.

**2a. Customers, packages and limits — done**
- [x] Packages: number of websites, databases and apps; disk space; memory and CPU for all of a
      customer's processes; PHP versions; servers; allowed Docker images; SFTP on or off
- [x] Customer accounts with a generated password shown once; customers sign in to the same
      panel and see only what they own. Administrators can create things for a customer
- [x] Ownership is checked in one place for every route; anything a customer may not see is
      "not found". A test walks 22 routes as another customer and expects 404 on each
- [x] Real limits: each customer has a systemd slice (cgroup v2) with the package's memory and
      CPU. Their PHP runs under a PHP-FPM master of their own inside it, and their containers
      get the slice as cgroup parent. Changed limits apply live, without restarting anything.
      A script over the memory limit costs only its own worker (`OOMPolicy=continue`)
- [x] Disk use measured every 30 minutes (site folders, app folders and volumes, databases);
      when it is used up, new websites, databases, apps and growing file actions stop
- [x] "Sign in as" a customer for one hour: a banner shows it, account security stays with the
      customer, and the audit log names the administrator with `as_customer`
- [x] Everyone can change their own password; English and Bangla
- [x] Tested in the VM: 256 MB package, a script asking for 400 MB was killed alone while the
      customer's other requests kept working; 50% CPU gave 2.0 s of CPU for two 4-second loops;
      container placed in the slice; package change applied live; 60 MB in a 50 MB package
      blocked writes and new databases
- [ ] Moving an existing website, database or app to another owner: later
- [ ] Suspending a customer; email to new customers: with notifications (2c)
- [ ] SFTP uploads are not stopped by the disk limit (the panel's own file actions are)

**2b. Cron jobs — done**
- [x] Jobs on a website (run with `sh` as the site's account, from `public_html`) or on an app
      (run inside one of its containers). Presets (every minute … monthly) or any five-field
      cron expression, in the user's time zone, with the next five runs shown while editing
- [x] Each job is a systemd timer and a oneshot service: a job still running is not started
      twice, a time limit stops it, output goes to the journal. The command lives in a script
      readable only by root and the site's account, so no quoting or `%` can change the unit.
      Site jobs run with `NoNewPrivileges`, a private /tmp and a read-only /usr and /etc, and
      customers' jobs run in their slice
- [x] Cron's "either day" rule (day of month OR day of week) becomes two OnCalendar lines;
      every schedule is checked with `systemd-analyze calendar` before it is used
- [x] Last run, result, exit code, next run and recent output; run now; pause; package limit on
      the number of jobs; jobs go with their website or app
- [x] Tested in the VM: every-minute job in Asia/Dhaka ran as the site's account; exit code 3
      and a 10-second time limit reported as such; a job inside an Alpine container; a
      customer's job in their slice; pause stopped the timer; nothing left after deletes
- [x] Found and fixed while testing: deleting a website left its home folder behind (userdel
      skips a home the account does not own); the folder is now removed with the site

**2c. Logs, traffic and notifications — done**
- [x] Request log of each website and app (time, address, status, path, size, duration) with a
      status filter, search and a live mode that fetches only new lines; the PHP error log of
      websites. The PHP log is read through the file worker (as the site's account, inside its
      folder), so a link planted there cannot expose other files
- [x] Traffic: the agent adds web server log lines to hourly totals (requests, bytes, status
      classes, most requested paths), follows log roll-overs, and never counts a line twice;
      the panel collects every 15 minutes and keeps 90 days. Charts for 24 hours, 7 and 30
      days; customers see this month's traffic on their dashboard
- [x] Notifications in the panel (a bell with unread count) and on channels each user sets up:
      email (through the administrators' SMTP server), Telegram, Discord, Slack, and webhooks
      signed with HMAC-SHA256. Each channel can pick its events; "send a test" shows what the
      service answered. Messages are written in the recipient's language (the notification
      texts are checked for missing translations like the UI)
- [x] Events: server offline (after 3 minutes) and back, disk over 90%, backup failed or
      partly failed, website, app or database failing to apply (once, when it starts), disk
      space of a package at 90% and 100%, failed tasks, and sign-in from a new address.
      Failed sends are retried after 1, 5 and 30 minutes
- [x] Secrets (bot tokens, webhook addresses, signing keys, the SMTP password) are encrypted
      with AES-256-GCM using a key file the panel keeps (`/var/lib/ekpanel/secret.key`), and
      never sent back to the browser. Customers' webhooks cannot reach private or local
      addresses (checked on the address actually dialled, so redirects and DNS tricks fail too)
- [x] Tested in the VM: traffic counts stable across repeated collections; following the
      request log; PHP warnings shown; a link to /etc/shadow returned nothing; customers'
      webhooks to 127.0.0.1 and 169.254.169.254 refused; a webhook on the server received a
      signed test, and "offline" 3 minutes after stopping the agent, then "back online"
- [ ] Bandwidth limits in packages: later (the numbers are now there)
- [ ] Cron job failures do not alert yet (the panel does not watch job results)

**2d. Web terminal — done**
- [x] A root shell on any server for administrators, and a shell inside a running container of
      an app (any service of a Compose app) for whoever owns the app, in the browser (xterm.js)
      with colours, resizing and reconnecting
- [x] The agent runs the shell in a pseudo-terminal and streams it over its existing mTLS
      connection; the panel bridges it to a WebSocket from the browser. Shells belong to that
      connection: when the browser leaves or the agent disconnects, the whole process group is
      hung up, then killed after 3 seconds. At most 10 shells per server
- [x] Opening one needs the password (and two-factor code, if on) again; that confirmation lasts
      15 minutes for that session only. Not available while signed in as a customer. Shells
      close after 30 minutes without typing and after 8 hours in any case
- [x] The audit log records opening and closing, with duration and bytes in and out (not what
      was typed)
- [x] Tested in the VM: root shell with the right size after resizing, `exit` reported to the
      browser, a `sleep` left running was killed when the tab closed, a shell in an Alpine
      container, a stopped service reported as such
- [ ] Recording sessions, and shells as a website's own account: later

**2e. App catalogue — done**
- [x] 14 one-click services: Uptime Kuma, Grafana, n8n, Gitea, IT Tools, Vaultwarden, Umami,
      Ghost, listmonk, Nextcloud, Memos, Excalidraw, ntfy and Redis. Each is a reviewed Compose
      file in the panel (`internal/catalog`), with categories, search and the memory it needs
- [x] Installing asks only for a name, server, owner and domain. Passwords and keys are
      generated for each install and kept in the app's variables; values made from the domain
      follow it when it changes. The app page shows the service's notes and sign-in details
- [x] An installed service is a Compose app that remembers its template and version. Its
      Compose file and port stay the template's; domain, variables and limits can change
- [x] Customers can install services whose images their package allows (their own Compose
      files stay refused). The agent puts every container of a customer's app in their slice
      and refuses privileged mode, capabilities, devices, host folders and namespaces, volumes
      with driver options, and secrets or configs from the host. Tests check every template
      for the same rules, for declared images and variables, and for translations
- [x] Tested in the VM: all 14 installed, answered over HTTPS and were removed without leaving
      volumes. Generated passwords worked (Grafana, Nextcloud, Vaultwarden token, Redis, with
      wrong ones refused); ntfy refused anonymous use; a customer's Umami ran both containers
      in their slice and Nextcloud outside their package was refused. Ghost needs a minute or
      two on its first start, which its notes now say
- [ ] Updating an installed service to a newer template version: later
- [ ] More services, and services that need more than one domain or port: later

**2f. Git deploys — done** (agreed: builds without a Dockerfile and previews move to 2g)
- [x] Git apps: a repository (https, or SSH with a deploy key the panel makes), a branch, a
      Dockerfile and build folder. The agent clones and builds on the app's server; the output
      streams live to the deployment's log in the panel. Variables are also build arguments,
      passed through the environment, never on a command line
- [x] Zero-downtime releases: each Git app has two host ports. The new build starts on the
      free one next to the running container; once it answers (or its health check passes)
      the old one stops. The web server lists both ports, the running one first. A release
      that does not start is removed and the previous one keeps serving; the deploy fails and
      the app keeps naming the build that runs. Unchanged apps are not restarted (Compose's
      config hash decides)
- [x] Deployments: number, status, commit and message, who or what started it, duration and
      the full log. The last five builds stay on the server; rolling back to one releases it
      again without building
- [x] Webhooks for GitHub, Gitea, Forgejo and Gogs (HMAC-SHA256 of the body) and GitLab (its
      token), for pushes to the app's branch; the pushed commit is built even if the branch
      moved on since. Failures are rate limited
- [x] Git runs with no local configuration, no prompts, only the https and ssh transports
      (no file:// or helper programs), and paths are kept inside the repository, also
      through links. Deploy keys and webhook secrets are sealed with the panel's key
- [x] Git apps are for administrators for now (builds run as root's Docker)
- [x] Tested in the VM with a repository served over SSH by the VM itself: deploy with the
      deploy key and a build argument (15 s); 889 requests during two releases all answered
      200, slowest 63 ms; a release whose container exits was refused within seconds while
      the old one kept serving; rollback; a signed push of an older commit built that commit;
      a wrong signature was refused
- [x] Found and fixed while testing: revoking an agent's certificate (new connection command,
      or deleting the server) did not close the connection the agent already had; it now does
- [ ] Deploy notifications on success, cancelling a running build: later

**2g. Railpack builds, previews and Git apps for customers — done** (phase 2 complete)
- [x] Builds without a Dockerfile: Railpack detects the language and framework and plans the
      build, which BuildKit runs through Railpack's frontend. "Automatic" uses the Dockerfile
      when the repository has one. Railpack is one pinned version (0.43.0), downloaded on first
      use and checked against its published SHA-256; variables reach it and the build by name,
      their values through the environment. Git apps get PORT set to their port
- [x] Previews: with previews on, each pull request to the app's branch (GitHub, Gitea,
      Forgejo, Gogs; merge requests on GitLab) gets an app of its own at
      `<app>-pr<number>.<preview domain>`, built from the forge's pull request ref and rebuilt
      on every push, then removed with its data when the request closes or merges. Pull
      requests from forks are never previewed (their code would run on the server); at most 5
      at a time; deleting an app deletes its previews
- [x] Customers can have Git apps when their package includes them. Their builds run with
      their slice as cgroup parent, and their containers inside it
- [x] A first release that does not stay up now fails the deploy instead of being called live,
      and its container is removed
- [x] Tested in the VM: a Node app without a Dockerfile built with Railpack (Node detected,
      PORT and variables at run time; 5 minutes the first time, with downloads); a pull
      request ref deployed at its own host name next to main, a fork's pull request refused,
      closing removed the preview's containers and builds; a customer's build ran in their
      slice; a customer's first deploy of a crashing build failed and left nothing running
- [x] Found and fixed while testing: closing a pull request crashed the webhook (no user
      behind it when queueing the apply); a crashing first release was reported live
- [ ] Comments on the pull request with the preview's address (needs a token for the forge),
      previews of forks after approval: later

**Next**
- [ ] Phase 3: built-in DNS, email with webmail, WordPress toolkit, security suite
