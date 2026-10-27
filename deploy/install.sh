#!/usr/bin/env bash
# xcod installer for Linux (Ubuntu/Debian; Rocky/Alma with EPEL). Installs xcod as a system
# service (systemd), without Docker. Running it again upgrades the binary and keeps the data.
#
# Primary server (downloads the latest release from GitHub), as root:
#   curl -fsSL https://github.com/hubxcod/xcod-serve/releases/latest/download/install.sh | bash
#   curl -fsSL .../install.sh | bash -s -- --domain video.example.com
#
# Without access to GitHub: copy xcod-linux-amd64 next to the installer and run
#   bash install.sh
#
# Repeater (the ready-made command is on the primary's panel, under Network):
#   curl -fsSL http://PRIMARY:8090/cluster/install.sh | bash -s -- --origin http://PRIMARY:8090 --token ID.SECRET
#
# Options:
#   --binary FILE      xcod binary to install
#   --url URL          download the binary from this address
#   --version vX.Y.Z   install this GitHub release (default: the latest)
#   --origin URL       install as a repeater of this primary (downloads its binary)
#   --token T          repeater token, issued by the primary
#   --domain D         domain with automatic HTTPS (its DNS must already point to this server)
#   --capacity N       outbound bandwidth in Mbps used to split the audience (default 800)
#   --port P           xcod HTTP port (default 8090)
#   --no-https         do not use ports 80/443 (no automatic certificate)
#   --uninstall        remove xcod (data stays in /var/lib/xcod; add --purge to delete it)
set -euo pipefail

BIN_DIR=/opt/xcod
DATA_DIR=/var/lib/xcod
ETC_DIR=/etc/xcod
UNIT=/etc/systemd/system/xcod.service
UPD_DIR=$DATA_DIR/update

REPO="${XCOD_REPO:-hubxcod/xcod-serve}"
BINARY="" URL="${XCOD_DOWNLOAD:-}" GH_VERSION="" ORIGIN="" TOKEN="" DOMAIN="" CAPACITY="" PORT=8090
NO_HTTPS=0 UNINSTALL=0 PURGE=0

green() { printf '\033[1;32m%s\033[0m\n' "$*"; }
info() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33mWARNING:\033[0m %s\n' "$*" >&2; }
fail() { printf '\033[1;31mERROR:\033[0m %s\n' "$*" >&2; exit 1; }

while [ $# -gt 0 ]; do
  case "$1" in
    --binary) BINARY="$2"; shift 2 ;;
    --url) URL="$2"; shift 2 ;;
    --version) GH_VERSION="$2"; shift 2 ;;
    --origin) ORIGIN="${2%/}"; shift 2 ;;
    --token) TOKEN="$2"; shift 2 ;;
    --domain) DOMAIN="$2"; shift 2 ;;
    --capacity) CAPACITY="$2"; shift 2 ;;
    --port) PORT="$2"; shift 2 ;;
    --no-https) NO_HTTPS=1; shift ;;
    --uninstall) UNINSTALL=1; shift ;;
    --purge) PURGE=1; shift ;;
    -h|--help) sed -n '2,30p' "${BASH_SOURCE[0]:-/dev/null}" 2>/dev/null || true; exit 0 ;;
    *) fail "unknown option: $1 (use --help)" ;;
  esac
done

[ "$(id -u)" -eq 0 ] || fail "run as root (use 'sudo -i' or 'sudo bash')"
[ "$(uname -s)" = Linux ] || fail "the installer only supports Linux"
command -v systemctl >/dev/null || fail "systemd not found"

# ---------- uninstall ----------
if [ "$UNINSTALL" = 1 ]; then
  systemctl disable --now xcod xcod-update.path 2>/dev/null || true
  rm -f "$UNIT" /etc/systemd/system/xcod-update.service /etc/systemd/system/xcod-update.path /etc/sysctl.d/90-xcod.conf \
    /etc/systemd/journald@xcod.conf
  systemctl daemon-reload
  rm -rf "$BIN_DIR"
  if [ "$PURGE" = 1 ]; then rm -rf "$DATA_DIR" "$ETC_DIR"; info "data deleted"; fi
  green "xcod removed."
  exit 0
fi

case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) fail "unsupported architecture: $(uname -m)" ;;
esac
case "$PORT" in ''|*[!0-9]*) fail "invalid port: $PORT" ;; esac
if [ -n "$ORIGIN" ] || [ -n "$TOKEN" ]; then
  [ -n "$ORIGIN" ] && [ -n "$TOKEN" ] || fail "a repeater needs both --origin and --token"
  case "$TOKEN" in *.*) ;; *) fail "invalid token: copy the command from the primary's Network page" ;; esac
fi

UPGRADING=0
[ -x "$BIN_DIR/xcod" ] && UPGRADING=1

# ---------- dependencies ----------
info "installing dependencies"
PACKAGES="curl ca-certificates openssl"
[ -z "$ORIGIN" ] && PACKAGES="$PACKAGES ffmpeg" # repeaters do not transcode
if command -v apt-get >/dev/null; then
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -qq
  apt-get install -y -qq $PACKAGES >/dev/null
elif command -v dnf >/dev/null; then
  dnf install -y -q $PACKAGES || warn "could not install everything ($PACKAGES). For ffmpeg, enable EPEL and RPM Fusion."
else
  warn "unknown package manager: install $PACKAGES manually"
fi
command -v ffmpeg >/dev/null || [ -n "$ORIGIN" ] || warn "ffmpeg missing: only the Original quality will work"
# repeaters sign their requests with the time: a correct clock is required
timedatectl set-ntp true 2>/dev/null || true

# ---------- binary ----------
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
NEW="$TMP/xcod"
HERE=""
[ -n "${BASH_SOURCE[0]:-}" ] && [ -f "${BASH_SOURCE[0]}" ] && HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

if [ -n "$BINARY" ]; then
  cp "$BINARY" "$NEW"
elif [ -n "$HERE" ] && [ -f "$HERE/xcod-linux-$ARCH" ]; then
  cp "$HERE/xcod-linux-$ARCH" "$NEW"
elif [ -n "$HERE" ] && [ -f "$HERE/xcod" ] && [ ! -d "$HERE/xcod" ]; then
  cp "$HERE/xcod" "$NEW"
elif [ -n "$ORIGIN" ]; then
  info "downloading xcod from the primary"
  ID=${TOKEN%%.*} SECRET=${TOKEN#*.} TS=$(date +%s)
  SIG=$(printf '%s' "dl|$ID|$TS" | openssl dgst -sha256 -hmac "$SECRET" | awk '{print $NF}')
  CODE=$(curl -sSL -H "Authorization: XCOD $ID:$TS:$SIG" "$ORIGIN/cluster/xcod/$ARCH" -o "$NEW" -w '%{http_code}') \
    || fail "cannot reach the primary at $ORIGIN"
  [ "$CODE" = 200 ] || fail "the primary refused ($CODE): $(head -c 300 "$NEW")"
elif [ -n "$URL" ]; then
  info "downloading $URL"
  CODE=$(curl -sSL "$URL" -o "$NEW" -w '%{http_code}') || fail "cannot reach $URL"
  [ "$CODE" = 200 ] || fail "could not download $URL ($CODE)"
else
  # GitHub Releases, verified against the SHA-256 published with them
  if [ -n "$GH_VERSION" ]; then BASE="https://github.com/$REPO/releases/download/$GH_VERSION"
  else BASE="https://github.com/$REPO/releases/latest/download"; fi
  info "downloading xcod from github.com/$REPO (${GH_VERSION:-latest release})"
  CODE=$(curl -sSL "$BASE/xcod-linux-$ARCH" -o "$NEW" -w '%{http_code}') || fail "cannot reach GitHub"
  [ "$CODE" = 200 ] || fail "release not found on GitHub ($CODE): publish a release or use --binary"
  curl -fsSL "$BASE/SHA256SUMS" -o "$TMP/SHA256SUMS" || fail "SHA256SUMS not found in the release"
  EXPECTED=$(awk -v f="xcod-linux-$ARCH" '$2 == f || $2 == "*"f {print $1}' "$TMP/SHA256SUMS")
  [ -n "$EXPECTED" ] && [ "$(sha256sum "$NEW" | awk '{print $1}')" = "$EXPECTED" ] \
    || fail "the downloaded file does not match SHA256SUMS (corrupted or tampered download)"
fi
chmod 755 "$NEW"
VERSION=$("$NEW" -version 2>/dev/null) || fail "the binary does not run on this server ($ARCH)"

# ---------- user, directories, system ----------
id xcod >/dev/null 2>&1 || useradd --system --home-dir "$DATA_DIR" --shell /usr/sbin/nologin xcod
install -d -m 755 "$BIN_DIR"
install -d -m 700 -o xcod -g xcod "$DATA_DIR"
install -d -m 750 -o root -g xcod "$ETC_DIR"
install -d -m 700 -o xcod -g xcod "$UPD_DIR"

[ "$UPGRADING" = 1 ] && systemctl stop xcod 2>/dev/null || true
install -m 755 "$NEW" "$BIN_DIR/xcod"

port_busy() { ss -Hltn "( sport = :$1 )" 2>/dev/null | grep -q .; }
HTTPS_OK=0
if [ "$NO_HTTPS" = 0 ]; then
  if port_busy 80 || port_busy 443; then
    warn "ports 80/443 are already used by another program: no automatic certificate (use that proxy for the domain)"
  else
    HTTPS_OK=1
  fi
fi

if [ ! -f "$ETC_DIR/xcod.env" ] || [ "$UPGRADING" = 0 ]; then
  {
    echo "# xcod service environment (everything else is in the panel)"
    echo "XCOD_DATA=$DATA_DIR"
    echo "XCOD_LISTEN=:$PORT"
    if [ "$HTTPS_OK" = 1 ]; then echo "XCOD_HTTP_LISTEN=:80"; echo "XCOD_HTTPS_LISTEN=:443"; fi
  } > "$ETC_DIR/xcod.env"
  chmod 640 "$ETC_DIR/xcod.env"; chown root:xcod "$ETC_DIR/xcod.env"
fi
grep -q '^XCOD_UPDATE_DIR=' "$ETC_DIR/xcod.env" || echo "XCOD_UPDATE_DIR=$UPD_DIR" >> "$ETC_DIR/xcod.env"
PORT=$(sed -n 's/^XCOD_LISTEN=.*://p' "$ETC_DIR/xcod.env")
grep -q '^XCOD_HTTPS_LISTEN=' "$ETC_DIR/xcod.env" && HTTPS_OK=1

# first-run values: xcod applies them and the installer deletes the file right after
PASSWORD="" NEW_ADDRESS=0
BOOT="$ETC_DIR/bootstrap.env"
: > "$BOOT"; chmod 640 "$BOOT"; chown root:xcod "$BOOT"
if [ ! -f "$DATA_DIR/admin.json" ]; then
  PASSWORD=$(openssl rand -base64 30 | tr -dc 'A-Za-z0-9' | cut -c1-20)
  { echo "ADMIN_USER=admin"; echo "ADMIN_PASSWORD=$PASSWORD"; } >> "$BOOT"
fi
# secret panel address (/<token>/admin), created together with the login; kept on upgrades
PANEL_TOKEN=$(sed -n 's/.*"panel_token": *"\([A-Za-z0-9_-]*\)".*/\1/p' "$DATA_DIR/config.json" 2>/dev/null || true)
if [ -z "$PANEL_TOKEN" ]; then
  PANEL_TOKEN=$(openssl rand -base64 48 | tr -dc 'A-Za-z0-9' | cut -c1-24)
  echo "XCOD_PANEL_TOKEN=$PANEL_TOKEN" >> "$BOOT"
  NEW_ADDRESS=1
fi
[ -n "$ORIGIN" ] && { echo "XCOD_ORIGIN=$ORIGIN"; echo "XCOD_ORIGIN_TOKEN=$TOKEN"; } >> "$BOOT"
[ -n "$DOMAIN" ] && echo "XCOD_DOMAIN=$DOMAIN" >> "$BOOT"
[ -n "$CAPACITY" ] && echo "XCOD_CAPACITY_MBPS=$CAPACITY" >> "$BOOT"

cat > "$UNIT" <<EOF
[Unit]
Description=xcod (restreamer)
After=network-online.target
Wants=network-online.target

[Service]
User=xcod
Group=xcod
WorkingDirectory=$DATA_DIR
EnvironmentFile=$ETC_DIR/xcod.env
EnvironmentFile=-$BOOT
ExecStart=$BIN_DIR/xcod
Restart=always
RestartSec=2
LimitNOFILE=65535
# own log journal: kept for 90 days / 1 GB, then trimmed automatically (journald@xcod.conf)
LogNamespace=xcod
# only what is needed: bind ports 80/443 without being root
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
NoNewPrivileges=yes
ProtectSystem=strict
ReadWritePaths=$DATA_DIR
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
ProtectClock=yes
ProtectHostname=yes
RestrictSUIDSGID=yes
RestrictNamespaces=yes
RestrictRealtime=yes
LockPersonality=yes
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
SystemCallArchitectures=native
UMask=0077

[Install]
WantedBy=multi-user.target
EOF

# ---------- updates from the panel ----------
# xcod leaves the verified new version in $UPD_DIR and creates "ready"; this installer (root)
# swaps the binary, restarts and rolls back to the previous version if the new one does not answer.
rm -f "$BIN_DIR/atualizar.sh" # name used by older versions
cat > "$BIN_DIR/update.sh" <<'EOF'
#!/usr/bin/env bash
set -u
D=/var/lib/xcod/update BIN=/opt/xcod/xcod NEW=/var/lib/xcod/update/xcod.new
done_with() { echo "$1" > "$D/result"; chown xcod:xcod "$D/result"; rm -f "$D/ready" "$NEW"; exit 0; }
[ -f "$D/ready" ] || exit 0
# the file comes from the xcod user: only a regular file it owns (no links to system files)
[ -f "$NEW" ] && [ ! -L "$NEW" ] && [ "$(stat -c %U "$NEW")" = xcod ] || done_with "error: invalid update file"
# check the version as the xcod user (never run the file as root)
VER=$(setpriv --reuid=xcod --regid=xcod --clear-groups "$NEW" -version 2>/dev/null) || done_with "error: the file does not run on this server"
BEFORE=$("$BIN" -version 2>/dev/null || echo "?")
cp -p "$BIN" "$BIN.previous"
install -m 755 -o root -g root "$NEW" "$BIN.tmp" && mv -f "$BIN.tmp" "$BIN" || done_with "error: could not install"
systemctl restart xcod
PORT=$(sed -n 's/^XCOD_LISTEN=.*://p' /etc/xcod/xcod.env)
for _ in $(seq 1 30); do
  sleep 1
  curl -fsS -o /dev/null "http://127.0.0.1:${PORT:-8090}/healthz" 2>/dev/null && done_with "ok: $BEFORE -> $VER"
done
# the new version did not come up: roll back
mv -f "$BIN.previous" "$BIN"
systemctl restart xcod
done_with "error: $VER did not answer; rolled back to $BEFORE"
EOF
chmod 755 "$BIN_DIR/update.sh"

cat > /etc/systemd/system/xcod-update.service <<EOF
[Unit]
Description=Install the xcod update staged by the panel

[Service]
Type=oneshot
ExecStart=$BIN_DIR/update.sh
EOF
cat > /etc/systemd/system/xcod-update.path <<EOF
[Unit]
Description=Wait for an xcod update

[Path]
PathExists=$UPD_DIR/ready

[Install]
WantedBy=multi-user.target
EOF

# logs: a separate journal for xcod, 90 days and at most 1 GB; older entries are deleted automatically
cat > /etc/systemd/journald@xcod.conf <<'EOF'
[Journal]
Storage=persistent
MaxRetentionSec=90day
SystemMaxUse=1G
EOF

# network tuned for many viewers
cat > /etc/sysctl.d/90-xcod.conf <<'EOF'
net.core.somaxconn = 4096
net.core.netdev_max_backlog = 16384
net.core.rmem_max = 16777216
net.core.wmem_max = 16777216
net.ipv4.tcp_wmem = 4096 65536 16777216
net.ipv4.tcp_rmem = 4096 87380 16777216
net.core.default_qdisc = fq
net.ipv4.tcp_congestion_control = bbr
net.ipv4.ip_local_port_range = 10240 65535
EOF
sysctl --system >/dev/null 2>&1 || warn "could not apply the network tuning (sysctl)"

# firewall: only opens xcod's ports if one is already active (never turns a firewall on)
PORTS="$PORT"
[ "$HTTPS_OK" = 1 ] && PORTS="$PORTS 80 443"
if command -v ufw >/dev/null && ufw status 2>/dev/null | grep -q "Status: active"; then
  for p in $PORTS; do ufw allow "$p/tcp" >/dev/null; done
  info "firewall (ufw): ports $PORTS opened"
elif command -v firewall-cmd >/dev/null && firewall-cmd --state >/dev/null 2>&1; then
  for p in $PORTS; do firewall-cmd --permanent --add-port="$p/tcp" >/dev/null; done
  firewall-cmd --reload >/dev/null
  info "firewall (firewalld): ports $PORTS opened"
fi

info "starting the service"
systemctl daemon-reload
systemctl enable xcod >/dev/null 2>&1
systemctl enable --now xcod-update.path >/dev/null 2>&1 || warn "panel updates unavailable (xcod-update.path)"
systemctl restart xcod

for _ in $(seq 1 30); do
  curl -fsS -o /dev/null "http://127.0.0.1:$PORT/healthz" 2>/dev/null && break
  sleep 1
done
if ! curl -fsS -o /dev/null "http://127.0.0.1:$PORT/healthz" 2>/dev/null; then
  journalctl --namespace=xcod -u xcod -n 40 --no-pager >&2 || true
  fail "xcod did not answer: see the messages above"
fi
rm -f "$BOOT" # the initial password is not stored anywhere

IP=$(curl -4fsS --max-time 5 https://api.ipify.org 2>/dev/null || hostname -I | awk '{print $1}')
echo
green "============================================================"
if [ "$UPGRADING" = 1 ]; then green "  xcod upgraded ($VERSION)"; else green "  xcod installed ($VERSION)"; fi
green "============================================================"
echo "  Panel:     http://$IP:$PORT/$PANEL_TOKEN/admin"
[ -n "$DOMAIN" ] && [ "$HTTPS_OK" = 1 ] && echo "             https://$DOMAIN/$PANEL_TOKEN/admin  (certificate in ~1 min, if the DNS already points to $IP)"
if [ -n "$PASSWORD" ]; then
  echo "  User:      admin"
  echo "  Password:  $PASSWORD"
  echo
  warn "save the panel address and the password: the password will not be shown again (both can be changed under Security)"
elif [ "$NEW_ADDRESS" = 1 ]; then
  echo
  warn "the panel now has a secret address (above): /admin alone no longer opens it"
fi
[ -n "$ORIGIN" ] && echo "  Mode:      repeater of $ORIGIN"
echo
# the hosting provider's firewall (cloud panel) is out of the installer's reach
warn "if the panel does not open, allow TCP ports $(echo $PORTS | sed 's/ /, /g') in your hosting provider's firewall"
echo
echo "  Commands:  systemctl status xcod · journalctl --namespace=xcod -f   (logs kept 90 days)"
echo "             update: panel > Server > Updates (or run the installer again)"
echo "             uninstall: bash install.sh --uninstall"
echo
