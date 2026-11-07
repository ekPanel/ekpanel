# Security policy

ekPanel controls servers with root privileges, so we treat security reports as the highest
priority.

## Reporting a vulnerability

**Do not open a public issue.** Use GitHub's private vulnerability reporting
("Security" tab → "Report a vulnerability") on the project repository. Include:

- what an attacker can do, and what they need first (an account? network access?)
- steps to reproduce, or a proof of concept
- the panel and agent versions

We aim to acknowledge reports within 3 days and to fix critical issues within 14 days, and we
credit reporters in the release notes unless you prefer otherwise.

## Supported versions

ekPanel is in early development and has no stable release yet. Fixes go into the `dev` branch.

## Design principles

- Agents accept only typed, validated operations from a fixed list (for example, only
  allowlisted services can be restarted). There is no operation that runs arbitrary commands.
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
  they cannot run Compose files. An administrator acting as a customer is recorded as
  themselves in the audit log and cannot change the customer's password or two-factor setup.
- Every change is written to an append-only audit log that the database refuses to modify.
