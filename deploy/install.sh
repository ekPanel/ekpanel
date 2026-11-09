#!/usr/bin/env bash
# ekPanel installer for a fresh Ubuntu 24.04+ or Debian 12+ server.
#
# The panel (and this server as its first managed server):
#   curl -fsSL https://github.com/ekPanel/ekpanel/releases/latest/download/install.sh | sudo bash -s -- [options]
#
# Another server, managed by an existing panel (the panel shows this command with the values filled in):
#   ... | sudo bash -s -- --agent --panel panel.example.com:7443 --token TOKEN --ca-pin sha256:...
#
# Options:
#   --domain NAME      a DNS name pointing at this server. Caddy gets a free Let's Encrypt
#                      certificate for it. Without it the panel uses a self-signed certificate
#                      on the server's IP address, and browsers show a warning.
#   --email ADDRESS    contact address for Let's Encrypt expiry notices (optional).
#   --agent            set up this server for an existing panel, with --panel, --token and
#                      --ca-pin from the panel's "Add server" dialog.
#   --version vX.Y.Z   install this release (default: the release this script came with, or the latest).
#   --local-dir DIR    use the ekpanel and ekpanel-agent binaries in DIR instead of downloading
#                      a release (for development: `make panel agent`).
#   --release-url URL  download releases from a mirror instead of GitHub.
#   --release-key FILE trust this public key instead of the ekPanel release key (testing).
#
# Downloads are checked: SHA256SUMS must carry a valid signature from the ekPanel release
# key below, and the archive must match it. Running the installer again upgrades the
# binaries and keeps all data.

set -euo pipefail

# Written in by `make release`; empty means "the latest release".
EKPANEL_VERSION=""
RELEASE_URL="https://github.com/ekPanel/ekpanel/releases"

# The ekPanel release signing key (internal/release/release.pub in the repository).
RELEASE_PUBLIC_KEY='-----BEGIN PUBLIC KEY-----
MCowBQYDK2VwAyEA1xctdfx1S8AFsa6XLLvq7UddkQiV9eaR/fpouoABI1E=
-----END PUBLIC KEY-----'

MODE=panel
LOCAL_DIR=""
DOMAIN=""
EMAIL=""
PANEL_ADDR=""
TOKEN=""
CA_PIN=""
RELEASE_KEY_FILE=""

usage() { sed -n '2,25p' "${BASH_SOURCE[0]}" 2>/dev/null | sed 's/^# \{0,1\}//'; exit "${1:-0}"; }

while [ $# -gt 0 ]; do
  case "$1" in
    --domain) DOMAIN="${2:-}"; shift 2 ;;
    --email) EMAIL="${2:-}"; shift 2 ;;
    --agent) MODE=agent; shift ;;
    --panel) PANEL_ADDR="${2:-}"; shift 2 ;;
    --token) TOKEN="${2:-}"; shift 2 ;;
    --ca-pin) CA_PIN="${2:-}"; shift 2 ;;
    --version) EKPANEL_VERSION="${2:-}"; shift 2 ;;
    --local-dir) LOCAL_DIR="${2:-}"; shift 2 ;;
    --release-url) RELEASE_URL="${2:-}"; shift 2 ;;
    --release-key) RELEASE_KEY_FILE="${2:-}"; shift 2 ;;
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
  x86_64) ARCH=amd64 ;;
  aarch64) ARCH=arm64 ;;
  *) fail "CPU architecture $(uname -m) is not supported yet (amd64 and arm64 are)." ;;
esac
if [ "$MODE" = agent ]; then
  [ -n "$PANEL_ADDR" ] && [ -n "$TOKEN" ] && [ -n "$CA_PIN" ] \
    || grep -qs '"ca_pem"' /etc/ekpanel/agent.json \
    || fail "--agent needs --panel, --token and --ca-pin; copy the full command from the panel's \"Add server\" dialog."
  [ -z "$DOMAIN" ] || fail "--domain is for the panel's own server; websites get their domains in the panel."
  [ -e /etc/ekpanel/panel.env ] && fail "this server runs the panel; its agent is already set up."
fi
if [ -n "$DOMAIN" ] && ! [[ "$DOMAIN" =~ ^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$ ]]; then
  fail "--domain must be a plain host name such as panel.example.com."
fi
if [ -n "$EKPANEL_VERSION" ] && ! [[ "$EKPANEL_VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$ ]]; then
  fail "--version must look like v1.2.3."
fi

export DEBIAN_FRONTEND=noninteractive
export NEEDRESTART_SUSPEND=1  # no "restart services?" report during apt runs

say "Installing the packages ekPanel needs"
apt-get update -qq
# acl: lets the web server read each site's files without opening them to other sites.
apt-get install -y -qq ca-certificates curl gnupg openssl acl >/dev/null
[ "$MODE" = panel ] && apt-get install -y -qq postgresql >/dev/null
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

# ---- The binaries -------------------------------------------------------------------

WORK="$(mktemp -d)"
trap 'rm -rf -- "$WORK"' EXIT

if [ -z "$LOCAL_DIR" ]; then
  RELEASE_URL="${RELEASE_URL%/}"
  if [ -z "$EKPANEL_VERSION" ]; then
    EKPANEL_VERSION="$(curl -fsSL "$RELEASE_URL/latest/download/release.json" \
      | sed -n 's/.*"version" *: *"\(v[0-9][^"]*\)".*/\1/p')"
    [ -n "$EKPANEL_VERSION" ] || fail "could not find the latest release at $RELEASE_URL."
  fi
  say "Downloading ekPanel $EKPANEL_VERSION"
  archive="ekpanel-$EKPANEL_VERSION-linux-$ARCH.tar.gz"
  base="$RELEASE_URL/download/$EKPANEL_VERSION"
  for f in SHA256SUMS SHA256SUMS.sig "$archive"; do
    curl -fsSL --retry 3 -o "$WORK/$f" "$base/$f" || fail "could not download $base/$f"
  done
  if [ -n "$RELEASE_KEY_FILE" ]; then
    cp "$RELEASE_KEY_FILE" "$WORK/release.pub"
  else
    printf '%s\n' "$RELEASE_PUBLIC_KEY" > "$WORK/release.pub"
  fi
  openssl pkeyutl -verify -pubin -inkey "$WORK/release.pub" -rawin \
    -in "$WORK/SHA256SUMS" -sigfile "$WORK/SHA256SUMS.sig" >/dev/null 2>&1 \
    || fail "SHA256SUMS is not signed by the ekPanel release key; not installing."
  (cd "$WORK" && grep -E "^[0-9a-f]{64} [ *]$archive\$" SHA256SUMS | sha256sum -c --quiet --strict -) \
    || fail "$archive does not match its signed checksum; not installing."
  tar -xzf "$WORK/$archive" -C "$WORK"
  LOCAL_DIR="$WORK/ekpanel-$EKPANEL_VERSION-linux-$ARCH"
fi
binaries=(ekpanel-agent)
[ "$MODE" = panel ] && binaries+=(ekpanel)
for b in "${binaries[@]}"; do
  [ -x "$LOCAL_DIR/$b" ] || fail "$LOCAL_DIR/$b not found or not executable."
done

install -d -m 0755 /etc/ekpanel
# The agent writes one file per website here; the Caddyfile imports them.
install -d -m 0755 /etc/ekpanel/caddy/sites

# install_binary puts a binary in place, keeping the one it replaces as NAME.prev.
install_binary() {
  local name="$1"
  if [ -e "/usr/local/bin/$name" ] && ! cmp -s "$LOCAL_DIR/$name" "/usr/local/bin/$name"; then
    cp -p "/usr/local/bin/$name" "/usr/local/bin/$name.prev"
  fi
  install -m 0755 "$LOCAL_DIR/$name" "/usr/local/bin/$name.new"
  mv -f "/usr/local/bin/$name.new" "/usr/local/bin/$name"
}

write_agent_unit() {
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
}

# Behind Cloudflare's proxy, requests come from Cloudflare's addresses; trusting those (and
# only those) for CF-Connecting-IP gives logs, traffic and bans the visitor's real address.
# The ranges are Cloudflare's published ones (cloudflare.com/ips).
cloudflare_ranges="173.245.48.0/20 103.21.244.0/22 103.22.200.0/22 103.31.4.0/22 141.101.64.0/18 108.162.192.0/18 190.93.240.0/20 188.114.96.0/20 197.234.240.0/22 198.41.128.0/17 162.158.0.0/15 104.16.0.0/13 104.24.0.0/14 172.64.0.0/13 131.0.72.0/22 2400:cb00::/32 2606:4700::/32 2803:f800::/32 2405:b500::/32 2405:8100::/32 2a06:98c0::/29 2c0f:f248::/32"
servers_block=$'\n\tservers {\n\t\ttrusted_proxies static '"$cloudflare_ranges"$'\n\t\tclient_ip_headers CF-Connecting-IP\n\t}'
email_line=""
[ -n "$EMAIL" ] && email_line=$'\n\temail '"$EMAIL"

finish_caddy() {
  caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile >/dev/null 2>&1 \
    || fail "the generated Caddy configuration is invalid; see /etc/caddy/Caddyfile"
  systemctl enable caddy >/dev/null 2>&1
  systemctl reload-or-restart caddy
}

open_firewall() {
  if command -v ufw >/dev/null && ufw status | grep -q "Status: active"; then
    for port in "$@"; do ufw allow "$port/tcp" >/dev/null; done
  fi
}

# =====================================================================================
# A server for an existing panel
# =====================================================================================

if [ "$MODE" = agent ]; then
  say "Installing the ekPanel agent $("$LOCAL_DIR/ekpanel-agent" version)"
  install_binary ekpanel-agent
  write_agent_unit
  systemctl daemon-reload

  if ! grep -qs '"ca_pem"' /etc/ekpanel/agent.json; then
    say "Connecting this server to the panel at $PANEL_ADDR"
    /usr/local/bin/ekpanel-agent enroll --panel "$PANEL_ADDR" --token "$TOKEN" --ca-pin "$CA_PIN" \
      --config /etc/ekpanel/agent.json >/dev/null \
      || fail "the panel did not accept this server. The token works once and expires; create a new one in the panel."
  fi
  systemctl enable ekpanel-agent >/dev/null 2>&1
  systemctl restart ekpanel-agent

  say "Configuring Caddy for this server's websites"
  # Keep a Caddyfile someone else wrote: ekPanel only needs the import line in it.
  if [ -s /etc/caddy/Caddyfile ] && ! grep -q "ekPanel" /etc/caddy/Caddyfile \
     && ! grep -q "^:80 *{" /etc/caddy/Caddyfile; then
    grep -q "^import /etc/ekpanel/caddy/sites/\*.caddy" /etc/caddy/Caddyfile \
      || printf '\nimport /etc/ekpanel/caddy/sites/*.caddy\n' >> /etc/caddy/Caddyfile
  else
    cat > /etc/caddy/Caddyfile <<EOF
# Managed by the ekPanel installer. Websites are in /etc/ekpanel/caddy/sites.
{$email_line$servers_block
}

import /etc/ekpanel/caddy/sites/*.caddy
EOF
  fi
  finish_caddy
  open_firewall 80 443

  echo
  say "This server is connected to the panel."
  echo "    It appears in the panel's server list within a few seconds."
  echo "    Logs:  journalctl -u ekpanel-agent -f"
  exit 0
fi

# =====================================================================================
# The panel
# =====================================================================================

# ---- User, directories, database ----------------------------------------------------

say "Creating the ekpanel user and database"
id ekpanel >/dev/null 2>&1 || useradd --system --home-dir /var/lib/ekpanel --shell /usr/sbin/nologin ekpanel
install -d -m 0700 -o ekpanel -g ekpanel /var/lib/ekpanel

# The panel connects over the local socket with peer authentication: no password exists.
if ! runuser -u postgres -- psql -tAc "SELECT 1 FROM pg_roles WHERE rolname = 'ekpanel'" | grep -q 1; then
  runuser -u postgres -- createuser ekpanel
fi
if ! runuser -u postgres -- psql -tAc "SELECT 1 FROM pg_database WHERE datname = 'ekpanel'" | grep -q 1; then
  runuser -u postgres -- createdb -O ekpanel ekpanel
fi
# Customer database accounts on this server must not even connect to the panel's database.
runuser -u postgres -- psql -q -c "REVOKE ALL ON DATABASE ekpanel FROM PUBLIC"

# ---- Binaries and services ----------------------------------------------------------

say "Installing ekPanel $("$LOCAL_DIR/ekpanel" version)"
install_binary ekpanel
install_binary ekpanel-agent

cat > /etc/ekpanel/panel.env <<EOF
EKPANEL_LISTEN=127.0.0.1:8080
EKPANEL_AGENT_LISTEN=:7443
EKPANEL_DATABASE_URL=postgres:///ekpanel?host=/var/run/postgresql
EKPANEL_DATA_DIR=/var/lib/ekpanel
EKPANEL_RELEASES_URL=$RELEASE_URL
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
write_agent_unit

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
if [ -n "$DOMAIN" ]; then
  # Caddy obtains and renews a Let's Encrypt certificate for the domain by itself.
  cat > /etc/caddy/Caddyfile <<EOF
# Managed by the ekPanel installer. Websites are in /etc/ekpanel/caddy/sites.
{$email_line$servers_block
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
  panel_hosts="https://127.0.0.1, https://localhost, https://$(hostname)"
  [ -n "$ip" ] && panel_hosts="https://$ip, $panel_hosts"
  cat > /etc/caddy/Caddyfile <<EOF
# Managed by the ekPanel installer. Websites are in /etc/ekpanel/caddy/sites.
{
	default_sni ${ip:-localhost}$email_line$servers_block
}

# The panel answers on the server's addresses; websites answer on their own domains. The
# addresses are named, not ":443", or Caddy would use this certificate for every website.
${panel_hosts} {
	tls /etc/ekpanel/tls/panel.crt /etc/ekpanel/tls/panel.key
	encode zstd gzip
	reverse_proxy 127.0.0.1:8080
}

import /etc/ekpanel/caddy/sites/*.caddy
EOF
fi
finish_caddy
open_firewall 80 443 7443  # 7443: agents on other servers

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
