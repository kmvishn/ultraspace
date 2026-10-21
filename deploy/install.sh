#!/bin/sh
# Install or update ultraspace from an unpacked release (or a checkout with bin/ultraspace).
#
#   sudo ./install.sh --lan     reachable on your network at http://<this-machine>:4600
#   sudo ./install.sh           on 127.0.0.1:4600, for a reverse proxy with HTTPS in front
#
# Run in a terminal, it also offers the optional parts that are not installed yet:
# Terminal and VS Code, the server browser, and ffmpeg for video and browser sound.
# The same choices as flags, for scripts:
#
#   --terminal USER   Terminal and VS Code as Linux user USER (--no-vscode: Terminal only)
#   --browser         server browser (Chrome for Testing headless shell)
#   --media           ffmpeg and pulseaudio (video player, sound in the server browser)
#   --no-extras       ask nothing, install only ultraspace itself
#
# Running it again updates the binary and restarts the service; your config and data stay.
set -eu

die() { echo "install.sh: $*" >&2; exit 1; }
warn() { echo "install.sh: $*" >&2; }
[ "$(id -u)" = 0 ] || die "run it as root: sudo $0 $*"
command -v systemctl >/dev/null || die "needs systemd"

here=$(cd "$(dirname "$0")" && pwd)
bin="$here/ultraspace"
[ -f "$bin" ] || bin="$here/../bin/ultraspace"
[ -f "$bin" ] || die "no ultraspace binary next to this script"
deploy="$here/deploy"
[ -d "$deploy" ] || deploy="$here"

mode=proxy
term_user="" vscode=1 browser=0 media=0 extras=ask
while [ $# -gt 0 ]; do
  case "$1" in
    --lan) mode=lan ;;
    --terminal) [ $# -ge 2 ] || die "--terminal needs a user name"; term_user=$2; shift ;;
    --no-vscode) vscode=0 ;;
    --browser) browser=1 ;;
    --media) media=1 ;;
    --no-extras) extras=no ;;
    *) die "unknown option $1 (see the top of this script)" ;;
  esac
  shift
done
# Flags decide on their own; questions only when nothing was chosen and someone is there to answer.
if [ -n "$term_user" ] || [ "$browser" = 1 ] || [ "$media" = 1 ] || [ ! -t 0 ]; then extras=no; fi

"$bin" -version >/dev/null 2>&1 || die "this binary does not run here (wrong architecture? uname -m says $(uname -m))"

case "$(uname -m)" in
  x86_64 | amd64) arch=amd64 cft=linux64 ;;
  aarch64 | arm64) arch=arm64 cft=linux-arm64 ;;
  *) arch="" cft="" ;;
esac
opt=/opt/ultraspace
tmp=""
cleanup() { [ -z "$tmp" ] || rm -rf "$tmp"; }
trap cleanup EXIT
scratch() { [ -n "$tmp" ] || tmp=$(mktemp -d /var/tmp/ultraspace-install.XXXXXX); }

# ask QUESTION DEFAULT(y|n): true for yes.
ask() {
  printf '%s [%s] ' "$1" "$([ "$2" = y ] && echo Y/n || echo y/N)"
  read -r reply || reply=""
  case "${reply:-$2}" in [Yy]*) return 0 ;; *) return 1 ;; esac
}

apt_ready=0
apt_install() {
  command -v apt-get >/dev/null || { warn "no apt-get here; install these yourself: $*"; return 1; }
  if [ "$apt_ready" = 0 ]; then apt-get update -qq || return 1; apt_ready=1; fi
  DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends "$@" >/dev/null
}

# Package names gained a "t64" suffix in Ubuntu 24.04 / Debian 13; use whichever this release has.
pkg() {
  if apt-cache show "$1" >/dev/null 2>&1; then echo "$1"; else echo "$1" | sed 's/t64$//'; fi
}

need() { # need COMMAND PACKAGE...
  cmd=$1; shift
  command -v "$cmd" >/dev/null || apt_install "$@" || { warn "$cmd is needed"; return 1; }
}

# ---- Terminal and VS Code -------------------------------------------------

install_code_server() {
  [ -n "$arch" ] || { warn "no code-server build for $(uname -m); VS Code skipped"; return 1; }
  need curl curl ca-certificates && need sha256sum coreutils && need tar tar || return 1
  scratch
  echo "Looking up the latest code-server release..."
  curl -fsSL https://api.github.com/repos/coder/code-server/releases/latest -o "$tmp/release.json" ||
    { warn "could not reach GitHub for code-server; VS Code skipped (try again later)"; return 1; }
  ver=$(sed -n 's/.*"tag_name": *"v\([^"]*\)".*/\1/p' "$tmp/release.json" | head -n 1)
  file="code-server-$ver-linux-$arch.tar.gz"
  # The asset's "digest" field follows its "name" in GitHub's release JSON.
  sum=$(awk -v name="\"name\": \"$file\"" 'index($0, name) { hit = 1 }
    hit && /"digest": *"sha256:/ { sub(/.*"sha256:/, ""); sub(/".*/, ""); print; exit }' "$tmp/release.json")
  [ -n "$ver" ] && [ -n "$sum" ] || { warn "no checksum published for $file; VS Code skipped"; return 1; }
  echo "Downloading code-server $ver ($arch, about 240 MB)..."
  curl -fL --progress-bar "https://github.com/coder/code-server/releases/download/v$ver/$file" -o "$tmp/$file" || return 1
  echo "$sum  $tmp/$file" | sha256sum -c --quiet - || { warn "checksum mismatch for $file; not installed"; return 1; }
  mkdir -p "$opt"
  rm -rf "$opt/.code-server-new"
  mkdir "$opt/.code-server-new"
  tar -xzf "$tmp/$file" -C "$opt/.code-server-new" --strip-components=1 || return 1
  rm -f "$tmp/$file"
  [ ! -d "$opt/code-server" ] || { rm -rf "$opt/code-server.old"; mv "$opt/code-server" "$opt/code-server.old"; }
  mv "$opt/.code-server-new" "$opt/code-server"
  "$opt/code-server/bin/code-server" --version >/dev/null 2>&1 || { warn "code-server $ver does not run here"; return 1; }
  echo "code-server $ver installed in $opt/code-server (checksum verified)."
}

install_agent() { # install_agent USER
  u=$1
  if ! id "$u" >/dev/null 2>&1; then
    warn "there is no Linux user \"$u\""
    return 1
  fi
  [ "$(id -u "$u")" != 0 ] || { warn "Terminal and VS Code will not run as root; pick or create a normal user"; return 1; }
  old=$(sed -n 's/^User=//p' /etc/systemd/system/ultraspace-agent.service 2>/dev/null || true)
  sed "s/^SocketUser=.*/SocketUser=$u/" "$deploy/ultraspace-agent.socket" > /etc/systemd/system/ultraspace-agent.socket
  sed -e "s/^User=.*/User=$u/" -e "s/^Group=.*/Group=$(id -gn "$u")/" \
    -e "s#-code-server [^ ]*#-code-server $opt/code-server/bin/code-server#" \
    "$deploy/ultraspace-agent.service" > /etc/systemd/system/ultraspace-agent.service
  systemctl daemon-reload
  if [ -n "$old" ] && [ "$old" != "$u" ]; then
    # The running agent (and its shells) belong to the old user; start over as the new one.
    systemctl stop ultraspace-agent ultraspace-agent.socket 2>/dev/null || true
    echo "Switched Terminal and VS Code from $old to $u (open terminal sessions were closed)."
  fi
  systemctl enable --now ultraspace-agent.socket >/dev/null 2>&1 || { warn "could not start ultraspace-agent.socket"; return 1; }
  [ -S /run/ultraspace-agent.sock ] || { warn "the agent socket did not appear; see: systemctl status ultraspace-agent.socket"; return 1; }
  echo "Terminal and VS Code run as \"$u\". Anyone who can sign in to ultraspace can do what $u can."
}

setup_terminal() { # setup_terminal USER
  u=$1
  if [ "$extras" = ask ] && ! id "$u" >/dev/null 2>&1; then
    ask "There is no user \"$u\". Create it (no sudo rights)?" y || return 1
    useradd --create-home --shell /bin/bash "$u" || return 1
    echo "Created $u. Its home folder, /home/$u, is where Terminal and VS Code start."
  fi
  if [ "$vscode" = 1 ] && [ ! -x "$opt/code-server/bin/code-server" ]; then
    install_code_server || warn "continuing with the Terminal only"
  fi
  install_agent "$u"
}

# ---- Server browser -------------------------------------------------------

install_browser() {
  [ -n "$cft" ] || { warn "no Chrome build for $(uname -m); server browser skipped"; return 1; }
  need curl curl ca-certificates && need unzip unzip || return 1
  scratch
  cv=$(curl -fsSL https://googlechromelabs.github.io/chrome-for-testing/LATEST_RELEASE_STABLE) ||
    { warn "could not reach Chrome for Testing; server browser skipped"; return 1; }
  zip="chrome-headless-shell-$cft.zip"
  echo "Downloading Chrome headless shell $cv ($cft, about 125 MB)..."
  curl -fL --progress-bar -D "$tmp/headers" "https://storage.googleapis.com/chrome-for-testing-public/$cv/$cft/$zip" -o "$tmp/$zip" || return 1
  # Google's storage sends the file's MD5; check the download arrived intact.
  want=$(tr -d '\r' < "$tmp/headers" | sed -n 's/^[Xx]-[Gg]oog-[Hh]ash: md5=//p' | head -n 1)
  if [ -n "$want" ] && command -v openssl >/dev/null; then
    [ "$(openssl dgst -md5 -binary "$tmp/$zip" | base64)" = "$want" ] || { warn "Chrome download is corrupt; not installed"; return 1; }
  fi
  unzip -q "$tmp/$zip" -d "$tmp" || return 1
  rm -f "$tmp/$zip"
  mkdir -p "$opt"
  rm -rf "$opt/chrome.old"
  [ ! -d "$opt/chrome" ] || mv "$opt/chrome" "$opt/chrome.old"
  mv "$tmp/chrome-headless-shell-$cft" "$opt/chrome"

  echo "Installing the libraries and fonts Chrome needs..."
  libs=""
  for p in libatk1.0-0t64 libatk-bridge2.0-0t64 libxcomposite1 libxdamage1 libxext6 libxfixes3 libxrandr2 \
    libgbm1 libxkbcommon0 libasound2t64 libatspi2.0-0t64 libnss3 fontconfig fonts-liberation2 fonts-noto-color-emoji; do
    libs="$libs $(pkg "$p")"
  done
  # shellcheck disable=SC2086 # word splitting intended
  apt_install $libs || warn "some libraries did not install"
  missing=$(ldd "$opt/chrome/chrome-headless-shell" 2>/dev/null | grep 'not found' || true)
  [ -z "$missing" ] || { warn "Chrome is missing libraries:"; echo "$missing" >&2; return 1; }

  # Ubuntu 23.10+ blocks the user namespaces Chrome's sandbox needs unless a profile allows them.
  if [ "$(sysctl -n kernel.apparmor_restrict_unprivileged_userns 2>/dev/null || echo 0)" = 1 ]; then
    mkdir -p /etc/apparmor.d
    install -m 644 "$deploy/apparmor-ultraspace-chrome" /etc/apparmor.d/ultraspace-chrome
    { command -v apparmor_parser >/dev/null && apparmor_parser -r /etc/apparmor.d/ultraspace-chrome; } ||
      { warn "Chrome's sandbox needs its AppArmor profile; load it with: apparmor_parser -r /etc/apparmor.d/ultraspace-chrome"; return 1; }
  fi
  echo "Server browser installed: Chrome $cv in $opt/chrome."
}

install_media() {
  echo "Installing ffmpeg and pulseaudio..."
  apt_install ffmpeg pulseaudio || return 1
  echo "ffmpeg installed: the video player converts any format, and the server browser has sound."
}

# ---- ultraspace itself ----------------------------------------------------

install -m 755 "$bin" /usr/local/bin/ultraspace
id ultraspace >/dev/null 2>&1 || useradd --system --home /var/lib/ultraspace --shell /usr/sbin/nologin ultraspace
mkdir -p /etc/ultraspace

fresh=0
if [ ! -f /etc/ultraspace/config.toml ]; then
  fresh=1
  if [ "$mode" = lan ]; then
    sed -e 's#^listen = .*#listen = "0.0.0.0:4600"#' -e 's#^public_origin = .*#public_origin = ""#' \
      -e 's#^trust_proxy = .*#trust_proxy = false#' "$deploy/config.toml" > /etc/ultraspace/config.toml
  else
    sed -e 's#^public_origin = .*#public_origin = ""   \# set to your https address, e.g. "https://ultraspace.example.com"#' \
      "$deploy/config.toml" > /etc/ultraspace/config.toml
  fi
  echo "Wrote /etc/ultraspace/config.toml ($mode)."
fi

# ---- optional parts -------------------------------------------------------

if [ "$extras" = ask ]; then
  have_agent=0 have_chrome=0
  [ -f /etc/systemd/system/ultraspace-agent.socket ] && have_agent=1
  [ -x "$opt/chrome/chrome-headless-shell" ] && have_chrome=1
  if [ "$have_agent" = 0 ] || [ "$have_chrome" = 0 ] || ! command -v ffmpeg >/dev/null; then
    echo
    echo "Optional parts (each can be added later by running this script again):"
  fi
  if [ "$have_agent" = 0 ]; then
    echo
    echo "  Terminal and VS Code run as a Linux user of your choice. Anyone who can sign in to"
    echo "  ultraspace can then do whatever that user can, including sudo if it has it."
    if ask "Set up Terminal and VS Code?" n; then
      def=${SUDO_USER:-}
      [ -n "$def" ] && [ "$def" != root ] || def=ubuntu
      printf 'Run them as which Linux user? [%s] ' "$def"
      read -r term_user || term_user=""
      term_user=${term_user:-$def}
      ask "Include VS Code (code-server, about 240 MB download, ~700 MB on disk)?" y || vscode=0
    fi
  fi
  if [ "$have_chrome" = 0 ]; then
    echo
    echo "  The server browser runs a real Chrome on this machine and streams it to you"
    echo "  (about 125 MB download plus libraries; uses 150-400 MB of memory while open)."
    ask "Set up the server browser?" n && browser=1
  fi
  if ! command -v ffmpeg >/dev/null && command -v apt-get >/dev/null; then
    echo
    echo "  ffmpeg lets the video player play any format and gives the server browser sound."
    ask "Install ffmpeg?" n && media=1
  fi
  echo
fi

[ -z "$term_user" ] || setup_terminal "$term_user" || warn "Terminal and VS Code were not set up; see README, step 5"
[ "$browser" = 0 ] || install_browser || warn "the server browser was not set up; see README, step 4"
[ "$media" = 0 ] || install_media || warn "ffmpeg was not installed"

# ---- start ----------------------------------------------------------------

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

sleep 2
i=0
until [ -f /var/lib/ultraspace/setup-token ] || [ -f /var/lib/ultraspace/ultraspace.db ] || [ $i -ge 20 ]; do sleep 1; i=$((i + 1)); done
systemctl is-active --quiet ultraspace || die "the service did not start; see: journalctl -u ultraspace -n 50"

echo
version=$(/usr/local/bin/ultraspace -version | cut -d' ' -f2)
if [ "$fresh" = 0 ] && [ ! -f /var/lib/ultraspace/setup-token ]; then
  echo "Updated: ultraspace $version is running. Your config and data are unchanged."
  if systemctl is-active --quiet ultraspace-agent 2>/dev/null; then
    echo "Terminal sessions keep running on the old agent; to update it too (this ends them):"
    echo "  sudo systemctl restart ultraspace-agent"
  fi
  exit 0
fi
echo "ultraspace $version is running."
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
