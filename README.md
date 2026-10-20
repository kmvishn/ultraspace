# ultraspace

**Your own server, as a desktop in the browser.** ultraspace is a single Go binary that turns a
small Linux VPS into a personal web desktop: files, system monitor, music player, a real web
browser, a terminal and VS Code — all in windows you can drag, snap and resize, from any device.

Sign in on your laptop, close the lid, open your phone: the same windows, tabs, terminal sessions,
song and position are waiting for you.

```
┌──────────────────────────────────────────────────────────────────────┐
│ ◉ Files                          ▣ 6%  ▤ 41%  ↓ 120 KB/s  🔍  user  12:04 │
│  ┌─────────────── Files ───────────────┐   ┌──── Terminal ────────┐   │
│  │ Places   ‹ › ↑  Home › Music        │   │ ubuntu@v1:~$ go test │   │
│  │ Home     ▢ Lo-fi   ▢ Night Drive    │   │ ok  ultraspace 1.2s  │   │
│  │ Music    ▢ cover.jpg                │   │ ubuntu@v1:~$ █       │   │
│  └─────────────────────────────────────┘   └──────────────────────┘   │
│                  [ 🌐 ][ 📁 ][ 📈 ][ 🎵 ][ ⚙ ][ >_ ][ ‹› ]             │
└──────────────────────────────────────────────────────────────────────┘
```

- **One binary**, no runtime dependencies, no build step for the frontend. ~13 MB on disk, ~13 MB of RAM idle.
- **Desktop UI**: lock screen, menu bar with live stats, dock, widgets, Ctrl+K launcher, light/dark themes, wallpapers.
- **Follows you everywhere**: the workspace is saved on the server and restored on any device.
- **Heavy parts start on demand**: Chrome and VS Code run only while you use them and stop when idle.
- **Security first**: setup token, hashed sliding sessions, CSRF and origin checks, strict CSP, systemd sandboxing,
  and an egress filter on the remote browser.

---

## Contents

- [Apps](#apps)
- [How it works](#how-it-works)
- [Security model](#security-model)
- [Requirements](#requirements)
- [Quick start (local)](#quick-start-local)
- [Installing on a server](#installing-on-a-server)
- [Configuration](#configuration)
- [Using it](#using-it)
- [Resource usage](#resource-usage)
- [Development](#development)
- [HTTP API](#http-api)
- [Limitations](#limitations)
- [Credits](#credits)

---

## Apps

### Desktop

- **Lock screen** with clock; first run asks for a one-time setup token from the server.
- **Menu bar**: focused app, now playing, CPU / memory / network, shared clipboard, search, account, clock.
- **Dock** with running indicators; it scrolls sideways (swipe, or mouse wheel) when the apps do not fit, with a
  fade on the side that has more. **Widgets** for the clock, system rings and a live network graph.
- **Windows**: drag, resize from any edge, double-click to maximize, drag to the top or sides to snap, minimize to the dock.
  Each app remembers its size and position. Minimise, maximise and close sit on the right, Windows style; Settings →
  Appearance → *Window buttons* switches to macOS-style buttons on the left.
- **Launcher (Ctrl+K)**: apps, files anywhere in your library, and web addresses or searches for the Browser.
- **Appearance**: light, dark or auto theme, 10 accent colours, 8 built-in wallpapers or any image from Files,
  frosted-glass effects (can be turned off on slow devices).
- **Phones** (screens under 760 px; the desktop layout is untouched):
  - windows go full screen, and a ☰ button in the title bar slides an app's sidebar in as a drawer (Files places,
    System tabs, Music and Navidrome, the Notes list, Settings sections, Photo folders);
  - menus open as a sheet from the bottom, notifications appear above the dock, rows and buttons are sized for
    fingers, and text fields are 16 px so iPhones do not zoom;
  - a long press opens context menus (on iPhone and iPad too, which have no right-click event);
  - the Terminal adds a row of Esc / Tab / Ctrl / arrow keys, and the server Browser lays pages out for a phone.

### Files

A file manager confined to one folder on the server.

- Grid with image thumbnails or a list; breadcrumbs, back / forward / up, places sidebar, storage meter.
- Multi-select (Ctrl / Shift), right-click menus, keyboard shortcuts (Delete, F2, Enter, Backspace, Ctrl+A).
- Drag files onto folders to move them; drop files from your computer to upload (with progress and speed).
- Search across the whole library; *Set as wallpaper* for images; *Play in Music* for audio.
- **Between this computer and the server**: drop files onto a Files window (or the empty desktop) to upload, paste
  files with Ctrl+V into the open folder, and drag a file out of Files onto your computer's desktop to download it
  (Chrome and Edge). Music and Notes accept dropped audio and `.md`/`.txt` files too.
- Opens images and videos in the **Viewer**, audio in **Music**, text and code in the **Text Editor**.

### System

- Live CPU, memory and network charts (history kept on the server, so the graph is full when you open it).
- Disk usage, uptime, load, kernel and host information.
- **Processes**: sortable, filterable, refreshed every few seconds.
- **Services**: every systemd unit with its state, filter for failed units, and the unit's journal side by side.
- **Logs**: the system journal, filtered by priority, live.

Read-only by design: ultraspace itself runs unprivileged and cannot start or stop services.

### Music

One player for two libraries.

- **On this server**: audio files anywhere in Files. Each folder becomes an album, a `cover.jpg` / `folder.png`
  inside it becomes the artwork, and names like `01 - Artist - Title.mp3` are parsed into track, artist and title.
- **Navidrome** (or any Subsonic-compatible server): albums sorted by recently added / A–Z / most played /
  recently played / favourites / random, artists, playlists, favourites, *Mix for me*, and search.
  Hearts and play counts sync back to Navidrome.
- Player: shuffle, repeat (all / one), seeking, volume, keyboard shortcuts, and media keys / lock-screen
  controls through the Media Session API.
- The queue, track and position are part of your session: switch devices and pick up where you left off.

Navidrome credentials are entered once in the app. The server stores only a salted Subsonic token (never the
password), never sends it to the browser, and relays only listening-related API calls.

### Browser

Two engines, switchable from the ⋯ menu:

- **Server browser (default when installed)**: a real Chrome (`chrome-headless-shell`) runs on the server.
  Its screen is streamed to your window as JPEG frames over a WebSocket; mouse, wheel, keyboard, touch, paste and
  copy are sent back.
  - Every site works (GitHub, YouTube, sign-in pages…), because it is a real browser.
  - Cookies, logins, tabs and history live on the server, so they follow you to every device.
  - Downloads land in **Files → Downloads**, with a *Save to this device* button when they finish.
  - **Files into web pages**: a site's "Choose file" button asks whether to send a file from this device or from
    Files; you can also drag files from your computer onto the page, or paste (Ctrl+V) an image or file into it,
    for example into a mail or chat page. Files are staged on the server under random ids for six hours.
  - Pop-ups open as new tabs; page dialogs (alert, confirm, prompt, leave-page) appear as desktop dialogs.
  - Chrome starts when a Browser window connects and stops after a few idle minutes; a background tab stops
    streaming after a minute. Tabs are saved and reopened on the next start.
  - **Sleeping tabs**: background tabs are closed after a while (or beyond a per-machine limit, or at once when
    memory is short) and reload when you click them, like Chrome's tab discarding. After a restart only the
    active tab loads.
  - If the server is short of free memory, the browser says so instead of starting and pushing it into swap.
  - **Sound** (speaker button): Chrome plays into a private PulseAudio sink on the server; while a window has
    sound on, ffmpeg encodes it as AAC (fragmented MP4, ~130 kbit/s) and it plays in your browser through Media
    Source Extensions, kept within about a second of the picture. Needs `pulseaudio` and `ffmpeg` on the server;
    without them the browser is simply silent.
- **Light mode**: pages load in a sandboxed frame on your own device. Uses no server memory, but many sites refuse
  to be framed; for those it shows a clear page with *Open in a new browser tab*.

Both modes share bookmarks, history and the search engine choice (Google, Wikipedia, DuckDuckGo, Bing),
saved on the server.

### Terminal

- Real shells on the server, as the configured Linux user, rendered with xterm.js.
- **Sessions persist**: close the window, lose Wi-Fi or switch devices — the shell keeps running, and reattaching
  replays the recent output (512 KB of scrollback per session).
- Tabs, titles from the shell, clickable links, Ctrl+Shift+C / Ctrl+Shift+V, right-click menu.
- On touch devices, an extra key row: Esc, Tab, sticky Ctrl, arrows, `| ~ / -`.

### VS Code

- Full VS Code in the browser via [code-server](https://github.com/coder/code-server), running as the same Linux
  user as the Terminal.
- Open folders, editors, unsaved changes (hot exit), settings and extensions are kept on the server.
- Starts on first use (a few seconds), stops after a period unused (5–20 minutes depending on the machine); can
  also be opened in its own browser tab. A desktop left in a background tab lets go of it after 10 minutes.
- Runs lean by default: a smaller Node.js heap setting and memory-hungry extras (AI agent host, experiments,
  update checks, telemetry) turned off, which measured about 27% less memory. Existing settings are only ever
  added to, once.

### Notes

- Markdown notes kept as ordinary `.md` files in **Files → Notes**, so they can also be opened, backed up or edited
  anywhere else (including VS Code).
- Edit, split or preview; autosave; title = file name (renaming the title renames the file).
- Toolbar and shortcuts for headings, **bold** (Ctrl+B), *italic* (Ctrl+I), lists, checkboxes, code and links;
  lists continue on Enter.
- Tick checkboxes straight in the preview. Full-text search across notes.
- Changes made on another device appear when you come back, without ever overwriting unsaved typing.
- The renderer escapes all HTML and only allows http(s) and mailto links.

### Photos

- Every image and video in Files as a timeline, grouped by the month it was **taken** (EXIF date, else the file
  date), with All / Photos / Videos and per-folder views and three tile sizes.
- Thumbnails are made on the server, upright (EXIF orientation), cached on disk and served with ETags. They are
  generated one at a time and huge images are skipped, so a 512 MB server never decodes several photos at once.
  Files uses the same thumbnails instead of loading full images.
- Opens in the Viewer and steps through the current view; right-click for Show in Files, wallpaper, download,
  delete. Upload with the button or by dropping files on the window (they go to **Files → Photos**).
- Long timelines render in pages as you scroll.

### Shared clipboard

- The clipboard icon in the menu bar holds text shared between all your devices. Send what you type or paste;
  every other open desktop gets it **instantly** (Server-Sent Events) with a one-click *Copy*.
- Copying in the Terminal (Ctrl+Shift+C) or in the server Browser (Ctrl+C) also goes to the shared clipboard.
- The last 25 items are kept on the server; remove items or clear it any time.

### Also

- **Text Editor**: Ctrl+S to save, tab key, word wrap, line/column, unsaved-changes guard. Unsaved text travels
  with your session to other devices.
- **Viewer**: images and videos with arrow-key navigation through the folder, *Set as wallpaper*, download.
- **Settings**: appearance, account (change password — signs out other devices), **Devices** (every signed-in
  browser with IP and last activity; sign out any or all others), and About.

---

## How it works

```
                 HTTPS                        loopback
 Browser  ───────────────►  nginx  ─────────────────────►  ultraspace  (user: ultraspace)
 (any device)   (TLS, :443)        127.0.0.1:4600            │  SQLite: users, sessions, settings, workspace
                                                             │  Files root (os.Root, no escapes)
                                                             │  /proc + systemctl/journalctl (read-only)
                                                             │
                                ┌────────────────────────────┼─────────────────────────────┐
                                │                            │                             │
                     stdin/stdout pipe (CDP)          Unix socket, group-only        HTTPS (Subsonic)
                                │                            │                             │
                     chrome-headless-shell            ultraspace agent              Navidrome server
                     (on demand, sandboxed)           (user: your login user)
                                │                     ├─ PTY sessions (Terminal)
                  egress proxy in ultraspace          └─ code-server (VS Code, on demand)
                  (public addresses only)
```

- **ultraspace** (`ultraspace` with no arguments) is the web server. It embeds the frontend (`web/static`),
  stores everything in SQLite (`modernc.org/sqlite`, pure Go), and serves a JSON API.
- **The frontend** is plain ES modules with no build step: a small DOM helper, a window manager, and one module per
  app. Styles use CSS custom properties for theming.
- **The remote browser** is driven over `--remote-debugging-pipe` (no debugging port is opened). One Chrome per
  ultraspace user; only the active tab is screencast. All of Chrome's traffic goes through a forward proxy inside
  ultraspace that refuses loopback, private, link-local and other non-public addresses.
- **ultraspace agent** (`ultraspace agent`) is the same binary in a second mode, run as your login user by a
  separate systemd service. It listens on a socket that only the `ultraspace` group may open; the main server
  checks your login and origin, then proxies Terminal and VS Code traffic to it.
- **Workspace sync**: every few seconds the desktop saves its open windows and each app's state
  (`saveState()`) to `/api/state/workspace`. On sign-in it is restored. If another device saved more recently, a
  write is refused with 409 and you are asked whether to switch to that session or keep this one.

---

## Security model

ultraspace is built for one person (or a few trusted people) exposing a personal server to the internet.

**Accounts and sessions**
- The first account can only be created with a one-time **setup token** written to the data directory
  (`0600`), so only someone with shell access to the server can claim it. The file is deleted after setup.
- Passwords are hashed with bcrypt; unknown usernames are compared against a dummy hash (no timing leak).
- Logins, setup and password changes are **rate limited** per IP (10 failures per 10 minutes).
- Session cookies are random 256-bit tokens, stored only as SHA-256 hashes; `HttpOnly`, `SameSite=Strict`,
  `Secure` behind HTTPS. Sessions slide: they expire after `session_ttl` of *inactivity* (default 30 days).
- Changing the password signs out every other device; Settings → Devices can sign out any single device.

**Requests**
- State-changing requests need a custom header (`X-Ultraspace: 1`), a matching `Origin`, and are refused when
  `Sec-Fetch-Site` is cross-site. WebSockets check the origin.
- Strict **Content-Security-Policy**: scripts only from the app itself, no framing of ultraspace by other sites.
  (`style-src` allows inline styles because xterm.js themes itself with a `<style>` element.)
- File downloads are served with a sandboxing CSP and SVGs are never rendered inline.

**Files**
- All file access goes through Go's `os.Root`, so paths cannot escape the files root, including via `..` or
  symlinks (covered by tests). Writes are atomic (temp file + rename).

**Remote browser**
- Chrome's own sandbox stays on (an AppArmor profile allows the user namespaces Ubuntu otherwise blocks).
- An egress proxy blocks loopback, private (RFC 1918 / ULA), link-local (including the cloud metadata service at
  `169.254.169.254`), carrier-grade NAT, multicast and other reserved ranges — checked on the *resolved* address
  of every connection, so DNS tricks and redirects do not help. WebRTC is restricted to proxied connections.
- The light-mode site checker uses the same public-only rule.

**Terminal and VS Code** — read this before enabling them
- They run as the Linux user in `ultraspace-agent.service` (the example uses `ubuntu`). **Anyone who can sign in to
  ultraspace can do anything that user can.** On many cloud images that user has password-less `sudo`, which makes
  your ultraspace password equivalent to root. Use a long, unique password, or run the agent as a dedicated user
  without sudo.
- SSH access is not touched: key-only SSH keeps working exactly as before.

**Navidrome**
- Only a salted Subsonic token is stored, server-side, in a record the state API does not expose. The browser
  cannot supply its own credentials, and only an allow-list of listening, favourite and playlist calls is relayed.

**systemd hardening** (`deploy/ultraspace.service`): dedicated user, no capabilities, `NoNewPrivileges`,
`ProtectSystem=strict` (writable: the data directory only), `ProtectHome`, `PrivateTmp`, `PrivateDevices`, kernel
and cgroup protection, address-family restrictions, and memory limits so Chrome can never starve the host.

---

## Requirements

- Linux, x86-64 or arm64. Tested on **Ubuntu 24.04** (Oracle Cloud free tier, 1 GB RAM + 2 GB swap).
- Go **1.26+** to build.
- nginx (or any TLS reverse proxy) and a domain for HTTPS — a free [DuckDNS](https://www.duckdns.org) name and
  Let's Encrypt work well.
- Optional:
  - [chrome-headless-shell](https://googlechromelabs.github.io/chrome-for-testing/) for the server browser
    (~270 MB on disk).
  - [code-server](https://github.com/coder/code-server/releases) for VS Code (~700 MB on disk).

---

## Quick start (local)

```sh
git clone <this repo> ultraspace && cd ultraspace
make run                     # http://127.0.0.1:4600, data in ./dev-data
cat dev-data/setup-token     # paste this on the welcome screen to create your account
```

`make run` uses built-in defaults. To try the server browser or the terminal locally, point a config file at
them (see [Configuration](#configuration)) and start the agent in another shell:

```sh
./bin/ultraspace agent -socket /tmp/ultraspace-agent.sock \
  -code-server /path/to/code-server/bin/code-server
```

On Ubuntu 23.10+ Chrome's sandbox needs the AppArmor profile from `deploy/`; for a quick local test you can set
`no_sandbox = true` under `[browser]` instead (do not do this on a server).

---

## Installing on a server

The steps below match the files in `deploy/`. Replace `ultraspace.example.com` with your domain and `ubuntu` with
the user Terminal and VS Code should run as.

### 1. Build and copy the binary

```sh
make build                                   # or: make build-arm64 for ARM servers
scp bin/ultraspace server:/tmp/
ssh server 'sudo install -m 755 /tmp/ultraspace /usr/local/bin/ultraspace'
```

### 2. User, config and service

```sh
sudo useradd --system --home /var/lib/ultraspace --shell /usr/sbin/nologin ultraspace
sudo mkdir -p /etc/ultraspace
sudo cp deploy/config.toml /etc/ultraspace/config.toml   # then edit public_origin
sudo cp deploy/ultraspace.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now ultraspace
sudo cat /var/lib/ultraspace/setup-token             # needed once, on the welcome screen
```

The unit adds the `systemd-journal` group so the Logs view can read the journal; remove
`SupplementaryGroups=` if you do not want that.

### 3. HTTPS with nginx

```sh
sudo apt install nginx certbot python3-certbot-nginx
sudo cp deploy/nginx.conf /etc/nginx/sites-available/ultraspace     # edit server_name
sudo ln -s /etc/nginx/sites-available/ultraspace /etc/nginx/sites-enabled/
sudo certbot --nginx -d ultraspace.example.com
```

The nginx site passes WebSocket upgrades, disables buffering (for uploads and streams) and allows large uploads.
Open ports 80 and 443 in your firewall (on Oracle Cloud: both the instance's iptables and the VCN security list).

### 4. Server browser (optional)

```sh
# chrome-headless-shell from Chrome for Testing (linux64)
sudo mkdir -p /opt/ultraspace
unzip chrome-headless-shell-linux64.zip
sudo mv chrome-headless-shell-linux64 /opt/ultraspace/chrome

# optional, for sound in the server browser
sudo apt install --no-install-recommends pulseaudio ffmpeg

# libraries and fonts it needs on a minimal Ubuntu server
sudo apt install --no-install-recommends libatk1.0-0t64 libatk-bridge2.0-0t64 libxcomposite1 libxdamage1 \
  libxext6 libxfixes3 libxrandr2 libgbm1 libxkbcommon0 libasound2t64 libatspi2.0-0t64 \
  fontconfig fonts-liberation2 fonts-noto-color-emoji

# let Chrome build its sandbox (Ubuntu 23.10+ restricts user namespaces)
sudo cp deploy/apparmor-ultraspace-chrome /etc/apparmor.d/ultraspace-chrome
sudo apparmor_parser -r /etc/apparmor.d/ultraspace-chrome
```

Check with `ldd /opt/ultraspace/chrome/chrome-headless-shell | grep 'not found'` — it should print nothing.
The `[browser]` section in the example config already points at this path.

### 5. Terminal and VS Code (optional)

```sh
# code-server release tarball (verify the published sha256)
sudo tar -xzf code-server-*-linux-amd64.tar.gz -C /opt/ultraspace
sudo mv /opt/ultraspace/code-server-*-linux-amd64 /opt/ultraspace/code-server

# edit User=/Group= and SocketUser= if your login user is not "ubuntu"
sudo cp deploy/ultraspace-agent.socket deploy/ultraspace-agent.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now ultraspace-agent.socket
```

The agent is socket-activated: it starts on the first Terminal or VS Code request.

### 6. Updating

```sh
make deploy HOST=user@server SSH_KEY=~/.ssh/id_ed25519
```

This builds, copies the binary and restarts `ultraspace`. Restarting `ultraspace-agent` (only needed when the agent
code changes) ends running terminal sessions.

---

## Configuration

`/etc/ultraspace/config.toml` (all keys optional; defaults shown):

```toml
listen        = "127.0.0.1:4600"        # keep on loopback behind a TLS proxy
data_dir      = "/var/lib/ultraspace"   # database, setup token, browser profiles
files_root    = ""                      # default: <data_dir>/files
public_origin = ""                      # e.g. "https://ultraspace.example.com", accepted in Origin checks
trust_proxy   = false                   # trust X-Forwarded-Proto / X-Real-IP from 127.0.0.1
session_ttl   = "720h"                  # sign out after this much inactivity
memory_profile = "auto"                 # auto | tiny | small | normal (see Resource usage)
agent_socket  = "/run/ultraspace-agent.sock"

[browser]
chrome     = "/opt/ultraspace/chrome/chrome-headless-shell"   # missing = light mode only
idle       = "0"       # stop Chrome after this long with no window connected (0 = from memory_profile)
quality    = 70        # JPEG quality of the streamed screen
no_sandbox = false     # only for hosts where Chrome's sandbox cannot run
```

Command-line flags: `-config`, `-listen`, `-data`, `-debug`, `-version`.

Agent flags (`ultraspace agent`): `-socket` (when not socket-activated), `-code-server` (binary path),
`-code-idle` (default from the memory profile), `-memory-profile`, `-debug`.

---

## Using it

| Where | Shortcut | Action |
| --- | --- | --- |
| Desktop | Ctrl+K | Launcher: apps, files, web addresses and searches |
| Windows | double-click title | Maximize / restore |
| Windows | drag to top / left / right edge | Maximize / snap to half |
| Files | Delete, F2, Enter, Backspace, Ctrl+A | Delete, rename, open, go up, select all |
| Editor | Ctrl+S | Save |
| Viewer | ← → | Previous / next image |
| Music | Space, Shift+← / Shift+→ | Play/pause, previous / next |
| Browser | Ctrl+L, Ctrl+R / F5, Alt+← / Alt+→ | Address bar, reload, back / forward |
| Terminal | Ctrl+Shift+C / Ctrl+Shift+V | Copy / paste |

**Connecting Navidrome:** Music → *Connect Navidrome*, then enter the server address (for example
`https://music.example.com`), your Navidrome username and password. Do it once; it applies on every device.

**Continuing on another device:** just sign in. Open windows, the Music queue and position, and unsaved editor
text are restored; Browser tabs, Terminal sessions and VS Code were never closed, because they run on the server.

---

## Resource usage

ultraspace is meant to stay smooth down to a **512 MB** VPS. The core is tiny; the two heavy apps start only
when used and size themselves to the machine with a **memory profile**, picked from total RAM unless you set
`memory_profile`:

| Profile | RAM | Live browser tabs | Chrome renderers | Background tab sleeps after | Chrome stops after | VS Code stops after |
| --- | --- | --- | --- | --- | --- | --- |
| `tiny` | ≤ 700 MB | 1 | 1 | 2 min | 3 min idle | 5 min idle |
| `small` | ≤ 1.5 GB | 2 | 2 | 10 min | 5 min idle | 10 min idle |
| `normal` | more | 6 | 4 | 30 min | 10 min idle | 20 min idle |

Measured (PSS, which counts shared memory once):

| Component | Memory | When |
| --- | --- | --- |
| ultraspace | ~15–25 MB | always |
| ultraspace agent | a few MB plus your shells | after first Terminal/VS Code use |
| Chrome, blank | ~150 MB | while a Browser window is connected |
| Chrome, one live page | + the page: ~60 MB (Wikipedia) to ~300 MB (YouTube, news sites) | |
| code-server (VS Code) | ~360 MB with a small project open (was ~500 MB before tuning) | while in use |

Tuning built in:
- Chrome: no software GPU rasterizer (~130 MB less), no back-forward cache, capped renderer processes, smaller
  JavaScript young generation on `tiny`, sleeping tabs, and a free-memory check before starting.
- code-server: `--max-semi-space-size=1` for its Node processes (not passed to your own programs), AI agent host,
  experiments, update checks and telemetry off.
- Chrome and code-server get a higher `oom_score_adj`, so if memory runs out the kernel kills them, not sshd or
  nginx.
- The services cap memory as a share of RAM (`MemoryHigh=60%`/`MemoryMax=75%` for ultraspace and Chrome,
  `MemoryHigh=70%` for the agent), so one unit file fits every machine size.
- The frontend is served precompressed with ETags (repeat visits transfer almost nothing), xterm.js loads only
  when a Terminal opens, and the music scan asks the server for one cover per folder instead of every image.

On a 512 MB machine, use Chrome or VS Code one at a time, and add swap (or zram) so short peaks do not hit the
out-of-memory killer.

---

## Development

```
main.go                  flags; "ultraspace agent" subcommand
internal/
  server/                wiring, security headers, CSRF, static files, integration tests
  auth/                  setup, login, sessions, devices, rate limiter
  store/                 SQLite schema, migrations, queries
  files/                 file manager API on os.Root
  monitor/               /proc sampling, history, processes
  services/              systemctl / journalctl readers
  state/                 per-user JSON documents with conflict detection (workspace, bookmarks)
  sysmem/                host memory, memory profiles, OOM preference
  browser/               public-address dialer, light-mode site checker
  remote/                Chrome over CDP pipe, screencast, input, tabs, sound, egress proxy
  agent/                 agent server (PTY sessions, code-server) and the authenticated proxy to it
  navidrome/             Subsonic relay with server-side credentials
  clip/                  shared clipboard and the live event stream
  config/  httpx/        config loading, HTTP helpers
web/static/
  index.html style.css   shell and all styles
  js/core.js             DOM helper, API client, menus, dialogs, settings, formatting
  js/wm.js               window manager
  js/desktop.js          menu bar, dock, widgets, launcher, desktop drop-to-upload
  js/clipboard.js        shared clipboard panel and the live event stream
  js/markdown.js         safe Markdown renderer for Notes
  js/pick.js             picker for files in the library
  js/session.js          workspace sync across devices
  js/apps/*.js           one module per app
  vendor/xterm/          xterm.js (MIT)
deploy/                  systemd units, nginx site, AppArmor profile, example config
```

```sh
make test       # go vet + go test -race ./...
make build      # bin/ultraspace for this machine; VERSION=x.y.z to stamp a version
make build-arm64
make run        # local server with ./dev-data
```

The frontend has no build step: edit files under `web/static`, rebuild the binary (they are embedded), reload.
Apps register themselves with `registerApp({ id, name, icon, color, mount })`; `mount(win, args)` returns hooks
such as `onArgs`, `onFocus`, `onResize`, `onClose`, `onDestroy` and `saveState` (for workspace sync).

Tests cover setup and login, rate limiting, CSRF, file-root escapes (`..` and symlinks), uploads and writes,
search, settings, workspace sync and conflicts, device sign-out, database migration, the browser egress proxy,
the public-address rules, terminal session replay, the Navidrome relay, static caching and compression,
cover search, VS Code settings merging, photo dates/orientation/thumbnails, the shared clipboard and its live
events, the sound stream's MP4 splitting (with ffmpeg installed), and (with `ULTRASPACE_TEST_CHROME` set) sleeping and restoring tabs
in a real Chrome, plus file choosers, drag-and-drop and pasted files reaching a page.

---

## HTTP API

All endpoints except setup, login, logout and health need a session cookie. Non-GET requests need the header
`X-Ultraspace: 1`.

| Area | Endpoints |
| --- | --- |
| Auth | `GET/POST /api/setup`, `POST /api/login`, `POST /api/logout`, `GET /api/me` |
| Account | `POST /api/account/password`, `GET/PUT /api/account/settings`, `GET /api/account/sessions`, `POST /api/account/sessions/revoke` |
| Files | `GET /api/files/list`, `GET /api/files/download`, `GET /api/files/search`, `POST /api/files/{upload,mkdir,rename,delete,write}` |
| System | `GET /api/system`, `GET /api/system/history`, `GET /api/system/processes`, `GET /api/services`, `GET /api/logs` |
| Sync | `GET/PUT /api/state/{workspace,browser}` |
| Browser | `GET /api/browser/probe`, `GET /api/remote/status`, `GET /api/remote/ws` (WebSocket), `POST /api/remote/stop`, `POST /api/remote/{upload,import}` |
| Terminal | `GET /api/agent/status`, `GET/POST /api/term/sessions`, `DELETE /api/term/sessions/{id}`, `GET /api/term/sessions/{id}/ws` (WebSocket) |
| VS Code | `/code/…` (proxied code-server) |
| Navidrome | `GET /api/navidrome`, `POST /api/navidrome/{connect,disconnect}`, `GET /api/navidrome/rest/{method}` |
| Photos | `GET /api/files/photos`, `GET /api/files/thumb?path=&size=256\|512` |
| Clipboard | `GET/POST/DELETE /api/clipboard`, `DELETE /api/clipboard/{id}`, `GET /api/events` (Server-Sent Events) |
| Health | `GET /api/health` |

---

## Limitations

- **Server browser**: streamed images, so scrolling and video are choppier than a local browser (roughly
  10–15 fps), and sound runs slightly behind the picture. Some sites may still detect an automated browser.
- **Light mode** cannot follow navigation inside a page, and site logins stay on that device.
- **Music** reads album and artist from folder and file names, not from tags, for files in Files.
- **System** is read-only: no starting or stopping services from the web.
- **Terminal and VS Code** see the agent user's home directory, not the Files library (which belongs to the
  `ultraspace` user).
- One Chrome and one code-server per server; built for personal use, not many concurrent users.

---

## Credits

- [xterm.js](https://xtermjs.org) (MIT) — terminal rendering, vendored in `web/static/vendor/xterm`.
- [code-server](https://github.com/coder/code-server) — VS Code in the browser (installed separately).
- [Chrome for Testing](https://googlechromelabs.github.io/chrome-for-testing/) — `chrome-headless-shell`
  (installed separately).
- [Navidrome](https://www.navidrome.org) — the music server the Music app can connect to.
- Go libraries: [modernc.org/sqlite](https://gitlab.com/cznic/sqlite), [coder/websocket](https://github.com/coder/websocket),
  [creack/pty](https://github.com/creack/pty), [BurntSushi/toml](https://github.com/BurntSushi/toml),
  [golang.org/x/crypto](https://pkg.go.dev/golang.org/x/crypto).
- Icons drawn in the style of [Lucide](https://lucide.dev).

No license has been chosen for ultraspace yet; until one is added, all rights are reserved by the author.
