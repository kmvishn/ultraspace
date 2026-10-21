// Terminal: shells that run on the server as your Linux user. Sessions keep running
// when the window closes or the device sleeps; reopening reattaches with recent output.
import { h, icon, api, report, toast, menu, confirmDialog, debounce } from '../core.js';
import { registerApp } from '../wm.js';
import { share } from '../clipboard.js';

registerApp({
  id: 'terminal', name: 'Terminal', icon: 'terminal', color: ['#64748b', '#0f172a'], width: 900, height: 560, minWidth: 420,
  mount(win, args) { return mountTerminal(win, args); },
});

// xterm.js (~350 KB) loads the first time a Terminal opens, not with the desktop.
let Terminal, FitAddon, WebLinksAddon;
let libs = null;
function loadXterm() {
  libs ||= Promise.all([
    import('../../vendor/xterm/xterm.mjs'),
    import('../../vendor/xterm/addon-fit.mjs'),
    import('../../vendor/xterm/addon-web-links.mjs'),
  ]).then(([x, f, l]) => {
    ({ Terminal } = x); ({ FitAddon } = f); ({ WebLinksAddon } = l);
    document.head.append(h('link', { rel: 'stylesheet', href: 'vendor/xterm/xterm.css' }));
  });
  return libs;
}

const THEME = {
  background: '#101015', foreground: '#e4e4ec', cursor: '#c4b5fd', cursorAccent: '#101015', selectionBackground: '#7c6cf255',
  black: '#1e1e26', red: '#f87171', green: '#4ade80', yellow: '#facc15', blue: '#60a5fa', magenta: '#c084fc', cyan: '#22d3ee', white: '#e4e4ec',
  brightBlack: '#6b6b7b', brightRed: '#fca5a5', brightGreen: '#86efac', brightYellow: '#fde047', brightBlue: '#93c5fd', brightMagenta: '#d8b4fe', brightCyan: '#67e8f9', brightWhite: '#ffffff',
};

const enc = new TextEncoder();
let toldKeepsRunning = false;

function mountTerminal(win, args) {
  const tabs = []; // { id, title, term, fit, el, ws, exited, retry, timer }
  let active = null;
  let destroyed = false;
  let ctrlSticky = false;

  const strip = h('div', { class: 'bw-tabs', role: 'tablist' });
  const addBtn = h('button', { class: 'icon-btn bw-add', 'aria-label': 'New terminal', title: 'New terminal', onclick: () => create() }, icon('plus', 16));
  const panes = h('div', { class: 'term-panes' });
  const ctrlKey = h('button', { class: 'term-key', onclick: () => { ctrlSticky = !ctrlSticky; ctrlKey.classList.toggle('on', ctrlSticky); active?.term.focus(); } }, 'Ctrl');
  const keys = h('div', { class: 'term-keys' },
    [['Esc', '\x1b'], ['Tab', '\t']].map(([l, s]) => keyBtn(l, s)), ctrlKey,
    [['↑', '\x1b[A'], ['↓', '\x1b[B'], ['←', '\x1b[D'], ['→', '\x1b[C'], ['|', '|'], ['~', '~'], ['/', '/'], ['-', '-']].map(([l, s]) => keyBtn(l, s)));
  const empty = h('div', { class: 'empty-state hidden' });

  win.body.append(h('div', { class: 'app app-terminal' },
    h('div', { class: 'bw-strip term-strip' }, strip, addBtn), panes, empty, keys));

  function keyBtn(label, seq) {
    return h('button', { class: 'term-key', onclick: () => { sendInput(seq); active?.term.focus(); } }, label);
  }

  // ---------- sessions ----------

  async function start() {
    try {
      await loadXterm();
      const st = await api('GET', '/api/agent/status');
      if (!st.available) {
        showEmpty('The terminal service is not running on this server.', 'It is installed with ultraspace as the "ultraspace-agent" service.');
        return;
      }
      const { sessions } = await api('GET', '/api/term/sessions');
      for (const s of sessions.filter((x) => !x.exited)) addTab(s.id);
      if (!tabs.length) await create();
      else activate(tabs.find((t) => t.id === args.restore?.active) || tabs[tabs.length - 1]);
    } catch (err) {
      showEmpty('Could not reach the terminal service.', err.message);
    }
  }

  function showEmpty(title, sub) {
    empty.classList.remove('hidden');
    empty.replaceChildren(icon('terminal', 40), h('div', { class: 'empty-title' }, title), h('div', { class: 'empty-sub' }, sub));
  }

  async function create() {
    await loadXterm();
    const size = guessSize();
    try {
      const s = await api('POST', '/api/term/sessions', size);
      activate(addTab(s.id));
    } catch (err) { report(err); }
  }

  function guessSize() {
    const r = panes.getBoundingClientRect();
    return { cols: Math.max(20, Math.floor((r.width - 16) / 8.1)), rows: Math.max(5, Math.floor((r.height - 12) / 18)) };
  }

  function addTab(id) {
    const el = h('div', { class: 'term-pane hidden' });
    panes.append(el);
    const term = new Terminal({
      fontFamily: '"JetBrains Mono", "Fira Code", ui-monospace, SFMono-Regular, Menlo, Consolas, "DejaVu Sans Mono", monospace',
      fontSize: 13.5, lineHeight: 1.15, cursorBlink: true, scrollback: 5000, theme: THEME, allowProposedApi: false,
      macOptionIsMeta: true, rightClickSelectsWord: true,
    });
    const fit = new FitAddon();
    term.loadAddon(fit);
    term.loadAddon(new WebLinksAddon((e, uri) => window.open(uri, '_blank', 'noopener,noreferrer')));
    const tab = { id, title: 'Terminal', term, fit, el, ws: null, exited: false, retry: 0, timer: null, opened: false };
    term.onData((d) => sendInput(d, tab));
    term.onBinary((d) => tab.ws?.readyState === 1 && tab.ws.send(Uint8Array.from(d, (c) => c.charCodeAt(0))));
    term.onTitleChange((t) => { tab.title = t || 'Terminal'; render(); });
    term.onResize(({ cols, rows }) => tab.ws?.readyState === 1 && tab.ws.send(JSON.stringify({ t: 'resize', cols, rows })));
    // Ctrl+Shift+C / V copy and paste, like desktop terminals (plain Ctrl+C is interrupt).
    term.attachCustomKeyEventHandler((e) => {
      if (e.type !== 'keydown' || !e.ctrlKey || !e.shiftKey) return true;
      if (e.code === 'KeyC') { copySelection(tab); return false; }
      if (e.code === 'KeyV') { paste(tab); return false; }
      return true;
    });
    el.addEventListener('contextmenu', (e) => {
      e.preventDefault();
      menu(e.clientX, e.clientY, [
        { label: 'Copy', icon: 'fileText', hint: 'Ctrl Shift C', disabled: !term.hasSelection(), action: () => copySelection(tab) },
        { label: 'Paste', icon: 'download', hint: 'Ctrl Shift V', action: () => paste(tab) },
        { label: 'Clear', icon: 'refresh', action: () => term.clear() },
        '-',
        { label: 'New terminal', icon: 'plus', action: () => create() },
        { label: 'End this session', icon: 'x', danger: true, action: () => endSession(tab) },
      ]);
    });
    tabs.push(tab);
    render();
    return tab;
  }

  function connect(tab) {
    if (destroyed || tab.exited) return;
    const proto = location.protocol === 'https:' ? 'wss' : 'ws';
    const ws = new WebSocket(`${proto}://${location.host}/api/term/sessions/${encodeURIComponent(tab.id)}/ws`);
    ws.binaryType = 'arraybuffer';
    tab.ws = ws;
    let first = true;
    ws.onopen = () => {
      tab.retry = 0;
      ws.send(JSON.stringify({ t: 'resize', cols: tab.term.cols, rows: tab.term.rows }));
    };
    ws.onmessage = (e) => {
      if (typeof e.data === 'string') {
        if (JSON.parse(e.data).t === 'exit') sessionEnded(tab);
        return;
      }
      if (first) { tab.term.reset(); first = false; } // the server replays recent output on every attach
      tab.term.write(new Uint8Array(e.data));
    };
    ws.onclose = (e) => {
      if (tab.ws === ws) tab.ws = null;
      if (destroyed || tab.exited) return;
      if (e.code === 1008 || e.code === 4404) { sessionEnded(tab); return; }
      tab.retry++;
      tab.timer = setTimeout(async () => {
        // The session may have ended while we were away (e.g. the server restarted).
        try {
          const { sessions } = await api('GET', '/api/term/sessions');
          if (!sessions.some((s) => s.id === tab.id)) { sessionEnded(tab); return; }
        } catch { /* offline: keep trying */ }
        connect(tab);
      }, Math.min(10000, 500 * 2 ** Math.min(tab.retry, 4)));
    };
  }

  function sessionEnded(tab) {
    if (tab.exited) return;
    tab.exited = true;
    tab.term.write('\r\n\x1b[2m[session ended — press any key to close this tab]\x1b[0m\r\n');
    tab.term.onKey(() => closeTab(tab));
    render();
  }

  function sendInput(d, tab = active) {
    if (!tab) return;
    if (tab.exited) { closeTab(tab); return; }
    if (ctrlSticky && d.length === 1) {
      const c = d.toUpperCase().charCodeAt(0);
      if (c >= 64 && c <= 95) d = String.fromCharCode(c - 64);
      ctrlSticky = false;
      ctrlKey.classList.remove('on');
    }
    if (tab.ws?.readyState === 1) tab.ws.send(enc.encode(d));
  }

  function activate(tab) {
    if (!tab) return;
    active = tab;
    for (const t of tabs) t.el.classList.toggle('hidden', t !== tab);
    if (!tab.opened) { tab.term.open(tab.el); tab.opened = true; }
    requestAnimationFrame(() => {
      try { tab.fit.fit(); } catch { /* not visible yet */ }
      tab.term.focus();
    });
    if (!tab.ws && !tab.exited) connect(tab);
    render();
  }

  function closeTab(tab) {
    clearTimeout(tab.timer);
    tab.ws?.close();
    tab.term.dispose();
    tab.el.remove();
    tabs.splice(tabs.indexOf(tab), 1);
    if (active === tab) active = null;
    if (!tabs.length) { win.close(); return; }
    activate(tabs[tabs.length - 1]);
  }

  async function endSession(tab) {
    if (!tab.exited && !(await confirmDialog('End this terminal session?', 'Programs running in it will be stopped.', 'End session'))) return;
    if (!tab.exited) await api('DELETE', `/api/term/sessions/${encodeURIComponent(tab.id)}`).catch(report);
    tab.exited = true;
    closeTab(tab);
  }

  function copySelection(tab) {
    const text = tab.term.getSelection();
    if (!text) return;
    share(text);
    navigator.clipboard.writeText(text).then(() => toast('Copied, also to your other devices'), () => toast('Sent to your shared clipboard'));
  }

  function paste(tab) {
    navigator.clipboard.readText().then((t) => t && tab.term.paste(t), () => toast('Use Ctrl+V to paste in this browser', 'error'));
  }

  function render() {
    strip.replaceChildren(...tabs.map((t, i) => h('div', {
      class: `bw-tab ${t === active ? 'active' : ''} ${t.exited ? 'ended' : ''}`, role: 'tab', onclick: () => activate(t),
      onauxclick: (e) => { if (e.button === 1) { e.preventDefault(); endSession(t); } },
    }, icon('terminal', 13), h('span', { class: 'bw-tab-title' }, t.title === 'Terminal' ? `Terminal ${i + 1}` : t.title),
    h('button', { class: 'bw-tab-close', 'aria-label': 'End session', title: 'End session', onclick: (e) => { e.stopPropagation(); endSession(t); } }, icon('x', 12)))));
    if (active) win.setTitle(active.title === 'Terminal' ? 'Terminal' : active.title);
  }

  const refit = debounce(() => { try { active?.fit.fit(); } catch { /* hidden */ } }, 60);
  const ro = new ResizeObserver(refit);
  ro.observe(panes);

  start();

  // The last screenfuls of the active terminal, for "explain this error".
  function context() {
    if (!active) return null;
    const buf = active.term.buffer.active;
    const lines = [];
    for (let i = Math.max(0, buf.length - 120); i < buf.length; i++) lines.push(buf.getLine(i)?.translateToString(true) ?? '');
    return { title: active.title, text: lines.join('\n').trim() };
  }

  return {
    context,
    onFocus() { active?.term.focus(); },
    onResize: refit,
    async onClose() {
      if (!toldKeepsRunning && tabs.some((t) => !t.exited)) {
        toldKeepsRunning = true;
        toast('Your terminal sessions keep running on the server. Open Terminal again to continue.');
      }
      return true;
    },
    onDestroy() {
      destroyed = true;
      ro.disconnect();
      for (const t of tabs) { clearTimeout(t.timer); t.ws?.close(); t.term.dispose(); }
    },
    saveState: () => ({ active: active?.id || null }),
  };
}
