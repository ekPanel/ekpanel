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
- Cloudflare DNS as the recommended way (one click, kept in sync), built-in DNS (PowerDNS) as
  an option
- Email (Stalwart + Roundcube), auto DKIM/SPF/DMARC
- WordPress toolkit, app installer
- Security suite (CrowdSec, country blocking, malware scan; WAF later)
- FTP, PHP extensions/ini editor, redirects, hotlink protection

### Phase 4 — "Beyond"
- Signed releases, one-line installer from releases, self-update of panel and agents
- Multi-server: place sites/apps on any node, move between nodes
- Reseller accounts, white-label, billing integrations
- Importers (cPanel, aaPanel, Coolify)
- WAF (Coraza) and rate limiting
- Public API docs site, `ekpanel` CLI, Terraform provider

### Phase 5 — Game servers
- Minecraft and other game servers in containers, console, files, startup variables

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
- **Branches**: all work happens on `dev`; `pub` holds published code and changes only
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

## 12. Phase 3 plan

Agreed (2026-11-07):

- **DNS: Cloudflare first.** ekPanel recommends keeping domains on Cloudflare and manages them
  there in one click, kept in sync through Cloudflare's API. The panel's own DNS (PowerDNS) is
  an option for people who do not use Cloudflare, and comes last.
- **Mail: Stalwart** (AGPL-3.0, one binary with SMTP, IMAP, JMAP, spam filter and DKIM), driven
  through its API. **Webmail: Roundcube**, hosted as a PHP site.
- **Security: CrowdSec now, WAF later.** 3e uses the packaged Caddy. A WAF (Coraza) needs a
  Caddy built with modules, which ekPanel would then build and ship itself; that comes later.
- **Order:** 3a → 3b → 3c → 3d → 3e → 3f → 3g, each tested in the VM like phase 2.
- **Built-in DNS: PowerDNS with SQLite, zones kept in the panel.** The panel is the source of
  truth and sends the primary each whole zone; secondaries copy it by zone transfer, which
  also carries DNSSEC signatures. No database server is needed on name servers.

**3a. Website extras — done**
- [x] Redirects (301, 302, 307, 308) of exact paths or prefixes (/blog/*), keeping the rest of
      the path if wanted, in the order listed; the extra domains of a site can redirect to its
      main domain. Hotlink protection: chosen file types answer 403 to pages of other sites,
      with sites that may embed them (*.example.com for all subdomains)
- [x] PHP settings per site from a list (memory_limit, upload and post size, execution and
      input time, input vars, file uploads, session lifetime, display_errors, short_open_tag,
      allow_url_fopen, date.timezone). Applications may still raise them, except
      allow_url_fopen. The panel and the agent check every value with the same rules, so none
      can add lines to the web server's or PHP's configuration
- [x] PHP extensions per server and version: 17 optional ones (redis, imagick, mongodb, apcu,
      xdebug …) added or removed as a task, then every PHP-FPM master of that version reloads;
      the ones ekPanel installs are locked
- [x] Own certificates: checked to fit their key, be valid now and cover the domain (other
      names it does not cover are listed); the key is sealed in the panel and readable only by
      root and the web server on the server. A notification goes out two weeks before expiry
- [x] Tested in the VM: every redirect kind and the alias redirect; hotlinked images refused
      (also from php.test.evil.com) and allowed from the site, a partner and without a
      referrer; PHP saw the new values; redis loaded after installing and gone after removing;
      the uploaded certificate was served
- [x] Found and fixed while testing: on installs without a domain, the panel's own Caddy site
      (":443") made Caddy use the panel's self-signed certificate for every website, so real
      domains would have shown certificate errors. The panel now answers on its addresses only

**3b. Cloudflare DNS — done**
- [x] Connect a Cloudflare account with an API token (checked with Cloudflare, then sealed);
      the DNS page explains which permissions to give it. Customers connect their own, and
      their zones only get their own websites and apps
- [x] One click per domain of the account (or add a new domain to Cloudflare, with the
      nameservers to set at the registrar): every website, alias, app and preview under it gets
      A and AAAA records for its server, proxied or not (a default per zone, a switch per
      record), marked "Managed by ekPanel"
- [x] Kept in sync on every change of a website or app and every 10 minutes: records follow a
      server's new address and go with their website. ekPanel changes only records it made and
      that nobody changed since; a record changed by hand, or one of someone else's in the way,
      is shown with a button to use ekPanel's value. Each hostname goes to its most specific
      zone
- [x] Servers report their addresses; the first public one is used (not private, shared NAT or
      site-local), or one set by hand for servers behind NAT
- [x] Zone editor for A, AAAA, CNAME, MX, TXT, CAA and NS records; the zone's SSL mode with a
      one-click switch to Full (strict); cache purge; stop managing (keeping or deleting the
      records ekPanel made, never ones changed by hand)
- [x] Caddy trusts Cloudflare's published addresses, and only those, for CF-Connecting-IP, so
      logs, traffic and later bans see visitors' real addresses
- [x] Tested against an in-memory Cloudflare (also runnable: `go run
      ./internal/cloudflare/cloudflaretest/fakecf`) in Go tests and in the VM: a domain added
      from the panel, records made once the server had a public address, a record changed by
      hand reported and restored, records following a new address; a request from a Cloudflare
      address logged with the visitor's address, a spoofed header from elsewhere ignored
- [ ] Not yet tried against the real Cloudflare API: needs an account and a test domain
- [x] Mail records: with 3c
- [ ] DNS-01 certificates (wildcards) through Cloudflare

**3c. Email — done**
- [x] Email per server: an administrator picks the mail host name (MX target, SMTP greeting,
      webmail address); the agent installs Stalwart 1.0.3 (checked against its published
      SHA-256), first in Stalwart's recovery mode to move its listeners off ports 443 and 8080,
      then keeps them to SMTP 25, submission 465 and 587, IMAPS 993 and POP3S 995, with the
      management API on loopback only. The mail ports are opened in a managed firewall.
      Turning email off stops the mail server and keeps its data
- [x] Mail domains (customers: only the domains of their own websites), mailboxes with sizes
      and passwords (SHA-512 crypt; the panel makes one if none is given and shows it once),
      forwarders to local or outside addresses, catch-all, forwarding with or without a kept
      copy, and auto-replies with optional dates (one Sieve script per mailbox, which mail
      apps cannot change). Usage per mailbox. Packages limit mail domains, mailboxes and
      mailbox size; mail counts toward the package's disk
- [x] DKIM keys (RSA and Ed25519) made by Stalwart per domain. MX, SPF, DKIM, DMARC and TLS
      reporting records, and the mail host name's A/AAAA records (never proxied), are
      published to Cloudflare with 3b's sync, next to other TXT records at the same names; or
      shown to copy for other DNS hosts
- [x] Webmail: Roundcube 1.7.5 (checked download) at https://<mail host name>/, run by its own
      PHP-FPM pool and account, signing in to the mail server over loopback. Caddy gets the
      host name's certificate, and the agent hands the same certificate to the mail server
      (and again after each renewal)
- [x] Delivery check: the host name's DNS, outgoing port 25, reverse DNS of each public
      address, and three blocklists
- [x] Tested in the VM: mail to a mailbox, a forwarder and an unknown address (catch-all)
      arrived; forwarding queued for outside; the auto-reply answered; sending as another
      address was refused; IMAP served the Caddy certificate; webmail signed in; Roundcube's
      installer and configuration were not reachable; a deleted mailbox could no longer sign
      in; deleting the domain and turning email off removed everything and stopped the
      service
- [x] Found and fixed while testing: Stalwart refuses to delete its default domain, so the
      last mail domain could never go. The mail host name is now the default domain
- [ ] Not yet tried with real outside mail (needs a server with port 25 open and reverse DNS)
- [ ] Later: per-domain mail host names and autoconfig for mail apps, DNS-01 wildcard
      certificates, mailing lists with archives

**3d. WordPress toolkit — done** (the PHP app installer is 3d-2)
- [x] Every WordPress site in one list, with what needs attention; every PHP site is looked
      at daily, so WordPress installed by hand is found too (or right away with a button)
- [x] Core, plugin and theme updates as a task, one at a time: the files are copied first,
      the site's home page is fetched after each update, and an update that makes it fail
      (server error or WordPress's critical error page) is undone; core goes back by
      downloading the previous version. Problems are notified
- [x] Automatic updates by rules per site (core: none, security releases or all; plugins;
      themes), made by the daily check with the same safety
- [x] Maintenance mode, plugins on and off (off works even when a plugin breaks WP-CLI),
      debug mode warning
- [x] Hardening: DISALLOW_FILE_EDIT; xmlrpc.php and PHP under wp-content/uploads refused by
      the web server; files others could write found and fixed, wp-config.php private
- [x] Staging: a site of its own at staging.<domain> with its own database (counted in the
      package), files and database copied, addresses replaced (also in serialized data),
      search engines asked not to index it. Push to live and refresh from live, of files,
      database or both, with the target in maintenance mode meanwhile and its previous
      database and replaced files kept in its tmp folder
- [x] Tested in the VM: an update that broke the site was undone (and the same update
      without the fault went through); a security release of core; maintenance mode
      answered 503; permissions fixed; xmlrpc.php and uploads PHP refused while the site
      and REST API kept working; a staging copy made, a post written there pushed to live,
      live kept its address and search engine setting, no staging address left behind

**3d-2. PHP app installer — done**
- [x] Joomla 6.2.0 and Moodle 5.2.4 from their release archives, checked against the SHA-256
      their publishers list; Drupal 11 (with Drush) and Laravel 13 made with a pinned, checked
      Composer 2.11.1 (their packages come from Packagist)
- [x] Into an empty PHP website, each with a MariaDB database of its own (counted in the
      package), a one-time admin password (none for Laravel), and the PHP version each needs
      checked first. The installers' passwords travel in the environment, which only the
      site's account can read, never as arguments other accounts could see
- [x] The site serves the app's public folder (a document root setting for every site:
      Drupal web/, Moodle and Laravel public/), Moodle gets max_input_vars 5000, its data
      folder outside the website and a cron job every minute
- [x] A failed install leaves public_html and the database empty again, ready to retry
- [x] Tested in the VM: all four installed and answered (home, admin or login pages); each
      admin password matched the hash in the app's database; Moodle's cron ran
- [x] Found and fixed while testing: Joomla's table prefix must start with a letter; Moodle
      5.3 needs MariaDB 11.4 (Ubuntu 24.04 has 10.11), so 5.2 is installed; the agent's
      downloads were limited to 64 MB; Drush's command files are shell wrappers
- [ ] PrestaShop: version 9 publishes no download with a checksum yet

**3e. Security — done** (malware scans and country blocking are in 3e-2)
- [x] CrowdSec per server, turned on from the panel: installed from CrowdSec's own package
      repository with the nftables firewall bouncer, its local API moved off port 8080 (the
      panel's), reading SSH and every website's and app's log; detections for SSH, web
      attacks, known CVEs and WordPress. Turning it off stops it and its bans
- [x] Bans: detected and by hand (single public addresses, from one hour to a year, with a
      reason), lifted from the panel; recent attacks with country; an allowlist that also
      covers the community blocklist, always including the server's own and Cloudflare's
      addresses
- [x] Behind Cloudflare the visitor's own address is detected and banned, never
      Cloudflare's (so bans only stop direct access; noted in the panel)
- [x] Overview per server: firewall, CrowdSec, root and password sign-in over SSH, pending
      and security updates, automatic security updates (one click to turn on), restart
      needed, and administrators without two-factor authentication
- [x] Tested in the VM: installed and running beside the panel; WordPress password guessing
      through Cloudflare and SSH password guessing banned; an allowlisted address not
      banned; bans by hand reached nftables and were lifted; turned off cleanly

**3e-2. Malware and countries — done**
- [x] Malware scans with ClamAV (from the distribution, its database kept current by
      freshclam): off, only when started, daily or weekly, of every website's home folder at
      the lowest CPU and disk priority, without following links; one scan per server at a
      time; less than 2 GB of memory is warned about
- [x] Files found: listed per server for administrators and on the website's page for its
      owner, with alerts to both. Quarantine (moved into a root-only folder, automatically
      if chosen), delete, put back (marking it safe) and mark safe; owners may quarantine
      and delete. Every file action runs as the site's account, never as root. Files fixed
      or deleted drop off after the next scan; files marked safe stay quiet until they match
      something else
- [x] Country blocking: block some countries, or allow only some, by DB-IP's free country
      list (CC BY 4.0, credited in the panel), downloaded once a month. An nftables table
      of its own ahead of ufw, CrowdSec and Docker drops new connections; port 80 stays open
      for certificates, and the allowlist, the server's own, Cloudflare's and private
      addresses are never blocked. The rules load again at boot. The panel refuses rules
      that would block the administrator setting them
- [x] Tested in the VM: real packets from Chinese, Singaporean and Bangladeshi addresses
      (a network namespace) blocked and let through as set, for HTTPS and SSH, with port 80
      open; the rules restored after loss; outgoing traffic unaffected in "allow only" mode;
      blocking yourself refused. ClamAV installed with its database; EICAR test files found
      (also inside a ZIP and with ": " in the name), quarantined, put back with their
      permissions, deleted; a link to /etc/shadow not followed; automatic quarantine; a file
      put back stayed quiet on the next scan. A scan takes about 1 GB of memory, and a full
      scan of 3.4 GB in 222,000 files runs well over an hour at the lowest priority
- [x] Found while testing: nftables keeps only the last "elements" line of a set (ranges
      now go in "add element" lines); a second "Scan now" started a second scan (it now
      joins the running one)

**3f. FTP — done**
- [x] Pure-FTPd, installed when the first site turns FTP on and stopped when the last turns
      it off. Each site gets a login of its own (the site's account name, a separate
      password shown once, SHA-512 crypt only) in Pure-FTPd's own user list, mapped to the
      site's account and locked into its home folder; no system account can sign in
- [x] TLS required for every session (a self-signed certificate, accepted once in the FTP
      program); port 21 and passive ports 30000-30100 opened in a managed firewall while any
      site has FTP; the public address given to clients on servers behind NAT
- [x] Packages say whether customers may use FTP (off by default)
- [x] Tested in the VM: plain FTP refused, FTPS sign-in, upload served by the website, no way
      out of the home folder, root and other accounts refused, a new password replaced the
      old, firewall ports opened and closed, Pure-FTPd stopped when the last site turned it off

**3g. Built-in DNS — done**
- [x] Name servers: ekPanel servers chosen on the DNS page, each with its host name
      (ns1.example.com); one primary and any number of secondaries. PowerDNS from the
      distribution with its SQLite backend, driven through its API on loopback, listening on
      the server's own addresses (systemd-resolved keeps 127.0.0.53); port 53 opened in a
      managed firewall. The primary sends zones to the secondaries by NOTIFY and AXFR, allowed
      to those addresses only; zone transfers use public addresses, or one set by hand (a
      private network). A server that stops being a name server stops PowerDNS
- [x] Zones kept in the panel: administrators' and customers' (customers only for the
      domains of their own websites and apps, never a public suffix, and not inside or around
      another account's zone); a domain is either hosted or at Cloudflare, not both. The
      primary gets each zone whole and only the changes are written, so the serial moves only
      when something changed
- [x] Zone editor: A, AAAA, CNAME, MX, TXT (any length), SRV, CAA and NS for subdomains,
      checked by panel and agent (a CNAME alone at its name, none at the domain itself)
- [x] Records made automatically, as with Cloudflare: websites', apps' and the mail host's
      addresses, and every mail domain's MX, SPF, DKIM, DMARC and TLS reporting records; the
      zone's NS records and glue for name servers named inside it. A record kept by hand with
      the same name and type wins and is reported
- [x] Templates: records for new zones, "@" and relative names, {domain} in values
- [x] DNSSEC per zone: signed on the primary (ECDSA P-256, NSEC), copied signed to the
      secondaries; the DS record for the registrar shown; turning it off warns to remove the
      DS record first
- [x] Guidance for the registrar: the name servers to set, and the glue (host) records when
      they are named inside the domain
- [x] Found while testing: PowerDNS cannot listen on all addresses beside systemd-resolved;
      a fresh server had no package lists, so installs now refresh them and retry (also for
      ClamAV)

**Next**
- [x] Phase 3 is done; phase 4 next (section 13)

---

## 13. Phase 4 plan

Agreed (2026-11-09):

- **Release first.** Nobody else can install ekPanel, or keep it current, until there are
  signed releases, an installer that downloads them and self-update. The GitHub workflow is
  written and tried locally; nothing is pushed or published without the owner's go-ahead.
- **In phase 4:** release and updates, the API with a CLI (and later Terraform), moving
  between servers, importers, resellers and white-label, the WAF, billing integrations.
- **Game servers move to phase 5.** They are a module of their own (Pterodactyl's scope) and
  wait until the hosting side is released and in use.
- **Order:** 4a → 4b → 4c → 4d → 4e → 4f → 4g, each tested in the VM like phase 3. Billing
  comes last: it builds on the API tokens (4b) and resellers (4e).

**4a. Releases and updates**
- [x] Release builds in CI (linux amd64 and arm64) with checksums and an Ed25519 signature,
      reproducible from a tag; the web UI built into the panel. Built and signed locally;
      the workflow runs on GitHub with the first tag
- [x] One-line installer that downloads a release, checks its signature and installs panel,
      agent and Caddy; also installs an agent alone for a second server (`--agent`)
- [x] Self-update: the panel shows a newer release, updates itself (keeping the old binary
      to go back to), then updates every agent through a typed operation, one server at a time
- [x] Release notes and a changelog; versions shown in the panel per server
- [x] Tried in the VM with test releases signed by a test key: install from a release; panel
      and agent updates; a panel that does not start (put back after 90 s); an agent that
      does not start (put back by its timer after 3 min); wrongly signed and changed
      downloads refused; a second server set up with `--agent`, then updated
- [ ] Weblate (or another translation site) set up before the first public release

How 4a works (decided 2026-11-09):
- **Signing.** CI signs `SHA256SUMS` with an Ed25519 key (secret `RELEASE_SIGNING_KEY`, only
  in the `release` environment); the public key is in `internal/release/release.pub`, built into
  both binaries and the installer. `openssl` checks it in the installer, Go's `crypto/ed25519`
  in the agent, so nothing extra is installed. GitHub build attestations come on top.
- **Releases** are made by pushing a `vX.Y.Z` tag on `pub` (a tag ruleset lets only owners
  do that). Assets: one archive per architecture, `SHA256SUMS`, `SHA256SUMS.sig`,
  `install.sh` (with its version written in) and `release.json` (the version, for update
  checks). Builds are reproducible: `-trimpath`, no build ID, fixed times in the archives.
- **Installer**: `curl -fsSL …/releases/latest/download/install.sh | sudo bash`. It downloads
  and checks a release (or uses `--local-dir`). `--agent --panel … --token … --ca-pin …`
  sets up a second server: Caddy, `acl`, the Caddyfile import, the agent and its unit.
- **Update check**: the panel reads `release.json` every 6 hours (`EKPANEL_UPDATE_CHECK=false`
  turns it off) and tells administrators once per new version.
- **Self-update** (administrators, Settings → Updates): the panel asks the agent on its own
  server to update it: download, check the signature and hash, run the new binary once,
  dump the panel database to `/var/backups/ekpanel/`, swap the binary (the old one stays as
  `ekpanel.prev`) and restart the panel. If the panel does not answer its health check within
  90 seconds, the agent puts the old binary back. The new panel then updates the agents one
  server at a time and waits for each to come back with the new version. An agent sets a
  systemd timer before it restarts itself: if the new agent has not reached the panel within
  3 minutes, the timer puts the old binary back. Versions only go up.

**4b. API, CLI, Terraform**
- [ ] API tokens per user with scopes (read, websites, databases, DNS …) and expiry, shown
      once, revocable, in the audit log
- [ ] An OpenAPI description of the API, checked in CI against the handlers, and a docs site
- [ ] The `ekpanel` CLI for the same API (sites, databases, apps, DNS, backups), with tokens
- [ ] Terraform provider: later in phase 4, once the API is stable

**4c. Moving between servers**
- [ ] Move a website (files, databases, cron, settings), an app or a database to another
      ekPanel server: copy, switch DNS (hosted or Cloudflare) and the certificate, then remove
      the old copy after a check
- [ ] Restore a backup onto another server; move a whole customer
- [ ] Server-to-server copy over the agents' own connection, not public SSH

**4d. Importers**
- [ ] cPanel backups (cpmove): accounts, files, MySQL databases and users, mail accounts and
      mail, DNS zones, cron jobs, SSL certificates
- [ ] aaPanel sites and databases; plain WordPress from another host (files and database)
- [ ] Coolify applications and services (Compose and environment)
- [ ] A report of what was imported and what was not

**4e. Resellers and white-label**
- [ ] Resellers: their own packages and customers within the limits an administrator gives
      them; they see only their customers
- [ ] White-label: name, logo, colors and the panel's domain, per panel and per reseller
- [ ] Sub-users of an account with chosen permissions (only files, only email …)
- [ ] Passkeys (WebAuthn) besides passwords and two-factor codes; active sessions listed and
      ended from the panel

**4f. Security center**
- [ ] WAF: Coraza with the OWASP Core Rule Set in a Caddy that ekPanel builds and ships (with
      the modules it needs), per site on/off and in detect-only mode first
- [ ] Rate limiting per site and for the panel's sign-in, fail2ban-style blocking for
      services CrowdSec does not read

**4g. Billing integrations**
- [ ] Provisioning modules for WHMCS, Blesta and Paymenter: create, suspend, unsuspend,
      change package and remove accounts, single sign-on into the panel
- [ ] Built-in billing (Stripe, bKash, SSLCommerz): after the modules, if still wanted

**Smaller things left from earlier phases** (taken along where they fit)
- [ ] Database browser and import/export (4b or 4c)
- [ ] Single-file restore and downloading a backup (4c)
- [ ] Suspending a customer, bandwidth limits in packages, alerts for failed cron jobs (4e)
- [ ] DNS-01 wildcard certificates through Cloudflare or the built-in DNS (4a or later)
- [ ] Moving a website, database or app to another owner (4e)
- [ ] Not yet tried for real: the Cloudflare API, outside mail delivery (need an account,
      a domain and a server with port 25)

**Found while writing the documentation** (2026-11-09)
- [x] A second server needs Caddy, `acl`, the Caddyfile import, a systemd unit and a
      readable /etc/ekpanel; the agent sets up none of these (4a: `install.sh --agent`)
- [ ] Two-factor sign-in has no recovery codes, and administrators cannot reset someone's
      two-factor (4e)
- [ ] The email page asks to add DNS records by hand for domains on ekPanel's own name
      servers, although ekPanel adds them there itself
- [ ] Backups taken before a staging push or pull are never cleaned up; a failed WordPress
      install is not cleaned up like the PHP app installer's
- [ ] Backups leave out app volumes and mailboxes; a restore puts back `public_html` only
- [ ] The Bangla UI spells some words two ways (ডাটাবেস/ডেটাবেস, মেমরি/মেমোরি,
      সেটিং/সেটিংস) and mixes Bangla and English script in the name server labels

**Next**
- [x] 4a. Releases and updates; the signing key is in the `release` environment
- [x] v0.1.0, the first release, published 2026-11-09 (signature, checksums and attestation checked)
- [ ] 4b. API, CLI, Terraform
