// The server-browser engine: a real Chrome runs on the server; this window shows its
// screen (JPEG frames over a WebSocket) and sends mouse, touch and keyboard input back.
import { h, icon, api, report, toast, menu, on, ask, confirmDialog, fmtAgo, debounce } from '../core.js';
import { openApp } from '../wm.js';
import { ENGINES, webTarget, hostOf, siteIcon, data, addHistory } from './web-common.js';

// DOM key names whose events carry no text but matter to the page.
const NAMED_TEXT = { Enter: '\r' };

export function mountRemote(win, args, { swap }) {
  let ws = null;
  let destroyed = false;
  let retry = 0;
  let retryTimer = null;
  let tabs = [];
  let active = null;
  let vp = { w: 1280, h: 800, dpr: 1 };
  let lastHistory = '';
  let state = 'connecting';

  // ---------- layout ----------

  const tabStrip = h('div', { class: 'bw-tabs', role: 'tablist' });
  const addBtn = h('button', { class: 'icon-btn bw-add', 'aria-label': 'New tab', title: 'New tab', onclick: () => send({ t: 'newtab' }) }, icon('plus', 16));
  const backBtn = h('button', { class: 'icon-btn', 'aria-label': 'Back', onclick: () => send({ t: 'back' }) }, icon('chevronLeft'));
  const fwdBtn = h('button', { class: 'icon-btn', 'aria-label': 'Forward', onclick: () => send({ t: 'forward' }) }, icon('chevronRight'));
  const reloadBtn = h('button', { class: 'icon-btn', 'aria-label': 'Reload', onclick: () => send({ t: active?.loading ? 'stop' : 'reload' }) }, icon('refresh', 16));
  const addrIcon = h('span', { class: 'bw-addr-icon' }, icon('search', 14));
  const suggestions = h('datalist', { id: `bw-suggest-${Math.random().toString(36).slice(2)}` });
  const addr = h('input', {
    class: 'bw-addr-input', type: 'text', placeholder: 'Search or type a web address', 'aria-label': 'Address',
    spellcheck: 'false', autocapitalize: 'off', autocomplete: 'off', list: suggestions.id,
  });
  const starBtn = h('button', { class: 'icon-btn', 'aria-label': 'Bookmark this page', title: 'Bookmark', onclick: () => toggleBookmark() }, icon('star', 16));
  const kbdBtn = h('button', { class: 'icon-btn bw-kbd', 'aria-label': 'Keyboard', title: 'Show keyboard', onclick: () => sink.focus() }, icon('terminal', 16));
  const moreBtn = h('button', { class: 'icon-btn', 'aria-label': 'More', onclick: (e) => moreMenu(e) }, icon('moreH', 16));
  const progress = h('div', { class: 'bw-progress' });
  const badge = h('span', { class: 'bw-badge', title: 'Running on your server: logins and tabs follow you to every device' }, icon('server', 12), 'Server');

  const canvas = h('canvas', { class: 'rb-canvas', tabindex: '-1' });
  const ctx2d = canvas.getContext('2d');
  // Keyboard input goes through this invisible textarea, so paste, IME and phone keyboards work.
  const sink = h('textarea', { class: 'rb-sink', 'aria-label': 'Type into the page', autocapitalize: 'off', autocomplete: 'off', spellcheck: 'false' });
  const overlay = h('div', { class: 'rb-overlay hidden' });
  const newTabEl = h('div', { class: 'rb-newtab hidden' });
  const stage = h('div', { class: 'rb-stage' }, canvas, sink, newTabEl, overlay);

  win.body.append(h('div', { class: 'app app-browser' },
    h('div', { class: 'bw-strip' }, tabStrip, addBtn),
    h('div', { class: 'toolbar bw-toolbar' },
      h('div', { class: 'tb-group' }, backBtn, fwdBtn, reloadBtn),
      h('form', { class: 'bw-addr', onsubmit: (e) => { e.preventDefault(); submit(addr.value); } }, addrIcon, addr, badge, suggestions),
      starBtn, kbdBtn, moreBtn, progress),
    stage));

  // ---------- connection ----------

  function connect() {
    if (destroyed) return;
    showOverlay(retry ? 'Reconnecting to your server browser…' : 'Starting the browser on your server…', 'spin');
    const proto = location.protocol === 'https:' ? 'wss' : 'ws';
    ws = new WebSocket(`${proto}://${location.host}/api/remote/ws`);
    ws.binaryType = 'blob';
    ws.onopen = () => { retry = 0; sendViewport(true); };
    ws.onmessage = (e) => (typeof e.data === 'string' ? onMessage(JSON.parse(e.data)) : onFrame(e.data));
    const self = ws;
    ws.onclose = () => {
      if (ws === self) ws = null;
      if (destroyed || paused) return;
      state = 'connecting';
      retry++;
      const wait = Math.min(15000, 500 * 2 ** Math.min(retry, 5));
      showOverlay('Connection lost. Reconnecting…', 'spin');
      retryTimer = setTimeout(connect, wait);
    };
  }

  function send(msg) {
    if (ws && ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify(msg));
  }

  function onMessage(m) {
    switch (m.t) {
      case 'status':
        state = m.state;
        if (m.state === 'ready') hideOverlay();
        else if (m.state === 'starting') showOverlay('Starting the browser on your server…', 'spin');
        break;
      case 'error':
        showOverlay(m.message, 'error');
        break;
      case 'tabs':
        tabs = m.tabs || [];
        active = tabs.find((t) => t.id === m.active) || tabs[0] || null;
        if (m.viewport) vp = m.viewport;
        render();
        break;
      case 'dialog':
        onDialog(m);
        break;
      case 'download':
        if (m.state === 'started') toast(`Downloading ${m.name}…`);
        else if (m.state === 'completed') toast(`Saved ${m.name} to Files → Downloads`);
        else if (m.state === 'canceled') toast(`Download of ${m.name || 'a file'} failed`, 'error');
        break;
      case 'copy':
        navigator.clipboard?.writeText(m.text).then(() => toast('Copied'), () => toast('Copy blocked by this browser', 'error'));
        break;
    }
  }

  let drawing = false;
  let pendingFrame = null;
  async function onFrame(blob) {
    if (drawing) { pendingFrame = blob; return; } // keep only the newest
    drawing = true;
    try {
      const bmp = await createImageBitmap(blob);
      if (canvas.width !== bmp.width || canvas.height !== bmp.height) { canvas.width = bmp.width; canvas.height = bmp.height; layoutCanvas(); }
      ctx2d.drawImage(bmp, 0, 0);
      bmp.close();
    } catch { /* a bad frame; the next one replaces it */ }
    drawing = false;
    if (pendingFrame) { const b = pendingFrame; pendingFrame = null; onFrame(b); }
  }

  // ---------- viewport ----------

  // The page is laid out at the size of this window's content area.
  function wantedViewport() {
    const r = stage.getBoundingClientRect();
    return { w: Math.max(200, Math.floor(r.width)), h: Math.max(150, Math.floor(r.height)), dpr: data.value.sharp ? Math.min(2, Math.max(1, devicePixelRatio || 1)) : 1 };
  }
  function sendViewport(force = false) {
    const want = wantedViewport();
    if (!force && want.w === vp.w && want.h === vp.h && want.dpr === vp.dpr) return;
    send({ t: 'viewport', ...want });
  }
  const resized = debounce(() => { sendViewport(); layoutCanvas(); }, 150);
  const ro = new ResizeObserver(resized);
  ro.observe(stage);

  // Fit the frame inside the stage (another device may have set a different size).
  function layoutCanvas() {
    const r = stage.getBoundingClientRect();
    const scale = Math.min(r.width / vp.w, r.height / vp.h) || 1;
    canvas.style.width = `${Math.round(vp.w * scale)}px`;
    canvas.style.height = `${Math.round(vp.h * scale)}px`;
  }

  function toPage(e) {
    const r = canvas.getBoundingClientRect();
    return {
      x: Math.round(((e.clientX - r.left) / r.width) * vp.w),
      y: Math.round(((e.clientY - r.top) / r.height) * vp.h),
    };
  }

  // ---------- rendering ----------

  function render() {
    tabStrip.replaceChildren(...tabs.map((t) => h('div', {
      class: `bw-tab ${t === active ? 'active' : ''} ${t.asleep ? 'asleep' : ''}`, role: 'tab', 'aria-selected': String(t === active),
      title: t.asleep ? `${t.title || t.url}\nAsleep to save memory: opens again when you click it` : t.url,
      onclick: () => send({ t: 'activate', id: t.id }),
      onauxclick: (e) => { if (e.button === 1) { e.preventDefault(); send({ t: 'close', id: t.id }); } },
    },
    t.loading ? h('span', { class: 'bw-spin' }) : t.asleep ? icon('moon', 13) : isBlank(t.url) ? icon('globe', 14) : siteIcon(t.url, 'xs'),
    h('span', { class: 'bw-tab-title' }, isBlank(t.url) ? 'New tab' : t.title || hostOf(t.url)),
    h('button', { class: 'bw-tab-close', 'aria-label': 'Close tab', onclick: (e) => { e.stopPropagation(); send({ t: 'close', id: t.id }); } }, icon('x', 12)))));
    tabStrip.querySelector('.active')?.scrollIntoView({ block: 'nearest', inline: 'nearest' });
    const t = active;
    const blank = !t || (isBlank(t.url) && !t.asleep);
    if (document.activeElement !== addr) addr.value = blank ? '' : t.url;
    addrIcon.replaceChildren(icon(blank ? 'search' : t.url.startsWith('https:') ? 'lock' : 'info', 14));
    backBtn.disabled = !t?.can_back;
    fwdBtn.disabled = !t?.can_fwd;
    reloadBtn.replaceChildren(icon(t?.loading ? 'x' : 'refresh', 16));
    reloadBtn.setAttribute('aria-label', t?.loading ? 'Stop' : 'Reload');
    starBtn.disabled = blank;
    starBtn.classList.toggle('on', !blank && data.isBookmarked(t.url));
    progress.classList.toggle('on', !!t?.loading);
    win.setTitle(blank ? 'New tab' : t.title || hostOf(t.url));
    newTabEl.classList.toggle('hidden', !blank || state !== 'ready');
    if (blank && state === 'ready' && !newTabEl.childElementCount) renderNewTab();
    layoutCanvas();
    if (t && !blank && !t.loading && t.url !== lastHistory && /^https?:/.test(t.url)) {
      lastHistory = t.url;
      addHistory(t.url, t.title);
    }
  }

  const isBlank = (u) => !u || u === 'about:blank';

  function renderNewTab() {
    const input = h('input', { class: 'input bw-hero-input', type: 'search', placeholder: `Search with ${(ENGINES[data.value.engine] || ENGINES.google).name} or type an address`, 'aria-label': 'Search or address' });
    const marks = data.value.bookmarks;
    const seen = new Set();
    const recent = data.value.history.filter((x) => !seen.has(x.url) && seen.add(x.url)).slice(0, 8);
    newTabEl.replaceChildren(h('div', { class: 'bw-newtab scroll' }, h('div', { class: 'bw-newtab-inner' },
      h('div', { class: 'bw-hero' }, h('span', { class: 'bw-hero-icon' }, icon('globe', 30)),
        h('form', { class: 'bw-hero-form', onsubmit: (e) => { e.preventDefault(); submit(input.value); } }, input)),
      h('section', null, h('div', { class: 'bw-section-title' }, icon('star', 14), 'Bookmarks'),
        marks.length
          ? h('div', { class: 'bw-tiles' }, marks.map((b) => h('button', {
            class: 'bw-tile', title: b.url, onclick: () => open(b.url),
            oncontextmenu: (e) => { e.preventDefault(); menu(e.clientX, e.clientY, [
              { label: 'Open', icon: 'globe', action: () => open(b.url) },
              { label: 'Open in new tab', icon: 'plus', action: () => send({ t: 'newtab', url: b.url }) },
              { label: 'Remove bookmark', icon: 'trash', danger: true, action: () => data.change((d) => { d.bookmarks = d.bookmarks.filter((x) => x.url !== b.url); }) },
            ]); },
          }, siteIcon(b.url, 'lg'), h('span', { class: 'bw-tile-name' }, b.title || hostOf(b.url)))))
          : h('p', { class: 'bw-empty' }, 'Press the star next to the address bar to bookmark a page. Bookmarks follow you to every device.')),
      recent.length ? h('section', null, h('div', { class: 'bw-section-title' }, icon('history', 14), 'Recently visited'),
        h('div', { class: 'bw-recent' }, recent.map((x) => h('button', { class: 'bw-recent-item', onclick: () => open(x.url) },
          siteIcon(x.url, 'xs'), h('span', { class: 'bw-recent-title' }, x.title || hostOf(x.url)),
          h('span', { class: 'bw-recent-host' }, hostOf(x.url)), h('span', { class: 'bw-recent-ago' }, fmtAgo(x.t)))))) : null,
      h('p', { class: 'bw-note' }, icon('server', 13), 'This browser runs on your server. Sites you sign in to stay signed in on every device, and downloads land in Files → Downloads.'))));
  }

  function showOverlay(text, kind) {
    overlay.className = `rb-overlay ${kind}`;
    overlay.replaceChildren(h('div', { class: 'rb-overlay-box' },
      kind === 'spin' ? h('span', { class: 'bw-spin big' }) : icon('info', 26),
      h('div', null, text),
      kind === 'error' ? h('div', { class: 'rb-overlay-actions' },
        h('button', { class: 'btn', onclick: () => { retry = 0; clearTimeout(retryTimer); ws?.close(); connect(); } }, 'Try again'),
        h('button', { class: 'btn', onclick: () => swap('local') }, 'Use light mode')) : null));
  }
  function hideOverlay() { overlay.className = 'rb-overlay hidden'; }

  // ---------- navigation ----------

  function submit(input) {
    const target = webTarget(input);
    if (!target) return;
    if (target.error) { toast(target.error, 'error'); return; }
    open(target.url || (ENGINES[data.value.engine] || ENGINES.google).search(target.search, false));
    addr.blur();
  }

  function open(url) {
    if (active) send({ t: 'navigate', url });
    else send({ t: 'newtab', url });
    canvas.focus({ preventScroll: true });
    sink.focus({ preventScroll: true });
  }

  function toggleBookmark() {
    const t = active;
    if (!t || isBlank(t.url)) return;
    const { url, title } = t;
    const had = data.isBookmarked(url);
    data.change((d) => {
      d.bookmarks = had ? d.bookmarks.filter((b) => b.url !== url) : [...d.bookmarks.filter((b) => b.url !== url), { url, title }];
    }).then(() => { toast(had ? 'Bookmark removed' : 'Bookmarked'); render(); });
  }

  function moreMenu(e) {
    const r = e.currentTarget.getBoundingClientRect();
    const t = active;
    const blank = !t || isBlank(t.url);
    const engine = data.value.engine;
    menu(r.right - 240, r.bottom + 6, [
      { label: 'New tab', icon: 'plus', action: () => send({ t: 'newtab' }) },
      { label: data.isBookmarked(t?.url) ? 'Remove bookmark' : 'Bookmark this page', icon: 'star', disabled: blank, action: toggleBookmark },
      { label: 'Copy link', icon: 'link', disabled: blank, action: () => navigator.clipboard.writeText(t.url).then(() => toast('Link copied'), report) },
      { label: 'Open on this device', icon: 'external', disabled: blank, action: () => window.open(t.url, '_blank', 'noopener,noreferrer') },
      { label: 'Downloads', icon: 'download', action: () => openApp('files', { path: '/Downloads' }) },
      '-',
      { label: data.value.sharp ? 'Sharp text: on' : 'Sharp text: off', icon: data.value.sharp ? 'check' : 'monitor', hint: 'uses more data',
        action: () => data.change((d) => { d.sharp = !d.sharp; }).then(() => sendViewport(true)) },
      ...Object.entries(ENGINES).map(([id, en]) => ({
        label: `Search with ${en.name}`, icon: id === engine ? 'check' : null, action: () => data.change((d) => { d.engine = id; }),
      })),
      '-',
      { label: 'Use light mode on this device', icon: 'globe', action: () => { data.change((d) => { d.mode = 'light'; }); swap('local'); } },
      { label: 'Shut down server browser', icon: 'power', danger: true, action: shutdown },
      { label: 'Clear history', icon: 'history', danger: true, disabled: !data.value.history.length, action: () => data.change((d) => { d.history = []; }) },
    ]);
  }

  async function shutdown() {
    if (!(await confirmDialog('Shut down the server browser?', 'This frees its memory on the server. Your tabs and logins are kept and come back next time.', 'Shut down'))) return;
    destroyed = true;
    clearTimeout(retryTimer);
    ws?.close();
    await api('POST', '/api/remote/stop').catch(report);
    win.close();
  }

  async function onDialog(m) {
    const where = hostOf(m.url || active?.url || '');
    if (m.type === 'prompt') {
      const v = await ask(`${where} asks`, m.message, m.default || '', 'OK');
      send({ t: 'dialog', session: m.session, accept: v !== null, text: v ?? '' });
    } else if (m.type === 'alert') {
      await confirmDialog(`${where} says`, m.message, 'OK', false);
      send({ t: 'dialog', session: m.session, accept: true });
    } else {
      const ok = await confirmDialog(m.type === 'beforeunload' ? 'Leave this page?' : `${where} asks`, m.message || 'Changes you made may not be saved.', m.type === 'beforeunload' ? 'Leave' : 'OK', false);
      send({ t: 'dialog', session: m.session, accept: ok });
    }
  }

  // ---------- mouse ----------

  const BUTTON = ['left', 'middle', 'right', 'back', 'forward'];
  const mods = (e) => (e.altKey ? 1 : 0) | (e.ctrlKey || e.metaKey ? 2 : 0) | (e.shiftKey ? 8 : 0);
  let moveQueued = null;

  canvas.addEventListener('mousedown', (e) => {
    e.preventDefault();
    sink.focus({ preventScroll: true });
    sendViewport();
    send({ t: 'mouse', type: 'mousePressed', ...toPage(e), button: BUTTON[e.button] || 'left', buttons: e.buttons, clickCount: e.detail || 1, mods: mods(e) });
  });
  canvas.addEventListener('mouseup', (e) => {
    send({ t: 'mouse', type: 'mouseReleased', ...toPage(e), button: BUTTON[e.button] || 'left', buttons: e.buttons, clickCount: e.detail || 1, mods: mods(e) });
  });
  canvas.addEventListener('mousemove', (e) => {
    const first = !moveQueued;
    moveQueued = { t: 'mouse', type: 'mouseMoved', ...toPage(e), button: e.buttons & 1 ? 'left' : 'none', buttons: e.buttons, mods: mods(e) };
    if (first) requestAnimationFrame(() => { send(moveQueued); moveQueued = null; });
  });
  canvas.addEventListener('contextmenu', (e) => e.preventDefault());
  canvas.addEventListener('wheel', (e) => {
    e.preventDefault();
    const k = e.deltaMode === 1 ? 40 : e.deltaMode === 2 ? 800 : 1;
    send({ t: 'wheel', ...toPage(e), dx: e.deltaX * k, dy: e.deltaY * k, mods: mods(e) });
  }, { passive: false });

  // ---------- touch: tap = click, drag = scroll ----------

  let touch = null;
  canvas.addEventListener('pointerdown', (e) => {
    if (e.pointerType === 'mouse') return;
    e.preventDefault();
    canvas.setPointerCapture(e.pointerId);
    touch = { id: e.pointerId, x: e.clientX, y: e.clientY, lx: e.clientX, ly: e.clientY, moved: false };
  });
  canvas.addEventListener('pointermove', (e) => {
    if (!touch || e.pointerId !== touch.id) return;
    const dx = e.clientX - touch.lx, dy = e.clientY - touch.ly;
    if (!touch.moved && Math.hypot(e.clientX - touch.x, e.clientY - touch.y) < 8) return;
    touch.moved = true;
    touch.lx = e.clientX; touch.ly = e.clientY;
    const scale = vp.w / canvas.getBoundingClientRect().width;
    send({ t: 'wheel', ...toPage(e), dx: -dx * scale, dy: -dy * scale, mods: 0 });
  });
  const endTouch = (e) => {
    if (!touch || e.pointerId !== touch.id) return;
    if (!touch.moved && e.type === 'pointerup') {
      const p = toPage(e);
      sendViewport();
      send({ t: 'mouse', type: 'mousePressed', ...p, button: 'left', buttons: 1, clickCount: 1, mods: 0 });
      send({ t: 'mouse', type: 'mouseReleased', ...p, button: 'left', buttons: 0, clickCount: 1, mods: 0 });
    }
    touch = null;
  };
  canvas.addEventListener('pointerup', endTouch);
  canvas.addEventListener('pointercancel', endTouch);

  // ---------- keyboard ----------

  const key = (type, e, text) => send({
    t: 'key', type, key: e.key, code: e.code, keyCode: e.keyCode || 0, text: text || '', mods: mods(e), location: e.location || 0, repeat: !!e.repeat,
  });
  const sent = new Set();

  sink.addEventListener('keydown', (e) => {
    if (e.isComposing || e.keyCode === 229) return; // IME / phone keyboard: handled on input
    const cmd = e.ctrlKey || e.metaKey;
    const k = e.key.toLowerCase();
    // Shortcuts for the Browser itself.
    if (cmd && k === 'l') { e.preventDefault(); addr.focus(); return; }
    if ((cmd && k === 'r') || e.key === 'F5') { e.preventDefault(); send({ t: 'reload' }); return; }
    if (e.altKey && e.key === 'ArrowLeft') { e.preventDefault(); send({ t: 'back' }); return; }
    if (e.altKey && e.key === 'ArrowRight') { e.preventDefault(); send({ t: 'forward' }); return; }
    if (cmd && k === 'v') return; // let the paste event deliver the clipboard
    e.preventDefault();
    sent.add(e.code);
    // A keyDown with text types it; rawKeyDown is a key press with no character.
    if (e.key.length === 1 && !cmd) key('keyDown', e, e.key);
    else if (NAMED_TEXT[e.key] && !cmd) key('keyDown', e, NAMED_TEXT[e.key]);
    else key('rawKeyDown', e);
    if (cmd && (k === 'c' || k === 'x')) setTimeout(() => send({ t: 'copy' }), 80);
  });
  sink.addEventListener('keyup', (e) => {
    if (!sent.delete(e.code)) return;
    e.preventDefault();
    key('keyUp', e);
  });
  sink.addEventListener('paste', (e) => {
    e.preventDefault();
    const text = e.clipboardData?.getData('text/plain');
    if (text) send({ t: 'text', text });
  });
  sink.addEventListener('input', (e) => {
    if (e.inputType === 'deleteContentBackward') {
      const ev = { key: 'Backspace', code: 'Backspace', keyCode: 8 };
      key('rawKeyDown', ev); key('keyUp', ev);
    } else if (e.data && !e.isComposing) {
      send({ t: 'text', text: e.data });
    }
    sink.value = '';
  });
  sink.addEventListener('compositionend', (e) => { if (e.data) send({ t: 'text', text: e.data }); sink.value = ''; });

  // ---------- address bar ----------

  const suggest = debounce(() => {
    const q = addr.value.trim().toLowerCase();
    const seen = new Set();
    const hits = [...data.value.bookmarks, ...data.value.history]
      .filter((x) => (x.url.toLowerCase().includes(q) || (x.title || '').toLowerCase().includes(q)) && !seen.has(x.url) && seen.add(x.url)).slice(0, 12);
    suggestions.replaceChildren(...hits.map((x) => h('option', { value: x.url }, x.title || '')));
  }, 120);
  addr.addEventListener('input', suggest);
  addr.addEventListener('focus', () => { addr.select(); suggest(); });
  addr.addEventListener('keydown', (e) => { if (e.key === 'Escape') { render(); sink.focus(); } });

  const off = on('browser-data', () => { newTabEl.replaceChildren(); render(); });

  // A tab left in the background stops streaming after a minute, so the server can
  // shut Chrome down when nobody is looking; it reconnects on return.
  let pauseTimer = null;
  let paused = false;
  const onVisibility = () => {
    if (document.hidden) {
      pauseTimer = setTimeout(() => { paused = true; clearTimeout(retryTimer); const s = ws; ws = null; s?.close(); }, 60_000);
    } else {
      clearTimeout(pauseTimer);
      if (paused) { paused = false; retry = 0; connect(); }
    }
  };
  document.addEventListener('visibilitychange', onVisibility);

  connect();
  if (args.open) setTimeout(() => submit(args.open), 600);

  return {
    onArgs(a) { if (a.open) { const t = webTarget(a.open); if (t?.url) send({ t: 'newtab', url: t.url }); else if (t?.search) send({ t: 'newtab', url: (ENGINES[data.value.engine] || ENGINES.google).search(t.search, false) }); } },
    onFocus() { sendViewport(); data.refresh(); if (document.activeElement !== addr) sink.focus({ preventScroll: true }); },
    onResize: resized,
    onDestroy() {
      destroyed = true;
      clearTimeout(retryTimer);
      clearTimeout(pauseTimer);
      document.removeEventListener('visibilitychange', onVisibility);
      ro.disconnect();
      off();
      ws?.close();
    },
    saveState: () => ({}),
  };
}
