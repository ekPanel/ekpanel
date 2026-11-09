# Changelog

All notable changes to ekPanel are listed here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions will follow
[Semantic Versioning](https://semver.org/) from the first release on.

## [Unreleased]

Everything so far, grouped by roadmap phase (see [PLAN.md](PLAN.md)). There is no release yet.

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
