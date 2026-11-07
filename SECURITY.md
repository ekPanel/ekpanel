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
- State-changing requests are protected against CSRF (`Sec-Fetch-Site`, `Origin` and
  JSON-only bodies); session cookies are `HttpOnly`, `SameSite=Lax` and `__Host-` prefixed.
- Strict Content-Security-Policy with no inline scripts.
- Every change is written to an append-only audit log that the database refuses to modify.
