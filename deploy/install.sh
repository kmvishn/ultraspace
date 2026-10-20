#!/bin/sh
# Install or update ultraspace from an unpacked release (or a checkout with bin/ultraspace).
#
#   sudo ./install.sh --lan     reachable on your network at http://<this-machine>:4600
#   sudo ./install.sh           on 127.0.0.1:4600, for a reverse proxy with HTTPS in front
#
# Running it again updates the binary and restarts the service; your config and data stay.
set -eu

die() { echo "install.sh: $*" >&2; exit 1; }
[ "$(id -u)" = 0 ] || die "run it as root: sudo $0 $*"
command -v systemctl >/dev/null || die "needs systemd"

here=$(cd "$(dirname "$0")" && pwd)
bin="$here/ultraspace"
[ -f "$bin" ] || bin="$here/../bin/ultraspace"
[ -f "$bin" ] || die "no ultraspace binary next to this script"
deploy="$here/deploy"
[ -d "$deploy" ] || deploy="$here"

mode=proxy
case "${1:-}" in
  --lan) mode=lan ;;
  "") ;;
  *) die "unknown option $1 (use --lan or nothing)" ;;
esac

"$bin" -version >/dev/null 2>&1 || die "this binary does not run here (wrong architecture? uname -m says $(uname -m))"

install -m 755 "$bin" /usr/local/bin/ultraspace
id ultraspace >/dev/null 2>&1 || useradd --system --home /var/lib/ultraspace --shell /usr/sbin/nologin ultraspace
mkdir -p /etc/ultraspace

if [ ! -f /etc/ultraspace/config.toml ]; then
  if [ "$mode" = lan ]; then
    sed -e 's#^listen = .*#listen = "0.0.0.0:4600"#' -e 's#^public_origin = .*#public_origin = ""#' \
      -e 's#^trust_proxy = .*#trust_proxy = false#' "$deploy/config.toml" > /etc/ultraspace/config.toml
  else
    sed -e 's#^public_origin = .*#public_origin = ""   \# set to your https address, e.g. "https://ultraspace.example.com"#' \
      "$deploy/config.toml" > /etc/ultraspace/config.toml
  fi
  echo "Wrote /etc/ultraspace/config.toml ($mode)."
fi

new_unit=0
if [ ! -f /etc/systemd/system/ultraspace.service ]; then
  install -m 644 "$deploy/ultraspace.service" /etc/systemd/system/ultraspace.service
  new_unit=1
fi
systemctl daemon-reload
if [ "$new_unit" = 1 ]; then
  systemctl enable --now ultraspace
else
  systemctl restart ultraspace
fi

i=0
until [ -f /var/lib/ultraspace/setup-token ] || [ -f /var/lib/ultraspace/ultraspace.db ] || [ $i -ge 20 ]; do sleep 0.5; i=$((i + 1)); done
systemctl is-active --quiet ultraspace || die "the service did not start; see: journalctl -u ultraspace -n 50"

echo
echo "ultraspace $(/usr/local/bin/ultraspace -version | cut -d' ' -f2) is running."
if [ -f /var/lib/ultraspace/setup-token ]; then
  echo "Setup token (paste it on the welcome screen to create your account):"
  echo "  $(cat /var/lib/ultraspace/setup-token)"
fi
if grep -q '^listen = "0.0.0.0' /etc/ultraspace/config.toml; then
  ip=$(hostname -I 2>/dev/null | cut -d' ' -f1)
  echo "Open http://${ip:-<this-machine>}:4600 in a browser on your network."
else
  echo "Listening on 127.0.0.1:4600. Put nginx/Caddy with HTTPS in front (see deploy/nginx.conf),"
  echo "then set public_origin in /etc/ultraspace/config.toml and: sudo systemctl restart ultraspace"
fi
