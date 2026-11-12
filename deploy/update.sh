#!/bin/sh
# Update an existing ultraspace host.
#
# Installs whatever dependency packages the current release needs, then, if a
# binary is available, replaces /usr/local/bin/ultraspace and restarts the
# service. /etc/ultraspace and /var/lib/ultraspace are left alone.
#
#   sudo ./update.sh                      # packages and host setup only
#   sudo ./update.sh /tmp/ultraspace      # packages, then this binary
#
# From a checkout, with the binary already built:
#
#   make update HOST=user@server SSH_KEY=~/.ssh/id_ed25519
#
# When a feature needs a new package, add it to dependencies.sh. This script
# and install.sh both pick it up.
set -eu

die() { echo "update.sh: $*" >&2; exit 1; }
warn() { echo "update.sh: $*" >&2; }
[ "$(id -u)" = 0 ] || die "run it as root: sudo $0 $*"
command -v systemctl >/dev/null || die "needs systemd"

here=$(cd "$(dirname "$0")" && pwd)
[ -f "$here/dependencies.sh" ] || die "dependencies.sh is missing next to this script"

apt_ready=0
apt_install() {
  command -v apt-get >/dev/null || { warn "no apt-get here; install these yourself: $*"; return 1; }
  if [ "$apt_ready" = 0 ]; then apt-get update -qq || return 1; apt_ready=1; fi
  DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends "$@" >/dev/null
}

# shellcheck source=dependencies.sh
. "$here/dependencies.sh"

install_dependencies

if id ultraspace >/dev/null 2>&1; then
  usermod -aG "$(host_groups)" ultraspace || true
else
  warn "no ultraspace user yet; run install.sh for a first install"
fi

bin=${1:-}
if [ -z "$bin" ]; then
  for candidate in "$here/ultraspace" "$here/../bin/ultraspace"; do
    if [ -f "$candidate" ]; then
      bin=$candidate
      break
    fi
  done
fi

if [ -n "$bin" ]; then
  [ -f "$bin" ] || die "no binary at $bin"
  "$bin" -version >/dev/null 2>&1 || die "this binary does not run here (wrong architecture? uname -m says $(uname -m))"
  install -m 755 "$bin" /usr/local/bin/ultraspace
fi

if systemctl cat ultraspace.service >/dev/null 2>&1; then
  systemctl restart ultraspace
  systemctl is-active --quiet ultraspace || die "the service did not start; see: journalctl -u ultraspace -n 50"
  version=$(/usr/local/bin/ultraspace -version | cut -d' ' -f2)
  echo "Updated: ultraspace $version is running. Config and data are unchanged."
else
  echo "Dependencies are in place. ultraspace is not installed yet; run install.sh."
fi
