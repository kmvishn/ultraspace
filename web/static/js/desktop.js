// The desktop shell: wallpaper, menu bar, dock, widgets and the Ctrl+K launcher.
import {
  h, icon, api, on, emit, menu, closeMenu, settings, system, fmtBytes, fmtDuration, debounce, drawChart,
  kindOf, KIND_ICON, dirOf, fileUrl, report,
} from './core.js';
import { initWM, allApps, openApp, openFile, windowsOf, focusedWindow, closeAllWindows } from './wm.js';
import { stopSession } from './session.js';
import { webTarget } from './apps/browser.js';

export const WALLPAPERS = [
  { id: 'aurora', name: 'Aurora' },
  { id: 'dusk', name: 'Dusk' },
  { id: 'ocean', name: 'Ocean' },
  { id: 'forest', name: 'Forest' },
  { id: 'sunset', name: 'Sunset' },
  { id: 'graphite', name: 'Graphite' },
  { id: 'dawn', name: 'Dawn' },
  { id: 'mist', name: 'Mist' },
];

export const ACCENTS = ['#7c6cf2', '#3b82f6', '#06b6d4', '#10b981', '#84cc16', '#f59e0b', '#f97316', '#ef4444', '#ec4899', '#a1a1aa'];

const media = matchMedia('(prefers-color-scheme: dark)');

export function applyAppearance(root, s) {
  const html = document.documentElement;
  const theme = s.theme === 'auto' ? (media.matches ? 'dark' : 'light') : s.theme;
  html.dataset.theme = theme;
  html.dataset.glass = s.transparency ? 'on' : 'off';
  if (/^#[0-9a-f]{6}$/i.test(s.accent)) html.style.setProperty('--accent', s.accent);
  if (!root) return;
  root.className = 'desktop';
  root.style.backgroundImage = '';
  const wp = String(s.wallpaper || 'aurora');
  if (wp.startsWith('file:')) {
    root.classList.add('wp-image');
    root.style.backgroundImage = `url("${fileUrl(wp.slice(5), true)}")`;
  } else {
    root.classList.add(`wp-${WALLPAPERS.some((w) => w.id === wp) ? wp : 'aurora'}`);
  }
}
media.addEventListener('change', () => applyAppearance(document.querySelector('.desktop'), settings.data));

let cleanup = [];

export function buildDesktop(user, { onSignOut }) {
  cleanup.forEach((fn) => fn());
  cleanup = [];

  const root = h('div', { class: 'desktop' });
  const windowsLayer = h('div', { class: 'windows' });
  const bar = menuBar(user, onSignOut);
  const widgets = buildWidgets(user);
  const dock = buildDock();
  root.append(widgets, windowsLayer, bar, dock);
  initWM(windowsLayer);
  applyAppearance(root, settings.data);
  cleanup.push(on('settings', (s) => applyAppearance(root, s)));

  // Right-click on the bare desktop.
  root.addEventListener('contextmenu', (e) => {
    if (e.target !== root && e.target !== windowsLayer && e.target !== widgets) return;
    e.preventDefault();
    menu(e.clientX, e.clientY, [
      { label: 'Open Files', icon: 'folder', action: () => openApp('files') },
      { label: 'Search…', icon: 'search', hint: 'Ctrl K', action: openLauncher },
      '-',
      { label: 'Change wallpaper…', icon: 'wallpaper', action: () => openApp('settings', { section: 'appearance' }) },
    ]);
  });

  const onKey = (e) => {
    if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 'k') { e.preventDefault(); openLauncher(); }
  };
  addEventListener('keydown', onKey);
  cleanup.push(() => removeEventListener('keydown', onKey));
  cleanup.push(() => { stopSession(); closeAllWindows(); });
  system.start();
  cleanup.push(() => system.stop());
  return root;
}

// ---------- menu bar ----------

function menuBar(user, onSignOut) {
  const appName = h('span', { class: 'mb-app' }, 'Desktop');
  const clock = h('span', { class: 'mb-clock' });
  const cpu = h('span', { class: 'mb-val' }, '–');
  const mem = h('span', { class: 'mb-val' }, '–');
  const net = h('span', { class: 'mb-val' }, '–');
  const nowPlaying = h('button', { class: 'mb-item mb-music hidden', onclick: () => openApp('music') });

  const logo = h('button', {
    class: 'mb-item mb-logo', 'aria-label': 'ultraspace menu',
    onclick: (e) => {
      const r = e.currentTarget.getBoundingClientRect();
      menu(r.left, r.bottom + 6, [
        { label: 'About ultraspace', icon: 'info', action: () => openApp('settings', { section: 'about' }) },
        { label: 'Settings', icon: 'sliders', action: () => openApp('settings') },
        '-',
        { label: `Sign out ${user.username}`, icon: 'logOut', action: onSignOut },
      ]);
    },
  }, h('span', { class: 'logo-mark' }));

  const stat = (iconName, val, label) => h('button', {
    class: 'mb-item mb-stat', title: label, onclick: () => openApp('system'),
  }, icon(iconName, 14), val);

  const bar = h('header', { class: 'menubar glass' },
    logo, appName,
    h('div', { class: 'mb-spacer' }),
    nowPlaying,
    h('div', { class: 'mb-stats' }, stat('cpu', cpu, 'CPU'), stat('memory', mem, 'Memory'), stat('arrowDown', net, 'Network')),
    h('button', { class: 'mb-item', 'aria-label': 'Search', title: 'Search (Ctrl K)', onclick: openLauncher }, icon('search', 15)),
    h('button', { class: 'mb-item', onclick: () => openApp('settings', { section: 'account' }) }, icon('user', 15), h('span', { class: 'mb-user' }, user.username)),
    clock);

  const fmtTime = new Intl.DateTimeFormat(undefined, { weekday: 'short', day: 'numeric', month: 'short', hour: '2-digit', minute: '2-digit' });
  const tick = () => { clock.textContent = fmtTime.format(new Date()).replace(',', ''); };
  tick();
  const t = setInterval(tick, 10_000);
  cleanup.push(() => clearInterval(t));

  cleanup.push(on('system', (s) => {
    cpu.textContent = `${s.cpu.percent.toFixed(0)}%`;
    mem.textContent = `${((100 * s.memory.used) / s.memory.total).toFixed(0)}%`;
    net.textContent = `${fmtBytes(s.network.rx_rate, 0)}/s`;
  }));
  cleanup.push(on('windows', () => {
    const w = focusedWindow();
    appName.textContent = w ? w.app.name : 'Desktop';
  }));
  cleanup.push(on('music', (m) => {
    nowPlaying.classList.toggle('hidden', !m || !m.track);
    if (m && m.track) nowPlaying.replaceChildren(icon(m.playing ? 'music' : 'pause', 14), h('span', null, m.track.title));
  }));
  return bar;
}

// ---------- dock ----------

function buildDock() {
  const dockEl = h('nav', { class: 'dock glass', 'aria-label': 'Dock' });
  const render = () => {
    const items = allApps().filter((a) => a.dock).map((app) => {
      const wins = windowsOf(app.id);
      return h('button', {
        class: `dock-item ${wins.length ? 'running' : ''}`, 'aria-label': app.name,
        onclick: () => {
          if (!wins.length) { openApp(app.id); return; }
          const w = wins[wins.length - 1];
          if (w.focused && !w.minimized) w.minimize();
          else { w.restore(); w.focus(); }
        },
      },
      h('span', { class: 'dock-tile', style: { background: `linear-gradient(145deg, ${app.color[0]}, ${app.color[1]})` } }, icon(app.icon, 24)),
      h('span', { class: 'dock-label' }, app.name),
      h('span', { class: 'dock-dot' }));
    });
    dockEl.replaceChildren(...items);
  };
  render();
  cleanup.push(on('windows', render));
  return h('div', { class: 'dock-wrap' }, dockEl);
}

// ---------- widgets ----------

function ring(label) {
  const NS = 'http://www.w3.org/2000/svg';
  const svg = document.createElementNS(NS, 'svg');
  svg.setAttribute('viewBox', '0 0 64 64');
  svg.setAttribute('class', 'ring');
  const bg = document.createElementNS(NS, 'circle');
  const fg = document.createElementNS(NS, 'circle');
  for (const c of [bg, fg]) {
    c.setAttribute('cx', '32'); c.setAttribute('cy', '32'); c.setAttribute('r', '26');
    c.setAttribute('fill', 'none'); c.setAttribute('stroke-width', '6');
  }
  bg.setAttribute('class', 'ring-bg');
  fg.setAttribute('class', 'ring-fg');
  const C = 2 * Math.PI * 26;
  fg.setAttribute('stroke-dasharray', C.toFixed(2));
  fg.setAttribute('stroke-dashoffset', C.toFixed(2));
  fg.setAttribute('transform', 'rotate(-90 32 32)');
  svg.append(bg, fg);
  const value = h('span', { class: 'ring-value' }, '–');
  const el = h('div', { class: 'ring-box' }, h('div', { class: 'ring-wrap' }, svg, value), h('span', { class: 'ring-label' }, label));
  return {
    el,
    set(pct, text) {
      fg.setAttribute('stroke-dashoffset', (C * (1 - Math.min(100, Math.max(0, pct)) / 100)).toFixed(2));
      fg.classList.toggle('warn', pct >= 75 && pct < 90);
      fg.classList.toggle('crit', pct >= 90);
      value.textContent = text;
    },
  };
}

function buildWidgets(user) {
  const time = h('div', { class: 'w-time' });
  const date = h('div', { class: 'w-date' });
  const greet = h('div', { class: 'w-greet' });
  const clockCard = h('section', { class: 'widget glass w-clock' }, time, date, greet);
  const tFmt = new Intl.DateTimeFormat(undefined, { hour: '2-digit', minute: '2-digit' });
  const dFmt = new Intl.DateTimeFormat(undefined, { weekday: 'long', day: 'numeric', month: 'long' });
  const tick = () => {
    const now = new Date();
    time.textContent = tFmt.format(now);
    date.textContent = dFmt.format(now);
    const hr = now.getHours();
    greet.textContent = `${hr < 5 ? 'Good night' : hr < 12 ? 'Good morning' : hr < 18 ? 'Good afternoon' : 'Good evening'}, ${user.username}`;
  };
  tick();
  const t = setInterval(tick, 10_000);
  cleanup.push(() => clearInterval(t));

  const rCpu = ring('CPU'), rMem = ring('Memory'), rDisk = ring('Disk');
  const sysLine = h('div', { class: 'w-sub' });
  const sysCard = h('section', { class: 'widget glass w-sys', onclick: () => openApp('system') },
    h('div', { class: 'w-head' }, icon('activity', 15), 'System'),
    h('div', { class: 'rings' }, rCpu.el, rMem.el, rDisk.el), sysLine);

  const canvas = h('canvas', { class: 'w-chart' });
  const rx = h('span'), tx = h('span');
  const netCard = h('section', { class: 'widget glass w-net', onclick: () => openApp('system') },
    h('div', { class: 'w-head' }, icon('arrowDown', 15), 'Network',
      h('span', { class: 'w-net-vals' }, h('span', { class: 'rx' }, icon('arrowDown', 12), rx), h('span', { class: 'tx' }, icon('arrowUp', 12), tx))),
    canvas);

  cleanup.push(on('system', (s) => {
    rCpu.set(s.cpu.percent, `${s.cpu.percent.toFixed(0)}%`);
    const mp = (100 * s.memory.used) / s.memory.total;
    rMem.set(mp, `${mp.toFixed(0)}%`);
    const d = (s.disks || [])[0];
    if (d) { const dp = (100 * d.used) / d.total; rDisk.set(dp, `${dp.toFixed(0)}%`); }
    sysLine.textContent = `${s.hostname} · up ${fmtDuration(s.uptime)} · ${s.cpu.cores} cores`;
    rx.textContent = `${fmtBytes(s.network.rx_rate, 0)}/s`;
    tx.textContent = `${fmtBytes(s.network.tx_rate, 0)}/s`;
    const pts = system.history.slice(-60);
    drawChart(canvas, [
      { values: pts.map((p) => p.rx), color: '#38bdf8', fill: true },
      { values: pts.map((p) => p.tx), color: '#f472b6', fill: false },
    ], { length: 60, gridLines: 2 });
  }));

  return h('div', { class: 'widgets' }, clockCard, sysCard, netCard);
}

// ---------- launcher (Ctrl+K) ----------

let launcherOpen = false;

export function openLauncher() {
  if (launcherOpen) return;
  launcherOpen = true;
  closeMenu();
  const input = h('input', { class: 'launcher-input', placeholder: 'Search apps and files…', 'aria-label': 'Search' });
  const results = h('div', { class: 'launcher-results', role: 'listbox' });
  const box = h('div', { class: 'launcher glass' }, h('div', { class: 'launcher-top' }, icon('search', 20), input,
    h('kbd', null, 'esc')), results);
  const back = h('div', { class: 'launcher-back' }, box);
  let items = [];
  let active = 0;
  let seq = 0;

  const close = () => { back.remove(); launcherOpen = false; };
  back.addEventListener('pointerdown', (e) => { if (e.target === back) close(); });

  const render = () => {
    if (!items.length) {
      results.replaceChildren(h('div', { class: 'launcher-empty' }, input.value ? 'No matches' : 'Type to search'));
      return;
    }
    let lastGroup = '';
    const nodes = [];
    items.forEach((it, i) => {
      if (it.group !== lastGroup) { nodes.push(h('div', { class: 'launcher-group' }, it.group)); lastGroup = it.group; }
      nodes.push(h('button', {
        class: `launcher-item ${i === active ? 'active' : ''}`, role: 'option',
        onpointermove: () => { if (active !== i) { active = i; render(); } },
        onclick: () => { close(); it.run(); },
      }, it.tile, h('span', { class: 'li-text' }, h('span', { class: 'li-title' }, it.title), it.sub ? h('span', { class: 'li-sub' }, it.sub) : null)));
    });
    results.replaceChildren(...nodes);
    results.querySelector('.active')?.scrollIntoView({ block: 'nearest' });
  };

  const appItems = (q) => allApps()
    .filter((a) => a.dock && a.name.toLowerCase().includes(q))
    .map((a) => ({
      group: 'Apps', title: a.name, run: () => openApp(a.id),
      tile: h('span', { class: 'li-tile', style: { background: `linear-gradient(145deg, ${a.color[0]}, ${a.color[1]})` } }, icon(a.icon, 16)),
    }));

  const webItems = (raw) => {
    const t = raw.trim().length >= 2 ? webTarget(raw) : null;
    if (!t || t.error) return [];
    const tile = h('span', { class: 'li-tile', style: { background: 'linear-gradient(145deg, #38bdf8, #0284c7)' } }, icon('globe', 16));
    return t.url
      ? [{ group: 'Web', title: `Open ${t.url.replace(/^https:\/\//, '')}`, sub: 'In Browser', tile, run: () => openApp('browser', { open: raw }) }]
      : [{ group: 'Web', title: `Search the web for “${t.search}”`, sub: 'In Browser', tile, run: () => openApp('browser', { open: raw }) }];
  };

  const searchFiles = debounce(async (q, mySeq) => {
    try {
      const res = await api('GET', `/api/files/search?q=${encodeURIComponent(q)}&limit=30`);
      if (mySeq !== seq) return;
      items = [...appItems(q), ...webItems(input.value), ...res.results.map((f) => ({
        group: 'Files', title: f.name, sub: dirOf(f.path),
        tile: h('span', { class: `li-tile file k-${kindOf(f)}` }, icon(KIND_ICON[kindOf(f)], 16)),
        run: () => (f.dir ? openApp('files', { path: f.path }) : openFile(f, {})),
      }))];
      render();
    } catch (err) { report(err); }
  }, 180);

  input.addEventListener('input', () => {
    const q = input.value.trim().toLowerCase();
    active = 0;
    seq++;
    items = [...appItems(q), ...webItems(input.value)];
    render();
    if (q.length >= 2) searchFiles(q, seq);
  });
  input.addEventListener('keydown', (e) => {
    if (e.key === 'Escape') { close(); }
    else if (e.key === 'ArrowDown') { e.preventDefault(); active = Math.min(items.length - 1, active + 1); render(); }
    else if (e.key === 'ArrowUp') { e.preventDefault(); active = Math.max(0, active - 1); render(); }
    else if (e.key === 'Enter' && items[active]) { close(); items[active].run(); }
  });

  items = appItems('');
  render();
  document.body.append(back);
  input.focus();
}

export { emit };
