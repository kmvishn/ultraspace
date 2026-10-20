// Browser. Two engines:
//  - remote (default when installed): a real Chrome on the server, streamed into the window.
//    Logins, tabs and history live on the server and follow you to every device.
//  - light: pages load in a sandboxed frame on this device; no server memory, but many
//    sites refuse to be framed.
import { h, icon, api, report, toast, menu, emit, on, fmtAgo, debounce } from '../core.js';
import { registerApp } from '../wm.js';
import { ENGINES, webTarget, hostOf, siteIcon, data, addHistory } from './web-common.js';
import { mountRemote } from './browser-remote.js';

export { webTarget };

registerApp({
  id: 'browser', name: 'Browser', icon: 'globe', color: ['#38bdf8', '#0284c7'], width: 1100, height: 720, minWidth: 480,
  mount(win, args) { return mountBrowser(win, args); },
});

const SANDBOX = 'allow-scripts allow-same-origin allow-forms allow-popups allow-popups-to-escape-sandbox allow-downloads allow-modals allow-presentation';
const ALLOW = 'fullscreen; autoplay; clipboard-write; encrypted-media; picture-in-picture';

let remoteStatus = null;

// Picks the engine, then mounts it. A swap (from the menu) remounts in the same window.
function mountBrowser(win, args) {
  let inner = null;
  let mode = null;
  const handle = {
    onArgs: (a) => inner?.onArgs?.(a),
    onFocus: () => inner?.onFocus?.(),
    onResize: () => inner?.onResize?.(),
    onDestroy: () => inner?.onDestroy?.(),
    saveState: () => ({ ...(inner?.saveState?.() || args.restore || {}), mode }),
  };
  const swap = (next, a = {}) => {
    inner?.onDestroy?.();
    win.body.replaceChildren();
    win.actionsEl.replaceChildren();
    mode = next;
    inner = next === 'remote' ? mountRemote(win, a, { swap }) : mountLocal(win, a, { swap });
  };
  (async () => {
    await data.refresh();
    if (!remoteStatus) remoteStatus = await api('GET', '/api/remote/status').catch(() => ({ available: false }));
    const want = data.value.mode || 'auto';
    swap(remoteStatus.available && want !== 'light' ? 'remote' : 'local', args);
  })();
  return handle;
}

export const remoteAvailable = () => !!remoteStatus?.available;
// ---------- the app ----------

let nextTab = 1;

function mountLocal(win, args, { swap }) {
  const tabs = [];
  let active = null;

  const tabStrip = h('div', { class: 'bw-tabs', role: 'tablist' });
  const addBtn = h('button', { class: 'icon-btn bw-add', 'aria-label': 'New tab', title: 'New tab', onclick: () => activate(newTab(null)) }, icon('plus', 16));
  const backBtn = h('button', { class: 'icon-btn', 'aria-label': 'Back', onclick: () => step(-1) }, icon('chevronLeft'));
  const fwdBtn = h('button', { class: 'icon-btn', 'aria-label': 'Forward', onclick: () => step(1) }, icon('chevronRight'));
  const reloadBtn = h('button', { class: 'icon-btn', 'aria-label': 'Reload', onclick: () => active && load(active) }, icon('refresh', 16));
  const addrIcon = h('span', { class: 'bw-addr-icon' });
  const suggestions = h('datalist', { id: `bw-suggest-${Math.random().toString(36).slice(2)}` });
  const addr = h('input', {
    class: 'bw-addr-input', type: 'text', placeholder: 'Search or type a web address', 'aria-label': 'Address',
    spellcheck: 'false', autocapitalize: 'off', autocomplete: 'off', list: suggestions.id,
  });
  const starBtn = h('button', { class: 'icon-btn', 'aria-label': 'Bookmark this page', title: 'Bookmark', onclick: () => toggleBookmark() }, icon('star', 16));
  const extBtn = h('button', { class: 'icon-btn bw-ext', 'aria-label': 'Open in a new browser tab', title: 'Open in a new browser tab', onclick: () => openOutside(active?.url) }, icon('external', 16));
  const moreBtn = h('button', { class: 'icon-btn', 'aria-label': 'More', onclick: (e) => moreMenu(e) }, icon('moreH', 16));
  const progress = h('div', { class: 'bw-progress' });
  const pages = h('div', { class: 'bw-pages' });

  win.body.append(h('div', { class: 'app app-browser' },
    h('div', { class: 'bw-strip' }, tabStrip, addBtn),
    h('div', { class: 'toolbar bw-toolbar' },
      h('div', { class: 'tb-group' }, backBtn, fwdBtn, reloadBtn),
      h('form', { class: 'bw-addr', onsubmit: (e) => { e.preventDefault(); submit(addr.value); } }, addrIcon, addr, suggestions),
      starBtn, extBtn, moreBtn, progress),
    pages));

  // ---------- tabs ----------

  function newTab(url, title = '') {
    const tab = { id: nextTab++, url: null, title, entries: [], pos: -1, el: h('div', { class: 'bw-page hidden' }), state: 'new', seq: 0, pending: null };
    tabs.push(tab);
    pages.append(tab.el);
    if (url) { tab.pending = url; tab.title = title || hostOf(url); } else showNewTab(tab);
    return tab;
  }

  function activate(tab) {
    active = tab;
    for (const t of tabs) t.el.classList.toggle('hidden', t !== tab);
    if (tab.pending) { const u = tab.pending; tab.pending = null; go(tab, u); }
    render();
    if (!tab.url) requestAnimationFrame(() => tab.el.querySelector('.bw-hero-input')?.focus());
  }

  function closeTab(tab) {
    const i = tabs.indexOf(tab);
    tabs.splice(i, 1);
    tab.el.remove();
    if (!tabs.length) { win.close(); return; }
    if (active === tab) activate(tabs[Math.min(i, tabs.length - 1)]);
    else render();
  }

  function render() {
    tabStrip.replaceChildren(...tabs.map((t) => h('div', {
      class: `bw-tab ${t === active ? 'active' : ''}`, role: 'tab', 'aria-selected': String(t === active), title: t.url || 'New tab',
      onclick: () => activate(t),
      onauxclick: (e) => { if (e.button === 1) { e.preventDefault(); closeTab(t); } },
    },
    t.state === 'loading' ? h('span', { class: 'bw-spin' }) : t.url ? siteIcon(t.url, 'xs') : icon('globe', 14),
    h('span', { class: 'bw-tab-title' }, t.url ? t.title || hostOf(t.url) : 'New tab'),
    h('button', { class: 'bw-tab-close', 'aria-label': 'Close tab', onclick: (e) => { e.stopPropagation(); closeTab(t); } }, icon('x', 12)))));
    tabStrip.querySelector('.active')?.scrollIntoView({ block: 'nearest', inline: 'nearest' });
    const t = active;
    if (!t) return;
    if (document.activeElement !== addr) addr.value = t.url || '';
    addrIcon.replaceChildren(icon(!t.url ? 'search' : t.url.startsWith('https:') ? 'lock' : 'info', 14));
    backBtn.disabled = t.pos <= 0 && !(t.pos === 0 && t.url);
    fwdBtn.disabled = t.pos >= t.entries.length - 1;
    reloadBtn.disabled = !t.url;
    starBtn.disabled = extBtn.disabled = !t.url;
    starBtn.classList.toggle('on', !!t.url && data.isBookmarked(t.url));
    progress.classList.toggle('on', t.state === 'loading');
    win.setTitle(t.url ? t.title || hostOf(t.url) : 'New tab');
  }

  // ---------- navigation ----------

  function submit(input) {
    const target = webTarget(input, { embed: true });
    if (!target || !active) return;
    if (target.error) { toast(target.error, 'error'); return; }
    const url = target.url || (ENGINES[data.value.engine] || ENGINES.google).search(target.search, true);
    addr.blur();
    go(active, url);
  }

  function go(tab, url, { push = true } = {}) {
    if (hostOf(url) === location.hostname) { toast('That is ultraspace itself — use its apps directly.', 'error'); return; }
    if (push) {
      tab.entries = tab.entries.slice(0, tab.pos + 1);
      tab.entries.push(url);
      tab.pos = tab.entries.length - 1;
    }
    tab.url = url;
    tab.title = hostOf(url);
    load(tab);
  }

  function step(d) {
    const t = active;
    if (!t) return;
    if (d < 0 && t.pos === 0) { // back from the first page goes to the new-tab page
      t.pos = -1; t.url = null; t.state = 'new'; showNewTab(t); render(); return;
    }
    const p = t.pos + d;
    if (p < 0 || p >= t.entries.length) return;
    t.pos = p;
    go(t, t.entries[p], { push: false });
  }

  // A fresh frame for every page, so frame navigations never land in the desktop's own history.
  function load(tab, { force = false } = {}) {
    const url = tab.url;
    const seq = ++tab.seq;
    if (url.startsWith('http:') && !force) { showBlocked(tab, 'insecure'); probe(tab, seq); return; }
    const frame = h('iframe', {
      class: 'bw-frame', src: url, sandbox: SANDBOX, allow: ALLOW, referrerpolicy: 'strict-origin-when-cross-origin',
      title: hostOf(url),
    });
    frame.addEventListener('load', () => {
      if (seq !== tab.seq || tab.state === 'blocked') return;
      tab.state = 'ready';
      render();
    });
    tab.state = 'loading';
    tab.el.replaceChildren(frame);
    render();
    if (!force) probe(tab, seq);
  }

  async function probe(tab, seq) {
    const url = tab.url;
    try {
      const res = await api('GET', `/api/browser/probe?url=${enc(url)}`);
      if (seq !== tab.seq) return;
      if (res.title) tab.title = res.title;
      if (!res.embeddable && tab.state !== 'blocked') showBlocked(tab, res.reason);
      if (!res.error) addHistory(url, tab.title);
      render();
    } catch (err) { if (seq === tab.seq) report(err); }
  }

  function showBlocked(tab, reason) {
    tab.state = 'blocked';
    const host = hostOf(tab.url);
    const insecure = reason === 'insecure';
    tab.el.replaceChildren(h('div', { class: 'bw-blocked' },
      h('div', { class: 'bw-blocked-icon' }, icon(insecure ? 'lock' : 'shieldOff', 30)),
      h('h2', null, `${host} can't be shown inside ultraspace`),
      h('p', null, insecure
        ? 'This page is not served over HTTPS, and browsers refuse to show insecure pages inside a secure one.'
        : 'This site tells browsers not to let other pages embed it. Many big sites do this (GitHub, YouTube’s main site, banks, most sign-in pages).'),
      h('div', { class: 'bw-blocked-actions' },
        h('button', { class: 'btn primary', onclick: () => openOutside(tab.url) }, icon('external', 15), 'Open in a new browser tab'),
        insecure ? null : h('button', { class: 'btn', onclick: () => { tab.state = 'loading'; load(tab, { force: true }); } }, 'Try anyway')),
      h('div', { class: 'bw-blocked-url' }, tab.url)));
    render();
  }

  function showNewTab(tab) {
    const input = h('input', { class: 'input bw-hero-input', type: 'search', placeholder: `Search with ${(ENGINES[data.value.engine] || ENGINES.google).name} or type an address`, 'aria-label': 'Search or address' });
    const marks = data.value.bookmarks;
    const seen = new Set();
    const recent = data.value.history.filter((x) => !seen.has(x.url) && seen.add(x.url)).slice(0, 8);
    tab.el.replaceChildren(h('div', { class: 'bw-newtab scroll' }, h('div', { class: 'bw-newtab-inner' },
      h('div', { class: 'bw-hero' }, h('span', { class: 'bw-hero-icon' }, icon('globe', 30)),
        h('form', { class: 'bw-hero-form', onsubmit: (e) => { e.preventDefault(); submit(input.value); } }, input)),
      h('section', null, h('div', { class: 'bw-section-title' }, icon('star', 14), 'Bookmarks'),
        marks.length
          ? h('div', { class: 'bw-tiles' }, marks.map((b) => h('button', {
            class: 'bw-tile', title: b.url, onclick: () => go(tab, b.url),
            oncontextmenu: (e) => { e.preventDefault(); menu(e.clientX, e.clientY, [
              { label: 'Open', icon: 'globe', action: () => go(tab, b.url) },
              { label: 'Open in new tab', icon: 'plus', action: () => activate(newTab(b.url, b.title)) },
              { label: 'Remove bookmark', icon: 'trash', danger: true, action: () => data.change((d) => { d.bookmarks = d.bookmarks.filter((x) => x.url !== b.url); }) },
            ]); },
          }, siteIcon(b.url, 'lg'), h('span', { class: 'bw-tile-name' }, b.title || hostOf(b.url)))))
          : h('p', { class: 'bw-empty' }, 'Press the star next to the address bar to bookmark a page. Bookmarks follow you to every device.')),
      recent.length ? h('section', null, h('div', { class: 'bw-section-title' }, icon('history', 14), 'Recently visited'),
        h('div', { class: 'bw-recent' }, recent.map((x) => h('button', { class: 'bw-recent-item', onclick: () => go(tab, x.url) },
          siteIcon(x.url, 'xs'), h('span', { class: 'bw-recent-title' }, x.title || hostOf(x.url)),
          h('span', { class: 'bw-recent-host' }, hostOf(x.url)), h('span', { class: 'bw-recent-ago' }, fmtAgo(x.t)))))) : null,
      h('p', { class: 'bw-note' }, icon('info', 13), 'Pages open inside your desktop when the site allows it; others get a button to open them in a normal tab. Sites you sign in to here keep that sign-in only on this device.'))));
  }

  // ---------- actions ----------

  function openOutside(url) {
    if (url) window.open(url, '_blank', 'noopener,noreferrer');
  }

  function toggleBookmark() {
    const t = active;
    if (!t?.url) return;
    const url = t.url, title = t.title;
    const had = data.isBookmarked(url);
    data.change((d) => {
      d.bookmarks = had ? d.bookmarks.filter((b) => b.url !== url) : [...d.bookmarks.filter((b) => b.url !== url), { url, title }];
    }).then(() => toast(had ? 'Bookmark removed' : 'Bookmarked'));
  }

  function moreMenu(e) {
    const r = e.currentTarget.getBoundingClientRect();
    const t = active;
    const engine = data.value.engine;
    menu(r.right - 220, r.bottom + 6, [
      { label: 'New tab', icon: 'plus', action: () => activate(newTab(null)) },
      { label: data.isBookmarked(t?.url) ? 'Remove bookmark' : 'Bookmark this page', icon: 'star', disabled: !t?.url, action: toggleBookmark },
      { label: 'Copy link', icon: 'link', disabled: !t?.url, action: () => navigator.clipboard.writeText(t.url).then(() => toast('Link copied'), report) },
      { label: 'Open in a new browser tab', icon: 'external', disabled: !t?.url, action: () => openOutside(t.url) },
      '-',
      ...Object.entries(ENGINES).map(([id, en]) => ({
        label: `Search with ${en.name}`, icon: id === engine ? 'check' : null, action: () => data.change((d) => { d.engine = id; }),
      })),
      '-',
      remoteAvailable() ? { label: 'Switch to the server browser', icon: 'server', action: () => { data.change((d) => { d.mode = 'auto'; }); swap('remote'); } } : null,
      { label: 'Clear history', icon: 'history', danger: true, disabled: !data.value.history.length, action: () => data.change((d) => { d.history = []; }) },
    ]);
  }

  // ---------- address bar suggestions ----------

  const suggest = debounce(() => {
    const q = addr.value.trim().toLowerCase();
    const pool = [...data.value.bookmarks, ...data.value.history];
    const seen = new Set();
    const hits = pool.filter((x) => (x.url.toLowerCase().includes(q) || (x.title || '').toLowerCase().includes(q)) && !seen.has(x.url) && seen.add(x.url)).slice(0, 12);
    suggestions.replaceChildren(...hits.map((x) => h('option', { value: x.url }, x.title || '')));
  }, 120);
  addr.addEventListener('input', suggest);
  addr.addEventListener('focus', () => { addr.select(); suggest(); });
  addr.addEventListener('keydown', (e) => { if (e.key === 'Escape') { addr.value = active?.url || ''; addr.blur(); } });

  win.body.addEventListener('keydown', (e) => {
    if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 'l') { e.preventDefault(); addr.focus(); }
    else if (e.altKey && e.key === 'ArrowLeft') { e.preventDefault(); step(-1); }
    else if (e.altKey && e.key === 'ArrowRight') { e.preventDefault(); step(1); }
  });

  const off = on('browser-data', () => {
    // Refresh new-tab pages, but never under someone who is typing in one.
    for (const t of tabs) {
      const input = t.el.querySelector('.bw-hero-input');
      if (!t.url && (!input || (!input.value && document.activeElement !== input))) showNewTab(t);
    }
    render();
  });

  // ---------- start ----------

  const r = args.restore;
  if (r?.tabs?.length) {
    for (const t of r.tabs.slice(0, 30)) newTab(typeof t.url === 'string' && webTarget(t.url, { embed: true })?.url ? t.url : null, t.title || '');
    activate(tabs[Math.min(Math.max(0, r.active | 0), tabs.length - 1)]);
  } else {
    activate(newTab(null));
    if (args.open) submit(args.open);
  }
  data.refresh();

  return {
    onArgs(a) { if (a.open) { activate(newTab(null)); submit(a.open); } },
    onFocus() { data.refresh(); },
    onDestroy() { off(); },
    saveState: () => ({
      tabs: tabs.map((t) => ({ url: t.url || t.pending || null, title: t.title })),
      active: Math.max(0, tabs.indexOf(active)),
    }),
  };
}
