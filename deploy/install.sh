#!/usr/bin/env bash
# ekPanel installer for a fresh Ubuntu 24.04+ or Debian 12+ server.
#
#   sudo ./install.sh --local-dir DIR [--domain panel.example.com] [--email you@example.com]
#
# --local-dir   folder containing the ekpanel and ekpanel-agent binaries (built with
#               `make panel agent`). Downloading release binaries comes with the first release.
# --domain      a DNS name pointing at this server. Caddy gets a free Let's Encrypt
#               certificate for it. Without it the panel uses a self-signed certificate
#               on the server's IP address, and browsers show a warning.
# --email       contact address for Let's Encrypt expiry notices (optional).
#
# What it does: installs PostgreSQL and Caddy, creates the ekpanel system user and
# database, installs both binaries with hardened systemd units, connects this server's
# agent to the panel, and prints the one-time setup token for the first administrator.
# Running it again upgrades the binaries and keeps all data.

set -euo pipefail

LOCAL_DIR=""
DOMAIN=""
EMAIL=""

usage() { sed -n '2,20p' "$0" | sed 's/^# \{0,1\}//'; exit "${1:-0}"; }

while [ $# -gt 0 ]; do
  case "$1" in
    --local-dir) LOCAL_DIR="${2:-}"; shift 2 ;;
    --domain) DOMAIN="${2:-}"; shift 2 ;;
    --email) EMAIL="${2:-}"; shift 2 ;;
    -h|--help) usage 0 ;;
    *) echo "Unknown option: $1" >&2; usage 2 ;;
  esac
done

say()  { printf '\033[1;36m==>\033[0m %s\n' "$*"; }
fail() { printf '\033[1;31mError:\033[0m %s\n' "$*" >&2; exit 1; }

# ---- Checks -------------------------------------------------------------------------

[ "$(id -u)" -eq 0 ] || fail "run this installer as root (for example with sudo)."
[ -r /etc/os-release ] || fail "cannot detect the operating system."
. /etc/os-release
case "${ID:-}:${VERSION_ID:-}" in
  ubuntu:24.04|ubuntu:24.10|ubuntu:25.*|ubuntu:26.*|debian:12|debian:13) ;;
  *) fail "${PRETTY_NAME:-this OS} is not supported yet. Use Ubuntu 24.04+ or Debian 12+." ;;
esac
case "$(uname -m)" in
  x86_64|aarch64) ;;
  *) fail "CPU architecture $(uname -m) is not supported yet (amd64 and arm64 are)." ;;
esac
[ -n "$LOCAL_DIR" ] || fail "release downloads are not available yet; pass --local-dir with the built binaries."
for b in ekpanel ekpanel-agent; do
  [ -x "$LOCAL_DIR/$b" ] || fail "$LOCAL_DIR/$b not found or not executable."
done
if [ -n "$DOMAIN" ] && ! [[ "$DOMAIN" =~ ^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$ ]]; then
  fail "--domain must be a plain host name such as panel.example.com."
fi

# ---- Packages -----------------------------------------------------------------------

export DEBIAN_FRONTEND=noninteractive
export NEEDRESTART_SUSPEND=1  # no "restart services?" report during apt runs
say "Installing PostgreSQL and Caddy"
apt-get update -qq
# acl: lets the web server read each site's files without opening them to other sites.
apt-get install -y -qq ca-certificates curl gnupg openssl acl postgresql >/dev/null
# add-apt-repository is used to add the PHP package repository on Ubuntu.
[ "$ID" = ubuntu ] && apt-get install -y -qq software-properties-common >/dev/null
if ! command -v caddy >/dev/null; then
  # Caddy's official package repository (https://caddyserver.com/docs/install#debian-ubuntu-raspbian).
  install -d -m 0755 /usr/share/keyrings
  curl -fsSL https://dl.cloudsmith.io/public/caddy/stable/gpg.key | gpg --dearmor --yes -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
  curl -fsSL https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt -o /etc/apt/sources.list.d/caddy-stable.list
  apt-get update -qq
  apt-get install -y -qq caddy >/dev/null
fi

# ---- User, directories, database ----------------------------------------------------

say "Creating the ekpanel user and database"
id ekpanel >/dev/null 2>&1 || useradd --system --home-dir /var/lib/ekpanel --shell /usr/sbin/nologin ekpanel
install -d -m 0700 -o ekpanel -g ekpanel /var/lib/ekpanel
install -d -m 0755 /etc/ekpanel

# The panel connects over the local socket with peer authentication: no password exists.
if ! runuser -u postgres -- psql -tAc "SELECT 1 FROM pg_roles WHERE rolname = 'ekpanel'" | grep -q 1; then
  runuser -u postgres -- createuser ekpanel
fi
if ! runuser -u postgres -- psql -tAc "SELECT 1 FROM pg_database WHERE datname = 'ekpanel'" | grep -q 1; then
  runuser -u postgres -- createdb -O ekpanel ekpanel
fi

# ---- Binaries and services ----------------------------------------------------------

say "Installing ekPanel $("$LOCAL_DIR/ekpanel" version)"
install -m 0755 "$LOCAL_DIR/ekpanel" /usr/local/bin/ekpanel
install -m 0755 "$LOCAL_DIR/ekpanel-agent" /usr/local/bin/ekpanel-agent

cat > /etc/ekpanel/panel.env <<'EOF'
EKPANEL_LISTEN=127.0.0.1:8080
EKPANEL_AGENT_LISTEN=:7443
EKPANEL_DATABASE_URL=postgres:///ekpanel?host=/var/run/postgresql
EKPANEL_DATA_DIR=/var/lib/ekpanel
EOF

cat > /etc/systemd/system/ekpanel.service <<'EOF'
[Unit]
Description=ekPanel control panel
After=network-online.target postgresql.service
Wants=network-online.target
Requires=postgresql.service

[Service]
User=ekpanel
Group=ekpanel
EnvironmentFile=/etc/ekpanel/panel.env
ExecStart=/usr/local/bin/ekpanel
Restart=on-failure
RestartSec=3

# The panel itself needs no privileges: the agent does the work on the server.
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes
ReadWritePaths=/var/lib/ekpanel
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6
RestrictNamespaces=yes
LockPersonality=yes
MemoryDenyWriteExecute=yes
SystemCallArchitectures=native
CapabilityBoundingSet=

[Install]
WantedBy=multi-user.target
EOF

cat > /etc/systemd/system/ekpanel-agent.service <<'EOF'
[Unit]
Description=ekPanel agent
After=network-online.target
Wants=network-online.target

[Service]
# The agent manages this server (websites, services, files), so it runs as root.
ExecStart=/usr/local/bin/ekpanel-agent run --config /etc/ekpanel/agent.json
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable --now postgresql >/dev/null 2>&1
systemctl enable ekpanel >/dev/null 2>&1
systemctl restart ekpanel

say "Waiting for the panel to start"
for _ in $(seq 1 30); do
  curl -fsS http://127.0.0.1:8080/api/v1/health >/dev/null 2>&1 && break
  sleep 1
done
curl -fsS http://127.0.0.1:8080/api/v1/health >/dev/null || fail "the panel did not start; see: journalctl -u ekpanel"

# ---- Connect this server's agent ----------------------------------------------------

# Enrol when there is no credential yet, or only one from an agent before mutual TLS.
if ! grep -qs '"ca_pem"' /etc/ekpanel/agent.json; then
  say "Connecting this server to the panel"
  read -r token pin < <(runuser -u ekpanel -- bash -c \
    'set -a; . /etc/ekpanel/panel.env; exec /usr/local/bin/ekpanel add-server --name "$1"' _ "$(hostname)")
  [ -n "${token:-}" ] && [ -n "${pin:-}" ] || fail "could not create an enrolment token for this server."
  /usr/local/bin/ekpanel-agent enroll --panel 127.0.0.1:7443 --token "$token" --ca-pin "$pin" \
    --config /etc/ekpanel/agent.json >/dev/null
fi
systemctl enable ekpanel-agent >/dev/null 2>&1
systemctl restart ekpanel-agent

# ---- Web server ---------------------------------------------------------------------

say "Configuring Caddy"
ip="$(hostname -I 2>/dev/null | awk '{print $1}')"
# The agent writes one file per website here; the Caddyfile imports them.
install -d -m 0755 /etc/ekpanel/caddy/sites
email_line=""
[ -n "$EMAIL" ] && email_line=$'\n\temail '"$EMAIL"
if [ -n "$DOMAIN" ]; then
  # Caddy obtains and renews a Let's Encrypt certificate for the domain by itself.
  cat > /etc/caddy/Caddyfile <<EOF
# Managed by the ekPanel installer. Websites are in /etc/ekpanel/caddy/sites.
{$email_line
}

$DOMAIN {
	encode zstd gzip
	reverse_proxy 127.0.0.1:8080
}

import /etc/ekpanel/caddy/sites/*.caddy
EOF
else
  # No domain: a self-signed certificate for this server's IP and host name. Browsers send
  # no server name when opening an IP address, so default_sni tells Caddy which one to use.
  install -d -m 0750 -o root -g caddy /etc/ekpanel/tls
  if [ ! -s /etc/ekpanel/tls/panel.crt ]; then
    openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 825 \
      -subj "/CN=$(hostname)" \
      -addext "subjectAltName=DNS:$(hostname),DNS:localhost,IP:127.0.0.1${ip:+,IP:$ip}" \
      -keyout /etc/ekpanel/tls/panel.key -out /etc/ekpanel/tls/panel.crt 2>/dev/null
    chgrp caddy /etc/ekpanel/tls/panel.key /etc/ekpanel/tls/panel.crt
    chmod 0640 /etc/ekpanel/tls/panel.key
  fi
  cat > /etc/caddy/Caddyfile <<EOF
# Managed by the ekPanel installer. Websites are in /etc/ekpanel/caddy/sites.
{
	default_sni ${ip:-localhost}$email_line
}

# The panel answers on the server's IP address; websites answer on their own domains.
:443 {
	tls /etc/ekpanel/tls/panel.crt /etc/ekpanel/tls/panel.key
	encode zstd gzip
	reverse_proxy 127.0.0.1:8080
}

import /etc/ekpanel/caddy/sites/*.caddy
EOF
fi
caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile >/dev/null 2>&1 \
  || fail "the generated Caddy configuration is invalid; see /etc/caddy/Caddyfile"
systemctl enable caddy >/dev/null 2>&1
systemctl reload-or-restart caddy

if command -v ufw >/dev/null && ufw status | grep -q "Status: active"; then
  ufw allow 80/tcp >/dev/null
  ufw allow 443/tcp >/dev/null
  ufw allow 7443/tcp >/dev/null  # agents on other servers
fi

# ---- Done ---------------------------------------------------------------------------

if [ -n "$DOMAIN" ]; then
  url="https://$DOMAIN"
else
  url="https://${ip:-<server-ip>}"
fi

echo
say "ekPanel is installed."
echo "    Open:  $url"
if [ -s /var/lib/ekpanel/setup-token ]; then
  echo "    Setup token (needed once, to create the administrator):"
  echo "        $(cat /var/lib/ekpanel/setup-token)"
fi
[ -z "$DOMAIN" ] && echo "    The certificate is self-signed, so your browser will warn you once. Use --domain for a trusted one."
echo "    Agents on other servers connect to TCP port 7443; allow it in any cloud firewall."
echo "    Logs:  journalctl -u ekpanel -u ekpanel-agent -f"
