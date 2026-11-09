# Contributing to ekPanel

Thank you for helping. Contributions of every size are welcome: code, translations, docs,
bug reports and design feedback.

## Development setup

Requirements: Linux, Go 1.27+ (an older `go` downloads 1.27 by itself), Node.js 22+, `make`.
You do **not** need PostgreSQL or Docker: `make dev` starts a private embedded PostgreSQL
inside `.data/`, and the tests start their own.

```bash
make ui          # install web dependencies and build the UI into the panel
make dev         # run the panel at http://127.0.0.1:8080 (prints a setup token)
make dev-ui      # optional: Vite dev server with hot reload at http://localhost:5173
make dev-agent TOKEN=... PIN=...   # enrol and run an agent on this machine
make test        # everything CI runs
```

`make help` lists all tasks. To start over, stop the panel and delete `.data/`.

### A real test server

Anything that changes a server (the installer, and every hosting feature from phase 1 on)
must be tested on a real Ubuntu machine. `scripts/test-vm.sh` makes a throwaway Ubuntu 24.04
VM with QEMU/KVM, without root:

```bash
scripts/test-vm.sh create                         # first time: downloads Ubuntu (~600 MB)
make panel agent
scripts/test-vm.sh copy bin/ekpanel bin/ekpanel-agent deploy/install.sh
scripts/test-vm.sh ssh 'sudo ./install.sh --local-dir .'
# the panel is now at https://127.0.0.1:8443 (self-signed certificate),
# and its agent port at 127.0.0.1:17443 for testing agents from this machine
scripts/test-vm.sh stop                           # or destroy, to delete its disk
```

## Branches

All work happens on **`dev`**. **`pub`** holds published code and changes only through
a pull request from `dev`. Open your pull requests against `dev`.

## Project layout

| Path | What it is |
| --- | --- |
| `cmd/ekpanel` | the panel binary |
| `cmd/ekpanel-agent` | the agent binary that runs on managed servers |
| `cmd/ekctl` | the command-line tool for the API (logic in `internal/cli`) |
| `internal/api` | HTTP API, sessions, CSRF protection, agent endpoints |
| `internal/store` | PostgreSQL queries |
| `internal/db/migrations` | schema migrations, applied in name order at startup |
| `internal/agent` | metric collection and the agent's connection loop |
| `internal/agentproto` | messages shared by the panel and agents |
| `internal/hub` | live server state, fan-out to browsers, routing operations to agents |
| `internal/pki` | the panel's private CA for agent mutual TLS |
| `internal/catalog` | the one-click service catalogue: Compose templates and their variables |
| `internal/tasks` | background task queue and its handlers |
| `web/` | React + TypeScript UI (Vite, Tailwind, TanStack Query, i18next) |

## Guidelines

- **Every UI string is translated.** Never write user-facing text directly in a component:
  add a key to `web/src/i18n/locales/en.json` and use `t("key")`. If you can, add the
  Bangla too; otherwise `npm run i18n:check` shows what is missing for translators.
- **Format with Intl.** Use the helpers in `web/src/lib/format.ts` for numbers, sizes, rates
  and dates so they follow the user's language.
- **Right-to-left safe.** Use logical Tailwind classes (`ms-`, `me-`, `ps-`, `pe-`, `start-`,
  `end-`, `text-start`) instead of left/right. Flip directional icons with `rtl:-scale-x-100`.
- **API errors carry a stable `code`.** The UI translates `errors.<code>`, so add both.
- **Security first.** Validate all input on the server, never log secrets, store only hashes
  of tokens, and add an audit entry for every change an admin or user makes.
- **Tests.** New behaviour comes with tests. API behaviour is tested end to end against a real
  PostgreSQL in `internal/api/api_test.go`.
- Run `gofmt` and `make test` before opening a pull request.

## Trying the DNS features without Cloudflare

`go run ./internal/cloudflare/cloudflaretest/fakecf -zones example.com` serves an in-memory
copy of the parts of Cloudflare's API ekPanel uses. Start the panel with
`EKPANEL_CLOUDFLARE_API=http://127.0.0.1:18080/client/v4` and connect with the token
`test-token`.

## Trying email

Turn email on for the test VM's server with a host name such as `mail.php.test` and the
self-signed certificate. Mail delivered inside the VM (to 127.0.0.1 port 25) reaches
mailboxes; webmail answers with `curl -k --resolve mail.php.test:443:127.0.0.1
https://mail.php.test/`. Mail to the outside cannot leave the VM and stays queued. The
agent's administrator login for Stalwart's API (127.0.0.1:18900) is in
`/etc/ekpanel/mail/admin.env` on the server.

## Adding a service to the catalogue

Each service is a Compose file in `internal/catalog/templates/<id>.yaml` and an entry in
`internal/catalog/catalog.go` (name, category, images, the service and port that get the
domain, and its variables). Customers can install templates, so they must follow the rules
that `go test ./internal/catalog` checks: named volumes only, no published ports, no
privileged mode, capabilities, devices, host namespaces or Docker socket. Values that differ
per install are `${VARIABLES}`: generated passwords and keys, or fixed text using `{domain}`.
Pin a major version of the image where the project publishes one, describe the service in
`catalog.apps.<id>` in `en.json` (and `bn.json` if you can), and test it on a real server.
Raise `Version` whenever you change a template.

## Making a release

Only the project's owners can publish a release; this is how it works.

1. In `CHANGELOG.md`, rename `## [Unreleased]` to `## [v1.2.3] - YYYY-MM-DD` and add a new
   empty `## [Unreleased]` above it. Versions follow [Semantic Versioning](https://semver.org/).
2. Open a pull request from `dev` to `pub`; an owner merges it.
3. An owner tags the merge on `pub` and pushes the tag:
   `git tag -s v1.2.3 origin/pub && git push origin v1.2.3`.
4. The **Release** workflow checks that the tag is on `pub` and that the changelog has the
   version, builds the archives (`make release`), signs `SHA256SUMS` with the key in the
   `release` environment (`make sign`), adds build attestations and publishes the release.
   Tags with a pre-release part (`v1.2.3-rc.1`) become pre-releases, which panels do not offer
   as updates.

Panels find the new release within 6 hours and offer it under **Settings → Updates**.

To build and sign a release on your own machine (with your own key; the panel and installer
only trust it if you build them with `RELEASE_EXTRA_KEY`):

```bash
openssl genpkey -algorithm ed25519 -out my.key && openssl pkey -in my.key -pubout -out my.pub
make release VERSION=v0.0.1-test RELEASE_EXTRA_KEY="$(base64 -w0 my.pub)"
make sign KEY=my.key PUBKEY=my.pub
```

## License of contributions

ekPanel is licensed under [AGPL-3.0-only](LICENSE). By opening a pull request you agree
that your contribution is released under the same license.

## Reporting bugs

Open an issue with what you did, what you expected and what happened, plus the panel and agent
versions (`ekpanel version`, `ekpanel-agent version`) and the server's OS. For security
problems, follow [SECURITY.md](SECURITY.md) instead.
