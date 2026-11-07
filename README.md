# HostPanel

**An open-source hosting control panel for everyone, everywhere.** One panel for classic web
hosting (like cPanel and aaPanel), Git and Docker app deployment (like Coolify), and later
email, DNS and game servers, with a modern UI in your own language.

> **Status: early development (phase 0 of 6).** Do not use it for production hosting yet.
> See [PLAN.md](PLAN.md) for the full roadmap.

> "HostPanel" is a working name and may change before the first release.

## What works today

- One-time setup with a setup token, so nobody else can claim a fresh install
- Sign-in with argon2id password hashing, optional two-factor authentication (TOTP),
  login rate limiting, and "sign out other devices"
- Add Linux servers: run one command on the server, and its agent connects to the panel
  (outbound only, so it works behind NAT)
- Live CPU, memory, disk, network, load and uptime per server, streamed over WebSocket
- An append-only audit log of every change (the database itself refuses edits and deletes)
- A web UI in **English** and **বাংলা (Bangla)**, ready for right-to-left languages, with
  light/dark themes, a `Ctrl+K` command palette and phone-friendly layouts

Coming next, in order: websites with automatic SSL, databases, file manager, Docker apps,
backups, then Git deploys, an app store, customers and packages. [PLAN.md](PLAN.md) lists
every phase.

## How it fits together

```
Browser ──HTTPS──► hostpanel (Go: API + embedded web UI) ──► PostgreSQL
                          ▲
                          │ WebSocket, opened by each agent
              hostpanel-agent on every managed server (Go, single binary)
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
make dev-agent TOKEN=<token from the dialog>
```

For UI work with hot reload, run `make dev` and `make dev-ui` together, then open
http://localhost:5173. See [CONTRIBUTING.md](CONTRIBUTING.md) for details.

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

To be decided before the first public release. See [PLAN.md](PLAN.md#license).
