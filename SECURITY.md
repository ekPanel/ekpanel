# Security policy

ekPanel controls servers with root privileges, so we treat security reports as the highest
priority.

## Reporting a vulnerability

**Do not open a public issue.** Report privately in one of these ways:

- GitHub's private vulnerability reporting ("Security" tab → "Report a vulnerability") on
  the [project repository](https://github.com/ekPanel/ekpanel);
- email to **ekpanelbd@gmail.com** with "Security" in the subject.

Include:

- what an attacker can do, and what they need first (an account? network access?)
- steps to reproduce, or a proof of concept
- the panel and agent versions

We aim to acknowledge reports within 3 days and to fix critical issues within 14 days, and we
credit reporters in the release notes unless you prefer otherwise.

## Supported versions

ekPanel is early software (the first release, v0.1.0, came out on 2026-11-09). Only the newest
release gets security fixes: they go into `dev` and come out in the next release, which panels
offer under **Settings → Updates**. Keep your panel and agents on the newest release.

## Design principles

- Agents accept only typed, validated operations from a fixed list (for example, only
  allowlisted services can be restarted). The one way to run any command is the web terminal,
  which is an interactive shell for a person, not an operation the API can script.
- The web terminal gives administrators a root shell, and app owners a shell inside their own
  containers. Opening one requires the password (and two-factor code) again within the last
  15 minutes, is refused while acting as a customer, and is checked against the browser's
  origin. Shells end with the browser tab, after 30 idle minutes and after 8 hours, and every
  session is recorded in the audit log (not its keystrokes).
- Panel and agents talk over mutual TLS on a separate port, using a private CA that agents
  pin by fingerprint at enrolment. Agent keys never leave their server; certificates last
  90 days and renew automatically; re-enrolling or deleting a server revokes its certificate.
- Passwords are hashed with argon2id. Session and enrolment tokens are stored only as
  SHA-256 hashes.
- A fresh install requires a one-time setup token, so nobody else can claim it first.
- Every website runs as its own Linux account with its own PHP-FPM pool and `open_basedir`;
  site folders are closed to other accounts, and only the web server gets read access
  through ACLs. The agent never modifies an account it did not create.
- Database passwords are never stored: the panel keeps only MariaDB's native hash or
  PostgreSQL's SCRAM-SHA-256 verifier. The agent never adopts or drops a database it did not
  create, and customer PostgreSQL accounts cannot connect to the panel's database.
- File manager actions run in a worker process that is chrooted into the site's folder and
  runs as the site's account. SFTP accounts are chrooted too, have no shell, and keep their
  SSH keys in a root-owned file they cannot change.
- Web server configuration is validated before it is loaded; a rejected change is rolled
  back so one site cannot take others down.
- State-changing requests are protected against CSRF (`Sec-Fetch-Site`, `Origin` and
  JSON-only bodies); session cookies are `HttpOnly`, `SameSite=Lax` and `__Host-` prefixed.
- Strict Content-Security-Policy with no inline scripts.
- Backup encryption keys and storage credentials live only on the server (readable by root
  only). The panel never stores them; the key is shown to the user once as a recovery key.
- The firewall always keeps SSH (on the ports sshd uses), HTTP, HTTPS and the agent port open,
  and the panel refuses rules that would block the address of the person adding them.
- Docker apps publish their port on 127.0.0.1 only; Compose files that would publish ports on
  public addresses (which Docker opens past the firewall) are refused unless the user allows
  it. Compose files can still do anything Docker allows, so only administrators can add apps.
  App folders and variables are readable by root only.
- Customers see and change only what they own; everything else answers "not found". Their
  PHP and containers run in a cgroup slice with their package's memory and CPU limits, and
  they cannot run Compose files of their own. They may install catalogue services, whose
  Compose files are reviewed with the project and cannot be changed by them; the agent also
  checks the resolved file of every customer's app and refuses privileged mode, added
  capabilities, devices, host folders and namespaces, volumes with driver options, and
  secrets or configs read from the host, and puts every container in the customer's slice. An administrator acting as a customer is recorded as
  themselves in the audit log and cannot change the customer's password or two-factor setup.
- Git deploys clone with no local Git configuration, no prompts and only the https and ssh
  transports, keep the Dockerfile and build folder inside the repository (also through
  links), and pass variables to builds through the environment. Each app's deploy key and
  webhook secret are sealed with the panel's key; webhooks must be signed (HMAC-SHA256, or
  GitLab's token) and failures are rate limited. Git apps are for administrators, and for
  customers whose package includes them; a customer's builds and containers run in their
  slice. Pull requests from forks are never previewed, since their code would run on the
  server. Railpack is a pinned version, checked against its published SHA-256.
- Cloudflare API tokens are checked with Cloudflare and stored sealed; customers' tokens
  manage only their zones, which only get their own websites and apps. ekPanel changes or
  deletes only DNS records it made, and none that someone changed since. Caddy trusts the
  CF-Connecting-IP header only from Cloudflare's published address ranges.
- PHP applications: Joomla and Moodle archives are checked against their published SHA-256,
  and Composer (for Drupal and Laravel) is a pinned, checked version; the packages Composer
  fetches come from Packagist. Installers run as the site's account, and admin and
  database passwords reach them through the environment (readable by that account only),
  not as command-line arguments. Configuration files with database passwords are readable
  by the site's account only.
- FTP accepts TLS sessions only, and only the logins ekPanel lists for websites with FTP
  turned on (no system accounts, no anonymous access), each locked into its site's home
  folder. Its port and passive ports are open only while a site uses FTP.
- CrowdSec comes from its own signed package repository; its API listens on loopback only
  and the firewall bouncer's key is readable by root only. ekPanel never bans Cloudflare's
  addresses or the server's own, and bans by hand take single public addresses only. Its
  allowlist also keeps the community blocklist from blocking chosen addresses.
- Malware scans: clamscan reads the websites' home folders as root but follows no links,
  and every action on a file found (reading it into quarantine, removing it, putting it
  back) runs as the site's own account, so a file swapped for a link in between cannot
  reach anything the site cannot. Quarantine is a folder only root can read. Website
  owners may quarantine and delete their files; putting back and marking safe are for
  administrators.
- Name servers: PowerDNS's API listens on loopback only, with a random key in a file only
  root and PowerDNS can read. Zone transfers are allowed to the other name servers only, and
  secondaries accept NOTIFY from the primary only. Records are checked by panel and agent
  before they reach PowerDNS. Customers may host only the domains of their own websites and
  apps, never a public suffix, and never a zone inside or around another account's.
- Country blocking never applies to the allowlist, the server's own, Cloudflare's or
  private addresses, or to port 80, and the panel refuses rules that would block the
  administrator's own address. The country list (DB-IP's, CC BY 4.0) is data only: it is
  checked to be a well-formed list of address ranges before use.
- WordPress toolkit: WP-CLI always runs as the site's own account, and without loading
  plugins and themes except to switch a plugin on. Plugin and theme names are checked by
  panel and agent before they reach WP-CLI. Copies between a site and its staging copy run
  only between ekPanel's site accounts on one server and databases on that server, read from
  the sites' own wp-config.php; the database dump in between is readable by root only, and
  the overwritten site's previous database stays in its own tmp folder. Staging copies ask
  search engines not to index them.
- Email: the mail server (Stalwart) and webmail (Roundcube) are pinned versions checked
  against their published SHA-256. The mail server's management API listens on loopback
  only; the agent signs in with a random password kept in a root-only file on the server.
  Mailbox passwords are stored only as SHA-512 crypt hashes (with the usual 5,000 rounds,
  since every mail sign-in checks them). Mailbox and forwarder addresses are checked by
  panel and agent alike, so no input reaches the mail server's settings or filter scripts
  unchecked. Mail apps cannot change filter scripts (ManageSieve is off). Webmail accepts only full
  addresses as user names and three failed sign-ins a minute. Customers can add
  email only for the domains of their own websites.
- Revoking an agent's certificate (a new connection command, or deleting the server) also
  closes any connection the agent has open.
- Secrets the panel must reuse (notification channel tokens, webhook addresses and keys, the
  SMTP password) are encrypted with AES-256-GCM under a key in the panel's data directory and
  are never returned by the API. Customers' webhooks cannot reach private, local or
  link-local addresses (such as cloud metadata services).
- Every change is written to an append-only audit log that the database refuses to modify.
