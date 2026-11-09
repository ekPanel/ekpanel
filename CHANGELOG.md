# Changelog

All notable changes to ekPanel are listed here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added
- API tokens (Account → API tokens): keys for scripts and the command-line tool, with
  scopes (read, websites, databases, apps, DNS, email, backups, servers, customers, settings)
  and an expiry; shown once, revocable, and named in the audit log. Tokens can never change
  account security, manage tokens or open a terminal (4b)
- An OpenAPI 3.1 description of the whole API at `/api/v1/openapi.json` (also
  `docs/openapi.json`), made from the router, with the scope each operation needs (4b)
- `ekctl`, a command-line tool for Linux, macOS and Windows: servers, websites, databases,
  apps, DNS, backups and tasks, `--json` output, and `ekctl api` for any endpoint (4b)
- Move a website to another server (with the databases you choose), from its page: files,
  databases and certificate are copied straight between the servers' agents, the new server
  is checked before anything switches, DNS the panel manages follows, and the old copy keeps
  serving until you remove it. A failed move leaves the website where it was (4c)
- Move an app to another server, from its page: the app is stopped, its data volumes (and a
  Git app's current build) are copied with their owners, it starts on the new server with a
  free port there, and the old copy stays stopped with its data until you remove it (4c)
- Move a database on its own, and everything a customer has on one server (their first
  website takes all their databases along), from the database's page and the customers list (4c)
- Rebuild a website on another server from its newest backup, when its server is gone: connect
  the same S3 storage there with its recovery key, and the website's page offers the rebuild
  while the old server is offline (4c)
- Import a cPanel account from its full backup (Admin → Import): websites with their files,
  MySQL databases with the same users and passwords, and cron jobs; paths to the old home
  folder in configuration files and cron jobs follow, and a report lists what was not
  imported (4d)
- The cPanel import also brings email: mailboxes with the same passwords and quotas,
  forwarders, the catch-all and all the mail with its folders and read and flagged marks;
  and DNS zones, onto ekPanel's name servers, keeping the records ekPanel does not make
  itself (4d)

### Changed
- PHP websites serve a folder's `index.html` when it has no `index.php`, as Apache does (4d)

### Fixed
- Turning on email could fail once on a new server ("table users already exists") while
  setting up webmail

## [v0.1.0] - 2026-11-09

The first release: everything from roadmap phases 0 to 3 and the start of phase 4, grouped
by phase (see [PLAN.md](PLAN.md)). It is early software. Try it, test it and tell us what
breaks, but do not host anything important on it yet.

### Phase 4: releases and updates
- Signed releases: a version tag on `pub` builds reproducible archives for amd64 and arm64,
  signs `SHA256SUMS` with the ekPanel release key (Ed25519) and adds build attestations (4a)
- One-line installer from a release, which checks the signature before installing; `--agent`
  sets up another server (agent, Caddy and its configuration) with one command from the
  panel's "Add server" dialog (4a)
- Self-update from Settings → Updates: the panel looks for new releases every 6 hours, backs
  up its database, updates itself and then every agent one server at a time; a new version
  that does not start is replaced by the previous one automatically (4a)

### Phase 3: cPanel level
- Built-in DNS: PowerDNS name servers (a primary and secondaries kept in sync by zone
  transfer), hosted zones with a zone editor and templates, records for websites, apps and
  email made automatically, DNSSEC with the DS record for the registrar, glue guidance (3g)
- Malware scans with ClamAV and quarantine; country blocking with DB-IP's country list (3e-2)
- One-click Joomla, Drupal, Moodle and Laravel (3d-2)
- FTP over TLS with a login per website (3f)
- Server security: CrowdSec bans, an allowlist and a security checklist (3e)
- WordPress toolkit: safe updates with rollback, automatic update rules, hardening and
  staging copies (3d)
- Email: Stalwart mail server, mailboxes, forwarders, auto-replies and Roundcube webmail (3c)
- Cloudflare DNS kept in sync with websites and apps (3b)
- Redirects, hotlink protection, PHP settings and extensions, own certificates (3a)

### Phase 2: Coolify level
- Git deploys with Dockerfile or Railpack builds, zero-downtime releases, rollbacks and pull
  request previews (2f, 2g)
- One-click app catalogue (2e)
- Web terminal for servers and app containers (2d)
- Logs, traffic charts and notifications by email, Telegram, Discord, Slack and webhooks (2c)
- Cron jobs for websites and apps (2b)
- Customers, packages and limits enforced with cgroups (2a)

### Phase 1: host real sites
- Docker apps from an image or a Compose file (1f)
- Backups with restic and restore; a basic ufw firewall (1e)
- One-click WordPress (1d)
- File manager and SFTP, locked into each site's folder (1c)
- MariaDB and PostgreSQL databases (1b)
- Static and PHP websites with a desired-state engine (1a)

### Phase 0: foundations
- Panel and agent with mutual TLS, typed operations, a task queue and metric history
- Server installer and a local test VM script
- Multilingual web UI (English and Bangla), setup token, sign-in with two-factor codes

### Fixed
- Interrupted tasks were never recovered when another task with the same purpose was
  already queued

[Unreleased]: https://github.com/ekPanel/ekpanel/compare/v0.1.0...HEAD
[v0.1.0]: https://github.com/ekPanel/ekpanel/releases/tag/v0.1.0
