// Torrents: a BitTorrent client running on the server. Downloads land in Files → Torrents;
// videos can be played while they download. Add magnet links (paste one anywhere in the
// window), .torrent files from this device or from Files. Web downloads started from
// Stremio (into Files → Downloads) are listed here too.
import { h, icon, api, report, toast, menu, confirmDialog, fmtBytes, fmtDuration, dirOf, filesFrom } from '../core.js';
import { registerApp, openApp } from '../wm.js';
import { pickFiles } from '../pick.js';

registerApp({
  id: 'torrents', name: 'Torrents', icon: 'magnet', color: ['#34d399', '#047857'], width: 980, height: 620, minWidth: 420, minHeight: 300,
  mount(win, args) { return mountTorrents(win, args); },
});

const FILTERS = [
  ['all', 'All', 'layers', () => true],
  ['downloading', 'Downloading', 'download', (t) => t.state === 'downloading' || t.state === 'metadata'],
  ['seeding', 'Seeding', 'upload', (t) => t.state === 'seeding'],
  ['done', 'Finished', 'check', (t) => t.state === 'done' || t.state === 'seeding'],
  ['paused', 'Paused', 'pause', (t) => t.state === 'paused' || t.state === 'error'],
  ['stream', 'Streamed', 'tv', (t) => t.stream],
  ['web', 'Web downloads', 'globe', () => false],
];
const STATE_LABEL = { metadata: 'Getting details', downloading: 'Downloading', seeding: 'Seeding', done: 'Finished', paused: 'Paused', error: 'Error' };
const rate = (n) => (n >= 1 ? `${fmtBytes(n)}/s` : '0 B/s');

// addTorrentFile sends a .torrent file (a File or Blob) to the server.
export async function addTorrentFile(file, name = file.name, stream = false) {
  const form = new FormData();
  form.append('file', file, name);
  if (stream) form.append('stream', '1');
  const res = await fetch('/api/torrents', { method: 'POST', headers: { 'X-Ultraspace': '1' }, body: form, credentials: 'same-origin' });
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(data.error || res.statusText);
  return data.hash;
}

function mountTorrents(win, args) {
  let data = { torrents: [], running: false, down: 0, up: 0, port: 0 };
  let filter = args.filter || args.restore?.filter || 'all';
  let web = [];
  let selected = args.restore?.selected || null;
  let detail = null;
  let timer = null;
  let disabled = '';
  let ticks = 0;

  const nav = h('div', { class: 'side-list' });
  const stats = h('div', { class: 'tr-side-stats' });
  const list = h('div', { class: 'tr-list scroll' });
  const pane = h('div', { class: 'tr-detail hidden' });
  const magnet = h('input', { class: 'input tr-magnet', type: 'text', placeholder: 'Paste a magnet link', 'aria-label': 'Magnet link', spellcheck: 'false' });
  const fileInput = h('input', { type: 'file', accept: '.torrent,application/x-bittorrent', multiple: true, class: 'hidden', onchange: () => { addFiles([...fileInput.files]); fileInput.value = ''; } });

  const root = h('div', { class: 'app app-torrents' },
    h('aside', { class: 'side' }, h('div', { class: 'side-title' }, 'Torrents'), nav, h('div', { class: 'side-fill' }), stats),
    h('div', { class: 'main' },
      h('form', { class: 'toolbar', onsubmit: (e) => { e.preventDefault(); addMagnet(magnet.value); } },
        icon('magnet', 16), magnet,
        h('button', { class: 'btn primary small', type: 'submit' }, icon('plus', 15), 'Add'),
        h('button', { class: 'btn small', type: 'button', title: 'Add a .torrent file from this device', onclick: () => fileInput.click() }, icon('upload', 15), h('span', { class: 'tr-hide-narrow' }, 'Open')),
        h('button', { class: 'btn small', type: 'button', title: 'Add a .torrent file from Files', onclick: fromFiles }, icon('folder', 15), h('span', { class: 'tr-hide-narrow' }, 'From Files')),
        fileInput),
      list, pane));
  win.body.append(root);

  // ---------- adding ----------

  async function addMagnet(text) {
    const uri = text.trim();
    if (!uri) { magnet.focus(); return; }
    try {
      const { hash } = await api('POST', '/api/torrents', { magnet: uri });
      magnet.value = '';
      toast('Torrent added');
      selected = hash;
      refresh();
    } catch (err) { report(err); }
  }

  async function addFiles(files) {
    for (const f of files) {
      try { selected = await addTorrentFile(f); toast(`Added ${f.name}`); } catch (err) { toast(`${f.name}: ${err.message}`, 'error'); }
    }
    refresh();
  }

  async function addFromLibrary(path) {
    try {
      const res = await fetch(`/api/files/download?path=${encodeURIComponent(path)}`, { credentials: 'same-origin' });
      if (!res.ok) throw new Error('could not read the file');
      selected = await addTorrentFile(await res.blob(), path.split('/').pop());
      toast('Torrent added');
      refresh();
    } catch (err) { report(err); }
  }

  async function fromFiles() {
    const paths = await pickFiles({ title: 'Choose .torrent files', multiple: true });
    for (const p of paths || []) await addFromLibrary(p);
  }

  root.addEventListener('paste', (e) => {
    if (e.target === magnet) return;
    const text = e.clipboardData?.getData('text') || '';
    if (text.trim().startsWith('magnet:')) { e.preventDefault(); addMagnet(text); }
  });
  root.addEventListener('dragover', (e) => { if ([...(e.dataTransfer?.types || [])].includes('Files')) { e.preventDefault(); root.classList.add('drop'); } });
  root.addEventListener('dragleave', (e) => { if (!root.contains(e.relatedTarget)) root.classList.remove('drop'); });
  root.addEventListener('drop', (e) => {
    root.classList.remove('drop');
    const files = filesFrom(e.dataTransfer).filter((f) => f.name.toLowerCase().endsWith('.torrent'));
    const text = e.dataTransfer?.getData('text') || '';
    if (!files.length && !text.startsWith('magnet:')) return;
    e.preventDefault();
    e.stopPropagation();
    if (files.length) addFiles(files); else addMagnet(text);
  });

  // ---------- actions ----------

  async function act(t, action) {
    try {
      await api('POST', `/api/torrents/${t.hash}/${action}`);
      if (action === 'remove' || action === 'delete') { if (selected === t.hash) selected = null; toast(action === 'delete' ? 'Removed with its files' : 'Removed (files kept)'); }
      refresh();
    } catch (err) { report(err); }
  }

  async function removeMenu(t, x, y) {
    menu(x, y, [
      { label: 'Remove, keep files', icon: 'x', action: () => act(t, 'remove') },
      { label: 'Remove and delete files', icon: 'trash', danger: true, action: async () => {
        if (await confirmDialog('Delete downloaded files?', `“${t.name}” will be removed and its files deleted from Files.`)) act(t, 'delete');
      } },
    ]);
  }

  function play(t, file) {
    const idx = file ? file.index : -1;
    openApp('video', { src: `torrent:${t.hash}:${idx}`, name: file ? file.path.split('/').pop() : t.name, key: `torrent:${t.hash}:${idx}` });
  }

  function rowMenu(t, x, y) {
    menu(x, y, [
      t.state === 'paused' || t.state === 'error' || t.state === 'done'
        ? { label: t.stream ? 'Download it all' : 'Resume', icon: 'play', action: () => act(t, 'resume') }
        : { label: 'Pause', icon: 'pause', action: () => act(t, 'pause') },
      { label: 'Play', icon: 'film', action: () => play(t) },
      { label: 'Show in Files', icon: 'folder', disabled: !t.path, action: () => openApp('files', { path: dirOf(t.path) }) },
      { label: 'Copy magnet link', icon: 'link', action: async () => {
        try { const d = await api('GET', `/api/torrents/${t.hash}`); await navigator.clipboard.writeText(d.magnet); toast('Magnet link copied'); } catch (err) { report(err); }
      } },
      '-',
      { label: 'Remove…', icon: 'trash', danger: true, action: () => removeMenu(t, x, y) },
    ]);
  }

  // ---------- rendering ----------

  function render() {
    const counts = Object.fromEntries(FILTERS.map(([id, , , fn]) => [id, data.torrents.filter(fn).length]));
    counts.web = web.length;
    nav.replaceChildren(...FILTERS.filter(([id]) => (id !== 'stream' && id !== 'web') || counts[id] || filter === id).map(([id, label, ic]) => h('button', {
      class: `side-item ${filter === id ? 'active' : ''}`, onclick: () => { filter = id; render(); win.toggleDrawer?.(false); },
    }, icon(ic, 16), h('span', null, label), h('span', { class: 'side-count' }, counts[id] || ''))));
    stats.replaceChildren(
      h('div', { class: 'tr-rate' }, icon('arrowDown', 14), rate(data.down)),
      h('div', { class: 'tr-rate' }, icon('arrowUp', 14), rate(data.up)),
      h('div', { class: 'muted small' }, disabled || (data.running ? `Engine on · port ${data.port}` : 'Engine idle (starts when needed)')));

    if (filter === 'web') {
      list.replaceChildren(...(web.length ? web.map(webRow) : [h('div', { class: 'empty-state' }, icon('globe', 40), h('div', { class: 'empty-title' }, 'No web downloads'),
        h('div', { class: 'empty-sub' }, 'In Stremio, the download button next to a web stream saves it on the server, into Files → Downloads.'))]));
      pane.classList.add('hidden');
      return;
    }
    const shown = data.torrents.filter((FILTERS.find(([id]) => id === filter) || FILTERS[0])[3]);
    if (disabled) {
      list.replaceChildren(h('div', { class: 'empty-state' }, icon('magnet', 40), h('div', { class: 'empty-title' }, 'Torrents are turned off'), h('div', { class: 'empty-sub' }, disabled)));
      return;
    }
    if (!shown.length) {
      list.replaceChildren(h('div', { class: 'empty-state' }, icon('magnet', 40),
        h('div', { class: 'empty-title' }, data.torrents.length ? 'Nothing here' : 'No torrents yet'),
        h('div', { class: 'empty-sub' }, 'Paste a magnet link, drop .torrent files here, or open one from Files. Downloads go to Files → Torrents, and videos play while they download.')));
    } else {
      list.replaceChildren(...shown.map(row));
    }
    renderDetail();
  }

  function row(t) {
    const pct = Math.round(t.progress * 1000) / 10;
    const active = t.state === 'downloading' || t.state === 'metadata';
    const eta = active && t.down > 0 && t.size ? fmtDuration((t.size - t.have) / t.down) : '';
    const paused = t.state === 'paused' || t.state === 'error' || t.state === 'done';
    const btn = (ic, label, fn, cls = '') => h('button', { class: `icon-btn ${cls}`, title: label, 'aria-label': label, onclick: (e) => { e.stopPropagation(); fn(e); } }, icon(ic, 16));
    return h('div', {
      class: `tr-row ${selected === t.hash ? 'selected' : ''}`, tabindex: '0',
      onclick: () => { selected = selected === t.hash ? null : t.hash; detail = null; render(); loadDetail(); },
      oncontextmenu: (e) => { e.preventDefault(); rowMenu(t, e.clientX, e.clientY); },
      onkeydown: (e) => { if (e.key === 'Delete') { const r = e.currentTarget.getBoundingClientRect(); removeMenu(t, r.left + 40, r.top + 30); } },
    },
      h('div', { class: 'tr-row-main' },
        h('div', { class: 'tr-name' }, t.stream ? h('span', { class: 'pill', title: 'Added for playing: only what you watch is downloaded' }, 'Stream') : null, h('span', null, t.name)),
        h('div', { class: `meter thin ${t.state === 'error' ? 'crit' : t.state === 'seeding' || t.state === 'done' ? 'ok' : ''}` }, h('div', { style: { width: `${pct}%` } })),
        h('div', { class: 'tr-meta muted small' },
          h('span', { class: `tr-state ${t.state}` }, t.playing ? 'Playing' : STATE_LABEL[t.state] || t.state),
          t.size ? h('span', null, `${fmtBytes(t.have)} of ${fmtBytes(t.size)} (${pct}%)`) : null,
          active || t.state === 'seeding' ? h('span', null, `↓ ${rate(t.down)}  ↑ ${rate(t.up)}`) : null,
          active || t.state === 'seeding' ? h('span', null, icon('users', 12), ` ${t.peers}${t.seeders ? ` (${t.seeders} seeds)` : ''}`) : null,
          eta ? h('span', null, `${eta} left`) : null,
          t.ratio > 0.005 ? h('span', null, `ratio ${t.ratio.toFixed(2)}`) : null,
          t.error ? h('span', { class: 'tr-error' }, t.error) : null)),
      h('div', { class: 'tr-actions' },
        btn('play', 'Play', () => play(t)),
        paused ? btn('download', t.stream ? 'Download it all' : 'Resume', () => act(t, 'resume')) : btn('pause', 'Pause', () => act(t, 'pause')),
        btn('moreH', 'More', (e) => { const r = e.currentTarget.getBoundingClientRect(); rowMenu(t, r.left, r.bottom + 4); })));
  }

  function webRow(d) {
    const pct = d.size > 0 ? Math.round((1000 * d.have) / d.size) / 10 : 0;
    const running = d.state === 'downloading' || d.state === 'queued';
    const eta = d.state === 'downloading' && d.rate > 0 && d.size > 0 ? fmtDuration((d.size - d.have) / d.rate) : '';
    const webAct = (action) => api('POST', `/api/downloads/${d.id}/${action}`).then(refresh).catch(report);
    return h('div', { class: 'tr-row' },
      h('div', { class: 'tr-row-main' },
        h('div', { class: 'tr-name' }, h('span', null, d.name)),
        h('div', { class: `meter thin ${d.state === 'failed' ? 'crit' : d.state === 'done' ? 'ok' : ''}` }, h('div', { style: { width: `${d.state === 'done' ? 100 : pct}%` } })),
        h('div', { class: 'tr-meta muted small' },
          h('span', { class: `tr-state ${d.state === 'done' ? 'done' : d.state === 'failed' ? 'error' : 'downloading'}` }, { queued: 'Waiting', downloading: 'Downloading', done: 'Finished', failed: 'Failed', cancelled: 'Cancelled' }[d.state]),
          h('span', null, d.size > 0 ? `${fmtBytes(d.have)} of ${fmtBytes(d.size)} (${pct}%)` : fmtBytes(d.have)),
          d.state === 'downloading' ? h('span', null, `↓ ${rate(d.rate)}`) : null,
          eta ? h('span', null, `${eta} left`) : null,
          h('span', null, d.host),
          d.error ? h('span', { class: 'tr-error' }, d.error) : null)),
      h('div', { class: 'tr-actions' },
        d.state === 'done' ? h('button', { class: 'icon-btn', title: 'Play', 'aria-label': 'Play', onclick: () => openApp('video', { path: d.path }) }, icon('play', 16)) : null,
        d.path ? h('button', { class: 'icon-btn', title: 'Show in Files', 'aria-label': 'Show in Files', onclick: () => openApp('files', { path: dirOf(d.path) }) }, icon('folder', 16)) : null,
        running ? h('button', { class: 'icon-btn', title: 'Cancel', 'aria-label': 'Cancel', onclick: () => webAct('cancel') }, icon('x', 16))
          : h('button', { class: 'icon-btn', title: 'Remove from the list (keeps the file)', 'aria-label': 'Remove from the list', onclick: () => webAct('forget') }, icon('trash', 16))));
  }

  async function loadDetail() {
    if (!selected) { renderDetail(); return; }
    try { detail = await api('GET', `/api/torrents/${selected}`); } catch { detail = null; selected = null; }
    renderDetail();
  }

  function renderDetail() {
    const t = data.torrents.find((x) => x.hash === selected);
    pane.classList.toggle('hidden', !t);
    if (!t) return;
    const files = detail?.torrent?.hash === t.hash ? detail.files : null;
    const toggle = async (f, wanted) => {
      const skip = files.filter((x) => (x === f ? !wanted : !x.wanted)).map((x) => x.index);
      try { await api('PUT', `/api/torrents/${t.hash}/files`, { skip }); loadDetail(); refresh(); } catch (err) { report(err); }
    };
    pane.replaceChildren(
      h('div', { class: 'tr-detail-head' }, h('strong', null, t.name), h('span', { class: 'tb-spacer' }),
        t.path ? h('button', { class: 'btn small', onclick: () => openApp('files', { path: files && files.length > 1 ? t.path : dirOf(t.path) }) }, icon('folder', 14), 'Show in Files') : null,
        h('button', { class: 'icon-btn', title: 'Close', 'aria-label': 'Close details', onclick: () => { selected = null; render(); } }, icon('x', 16))),
      !files ? h('div', { class: 'empty-state small' }, h('span', { class: 'bw-spin' }), 'Loading files…')
        : !files.length ? h('div', { class: 'empty-state small' }, 'The file list appears once the torrent’s details arrive from peers.')
          : h('div', { class: 'tr-files scroll' }, h('table', { class: 'table' },
            h('thead', null, h('tr', null, h('th', null, ''), h('th', null, 'File'), h('th', { class: 'num' }, 'Size'), h('th', { class: 'num' }, 'Done'), h('th', null, ''))),
            h('tbody', null, files.map((f) => h('tr', null,
              h('td', null, h('input', { type: 'checkbox', checked: f.wanted, 'aria-label': `Download ${f.path}`, onchange: (e) => toggle(f, e.target.checked) })),
              h('td', { class: 'tr-file-name' }, f.path),
              h('td', { class: 'num' }, fmtBytes(f.size)),
              h('td', { class: 'num' }, f.size ? `${Math.floor((100 * f.have) / f.size)}%` : '—'),
              h('td', null, f.playable ? h('button', { class: 'icon-btn', title: 'Play', 'aria-label': `Play ${f.path}`, onclick: () => play(t, f) }, icon('play', 15)) : null)))))));
  }

  // ---------- polling ----------

  async function refresh() {
    api('GET', '/api/downloads').then((r) => { web = r.downloads || []; if (filter === 'web') render(); }).catch(() => {});
    try {
      data = await api('GET', '/api/torrents');
      disabled = '';
    } catch (err) {
      if (err.status === 501) disabled = err.message; else return;
    }
    if (selected && !data.torrents.some((t) => t.hash === selected)) selected = null;
    render();
    if (selected && (!detail || detail.torrent?.hash !== selected || ++ticks % 4 === 0)) loadDetail();
  }
  const poll = () => { if (!document.hidden) refresh(); };
  timer = setInterval(poll, 2000);
  refresh();
  if (args.path) addFromLibrary(args.path);
  if (args.magnet) addMagnet(args.magnet);

  return {
    onArgs(a) { if (a.path) addFromLibrary(a.path); if (a.magnet) addMagnet(a.magnet); if (a.filter) { filter = a.filter; refresh(); } },
    onFocus() { refresh(); },
    saveState: () => ({ filter, selected }),
    onDestroy() { clearInterval(timer); },
  };
}
