#!/bin/sh
# Installs stackwire on a Linux server and starts its web interface.
#
#   curl -fsSL https://raw.githubusercontent.com/Paul198925/stackwire/main/install.sh | sudo sh
#
# Running it again updates stackwire and keeps the stack as it is.
# Settings, as environment variables:
#   STACKWIRE_DIR      stack folder (default /opt/stackwire)
#   STACKWIRE_VERSION  release to install, e.g. v0.1.0 (default: the latest)
#   STACKWIRE_PORT     port of the web interface (default 8900)
#   STACKWIRE_BASE_URL where to download from instead of the GitHub release
set -eu

REPO="Paul198925/stackwire"
DIR="${STACKWIRE_DIR:-/opt/stackwire}"
VERSION="${STACKWIRE_VERSION:-latest}"
PORT="${STACKWIRE_PORT:-8900}"
BIN="/usr/local/bin/stackwire"
UNIT="/etc/systemd/system/stackwire.service"

say() { printf '\033[1m==>\033[0m %s\n' "$*"; }
die() { printf '\033[31merror:\033[0m %s\n' "$*" >&2; exit 1; }

[ "$(uname -s)" = "Linux" ] || die "this installer is for Linux; on Windows and macOS see the README"
[ "$(id -u)" -eq 0 ] || die "run it as root, e.g. with: curl -fsSL https://raw.githubusercontent.com/$REPO/main/install.sh | sudo sh"
command -v systemctl >/dev/null 2>&1 || die "systemd is needed to keep stackwire running"
command -v curl >/dev/null 2>&1 || die "curl is needed"

case "$(uname -m)" in
  x86_64 | amd64) ARCH=amd64 ;;
  aarch64 | arm64) ARCH=arm64 ;;
  *) die "unsupported processor: $(uname -m)" ;;
esac

# --- Docker ---
if ! command -v docker >/dev/null 2>&1; then
  say "Installing Docker"
  curl -fsSL https://get.docker.com | sh
fi
systemctl enable --now docker >/dev/null 2>&1 || true
docker compose version >/dev/null 2>&1 || die "Docker is installed but its compose plugin is missing; install docker-compose-plugin"

# --- stackwire ---
if [ -n "${STACKWIRE_BASE_URL:-}" ]; then
  BASE="$STACKWIRE_BASE_URL" # a mirror, or a local build for testing
elif [ "$VERSION" = "latest" ]; then
  BASE="https://github.com/$REPO/releases/latest/download"
else
  BASE="https://github.com/$REPO/releases/download/$VERSION"
fi
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
say "Downloading stackwire ($VERSION, linux-$ARCH)"
curl -fsSL -o "$TMP/stackwire" "$BASE/stackwire-linux-$ARCH" || die "download failed from $BASE"
if curl -fsSL -o "$TMP/checksums.txt" "$BASE/checksums.txt" 2>/dev/null && command -v sha256sum >/dev/null 2>&1; then
  # Lines are "<hash>  <file>", or "<hash> *<file>" when written in binary mode.
  want="$(awk -v f="stackwire-linux-$ARCH" '{ n = $2; sub(/^\*/, "", n) } n == f { print $1 }' "$TMP/checksums.txt")"
  have="$(sha256sum "$TMP/stackwire" | cut -d' ' -f1)"
  [ -n "$want" ] && [ "$want" = "$have" ] || die "checksum of the download does not match"
fi
chmod 755 "$TMP/stackwire"
"$TMP/stackwire" version >/dev/null || die "the downloaded file does not run on this machine"
install -m 755 "$TMP/stackwire" "$BIN"
say "Installed $("$BIN" version) to $BIN"

# --- service ---
mkdir -p "$DIR"
cat >"$UNIT" <<EOF
[Unit]
Description=stackwire web interface
After=docker.service network-online.target
Wants=network-online.target
Requires=docker.service

[Service]
ExecStart=$BIN serve --dir $DIR --addr 0.0.0.0:$PORT
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable stackwire >/dev/null 2>&1
systemctl restart stackwire

# The web interface runs on the host. The firewall must let the browser and,
# with a domain, the proxy container reach it.
if command -v ufw >/dev/null 2>&1 && ufw status 2>/dev/null | grep -q "Status: active"; then
  say "Opening port $PORT in ufw"
  ufw allow "$PORT/tcp" >/dev/null
fi

# --- where to go next ---
i=0
while [ ! -f "$DIR/stackwire.yaml" ] && [ $i -lt 30 ]; do sleep 1; i=$((i + 1)); done
systemctl is-active --quiet stackwire || die "stackwire did not start; see: journalctl -u stackwire"

# This machine's addresses, without Docker's own bridges.
ADDRS="$(ip -4 -o addr show scope global 2>/dev/null | awk '$2 !~ /^(docker|br-|veth)/ { split($4, a, "/"); print a[1] }' || true)"
[ -n "$ADDRS" ] || ADDRS="<this-server>"
echo
if [ -f "$DIR/.setup-code" ]; then
  CODE="$(cat "$DIR/.setup-code")"
  say "stackwire is running. Open this link in your browser to set it up:"
  for a in $ADDRS; do echo "    http://$a:$PORT/?setup=$CODE"; done
  echo
  echo "    The link works once and only until the setup is done."
  echo "    The connection is not encrypted yet: set it up from a network you trust,"
  echo "    or choose a domain in the setup to get HTTPS."
else
  say "stackwire is updated and running:"
  for a in $ADDRS; do echo "    http://$a:$PORT"; done
fi
echo
echo "    Stack folder: $DIR    Logs: journalctl -u stackwire -f"
