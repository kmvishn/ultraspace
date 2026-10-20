import {
  h, icon, api, report, toast, menu, ask, confirmDialog, settings, system, fmtBytes, fmtDate, fmtAgo,
  kindOf, KIND_ICON, isEditable, joinPath, dirOf, baseOf, fileUrl, thumbUrl, hasThumb, downloadFile, ApiError,
} from '../core.js';
import { registerApp, openApp, openFile } from '../wm.js';

const DRAG_TYPE = 'application/x-ultraspace-paths';

registerApp({
  id: 'files', name: 'Files', icon: 'folder', color: ['#60a5fa', '#2563eb'], width: 980, height: 620, single: false,
  mount(win, args) { return mountFiles(win, args); },
});

function mountFiles(win, args) {
  let path = args.restore?.path || args.path || '/';
  let entries = [];
  let selected = new Set();
  let anchor = null;
  let query = '';
  const back = [];
  const fwd = [];
  let places = [];

  const crumbs = h('div', { class: 'crumbs' });
  const backBtn = h('button', { class: 'icon-btn', 'aria-label': 'Back', onclick: () => goBack() }, icon('chevronLeft'));
  const fwdBtn = h('button', { class: 'icon-btn', 'aria-label': 'Forward', onclick: () => goForward() }, icon('chevronRight'));
  const upBtn = h('button', { class: 'icon-btn', 'aria-label': 'Up', onclick: () => navigate(dirOf(path)) }, icon('arrowUp'));
  const search = h('input', { class: 'input search-input', type: 'search', placeholder: 'Search all files', 'aria-label': 'Search files' });
  const gridBtn = h('button', { class: 'seg-btn', 'aria-label': 'Grid view', onclick: () => setView('grid') }, icon('grid', 16));
  const listBtn = h('button', { class: 'seg-btn', 'aria-label': 'List view', onclick: () => setView('list') }, icon('list', 16));
  const fileInput = h('input', { type: 'file', multiple: true, class: 'hidden', onchange: () => { upload([...fileInput.files]); fileInput.value = ''; } });
  const content = h('div', { class: 'files-content', tabindex: '0' });
  const status = h('div', { class: 'statusbar' });
  const placesEl = h('div', { class: 'side-list' });
  const storage = h('div', { class: 'side-storage' });

  const root = h('div', { class: 'app app-files' },
    h('aside', { class: 'side' },
      h('div', { class: 'side-title' }, 'Places'), placesEl,
      h('div', { class: 'side-fill' }), storage),
    h('div', { class: 'main' },
      h('div', { class: 'toolbar' },
        h('div', { class: 'tb-group' }, backBtn, fwdBtn, upBtn), crumbs, h('div', { class: 'tb-spacer' }),
        search,
        h('div', { class: 'seg' }, gridBtn, listBtn),
        h('button', { class: 'icon-btn', 'aria-label': 'New folder', title: 'New folder', onclick: () => newFolder() }, icon('folderPlus')),
        h('button', { class: 'btn primary small', onclick: () => fileInput.click() }, icon('upload', 15), 'Upload'),
        fileInput),
      content, status));
  win.body.append(root);

  // ---------- navigation ----------

  function navigate(p, { record = true } = {}) {
    if (record && p !== path) { back.push(path); fwd.length = 0; }
    path = p || '/';
    query = '';
    search.value = '';
    selected.clear();
    anchor = null;
    load();
  }
  function goBack() { if (back.length) { fwd.push(path); navigate(back.pop(), { record: false }); } }
  function goForward() { if (fwd.length) { back.push(path); navigate(fwd.pop(), { record: false }); } }

  async function load() {
    try {
      if (query) {
        const res = await api('GET', `/api/files/search?q=${encodeURIComponent(query)}&limit=500`);
        entries = res.results.map((e) => ({ ...e }));
      } else {
        const res = await api('GET', `/api/files/list?path=${encodeURIComponent(path)}`);
        entries = res.entries.map((e) => ({ ...e, path: joinPath(path, e.name) }));
      }
      for (const p of [...selected]) if (!entries.some((e) => e.path === p)) selected.delete(p);
      render();
    } catch (err) {
      if (err instanceof ApiError && err.status === 404 && path !== '/') { toast('That folder no longer exists'); navigate('/'); return; }
      report(err);
      entries = [];
      render(err.message);
    }
    loadPlaces();
  }

  async function loadPlaces() {
    try {
      const res = await api('GET', '/api/files/list?path=/');
      places = res.entries.filter((e) => e.dir).map((e) => ({ name: e.name, path: `/${e.name}` }));
    } catch { places = []; }
    const placeIcon = (name) => {
      const n = name.toLowerCase();
      if (n.includes('music') || n.includes('audio')) return 'music';
      if (n.includes('photo') || n.includes('picture') || n.includes('image')) return 'image';
      if (n.includes('video') || n.includes('movie') || n.includes('film')) return 'film';
      if (n.includes('download')) return 'download';
      if (n.includes('doc')) return 'fileText';
      if (n.includes('project') || n.includes('code') || n.includes('src')) return 'code';
      return 'folder';
    };
    const item = (label, p, ic) => {
      const el = h('button', {
        class: `side-item ${p === path && !query ? 'active' : ''}`, onclick: () => navigate(p),
      }, icon(ic, 16), h('span', null, label));
      dropTarget(el, p);
      return el;
    };
    placesEl.replaceChildren(item('Home', '/', 'home'), ...places.map((p) => item(p.name, p.path, placeIcon(p.name))));
    const disk = (system.latest?.disks || []).find((d) => d.label === 'Files') || (system.latest?.disks || [])[0];
    storage.replaceChildren(disk ? h('div', null,
      h('div', { class: 'side-storage-head' }, icon('hardDrive', 14), 'Storage'),
      h('div', { class: 'meter thin' }, h('div', { style: { width: `${((100 * disk.used) / disk.total).toFixed(1)}%` } })),
      h('div', { class: 'side-storage-sub' }, `${fmtBytes(disk.total - disk.used)} free of ${fmtBytes(disk.total)}`)) : '');
  }

  // ---------- rendering ----------

  function setView(v) { settings.set({ filesView: v }); render(); }

  function render(errorText) {
    const view = settings.get('filesView') === 'list' || query ? 'list' : 'grid';
    gridBtn.classList.toggle('active', view === 'grid');
    listBtn.classList.toggle('active', view === 'list');
    backBtn.disabled = !back.length;
    fwdBtn.disabled = !fwd.length;
    upBtn.disabled = path === '/' || !!query;
    win.setTitle(query ? `Search: ${query}` : path === '/' ? 'Files' : baseOf(path));

    // Breadcrumbs
    const parts = path.split('/').filter(Boolean);
    const cr = [h('button', { class: 'crumb', onclick: () => navigate('/') }, icon('home', 15))];
    parts.forEach((name, i) => {
      const p = '/' + parts.slice(0, i + 1).join('/');
      cr.push(icon('chevronRight', 14, 'crumb-sep'));
      const b = h('button', { class: 'crumb', onclick: () => navigate(p) }, name);
      dropTarget(b, p);
      cr.push(b);
    });
    if (query) cr.push(icon('chevronRight', 14, 'crumb-sep'), h('span', { class: 'crumb' }, `Results for “${query}”`));
    crumbs.replaceChildren(...cr);

    if (errorText || !entries.length) {
      content.replaceChildren(h('div', { class: 'empty-state' },
        icon(errorText ? 'info' : query ? 'search' : 'folder', 42),
        h('div', { class: 'empty-title' }, errorText || (query ? 'No files match' : 'This folder is empty')),
        !errorText && !query ? h('div', { class: 'empty-sub' }, 'Drop files here, or use Upload.') : null));
    } else if (view === 'grid') {
      content.replaceChildren(h('div', { class: 'file-grid' }, entries.map(gridItem)));
    } else {
      content.replaceChildren(h('table', { class: 'table file-table' },
        h('thead', null, h('tr', null, h('th', null, 'Name'), query ? h('th', null, 'Folder') : null,
          h('th', { class: 'num' }, 'Size'), h('th', null, 'Modified'))),
        h('tbody', null, entries.map(listRow))));
    }
    updateStatus();
  }

  function thumb(e, size) {
    const k = kindOf(e);
    if (k === 'image') {
      // Small server-made thumbnails instead of full photos; anything without one shows as is.
      const src = hasThumb(e.name) ? thumbUrl(e.path, 256) : fileUrl(e.path, true);
      return h('span', { class: 'thumb img' }, h('img', {
        src, loading: 'lazy', decoding: 'async', alt: '', draggable: 'false',
        onerror: (ev) => { if (!ev.target.dataset.full) { ev.target.dataset.full = '1'; ev.target.src = fileUrl(e.path, true); } },
      }));
    }
    return h('span', { class: `thumb k-${k}` }, icon(KIND_ICON[k], size));
  }

  function itemProps(e) {
    return {
      class: `${selected.has(e.path) ? 'selected' : ''}`,
      draggable: 'true',
      dataset: { path: e.path },
      onclick: (ev) => clickItem(ev, e),
      ondblclick: () => open(e),
      oncontextmenu: (ev) => itemMenu(ev, e),
      ondragstart: (ev) => {
        if (!selected.has(e.path)) { selected = new Set([e.path]); markSelection(); }
        ev.dataTransfer.setData(DRAG_TYPE, JSON.stringify([...selected]));
        ev.dataTransfer.effectAllowed = 'move';
      },
    };
  }

  function gridItem(e) {
    const el = h('div', { ...itemProps(e), class: `file-tile ${selected.has(e.path) ? 'selected' : ''}` },
      thumb(e, 34), h('span', { class: 'file-name' }, e.name),
      h('span', { class: 'file-meta' }, e.dir ? '' : fmtBytes(e.size)));
    if (e.dir) dropTarget(el, e.path);
    return el;
  }

  function listRow(e) {
    const el = h('tr', itemProps(e),
      h('td', null, h('span', { class: 'row-name' }, h('span', { class: `row-icon k-${kindOf(e)}` }, icon(KIND_ICON[kindOf(e)], 17)), h('span', null, e.name))),
      query ? h('td', { class: 'muted' }, dirOf(e.path)) : null,
      h('td', { class: 'num muted' }, e.dir ? '—' : fmtBytes(e.size)),
      h('td', { class: 'muted', title: fmtDate(e.mtime) }, fmtAgo(e.mtime)));
    if (e.dir) dropTarget(el, e.path);
    return el;
  }

  function markSelection() {
    content.querySelectorAll('[data-path]').forEach((el) => el.classList.toggle('selected', selected.has(el.dataset.path)));
    updateStatus();
  }

  function updateStatus() {
    const sel = entries.filter((e) => selected.has(e.path));
    const size = sel.reduce((n, e) => n + (e.dir ? 0 : e.size), 0);
    const folders = entries.filter((e) => e.dir).length;
    status.replaceChildren(...[
      h('span', null, `${entries.length} item${entries.length === 1 ? '' : 's'}` + (folders ? ` · ${folders} folder${folders === 1 ? '' : 's'}` : '')),
      sel.length ? h('span', { class: 'status-sel' }, `${sel.length} selected${size ? ` · ${fmtBytes(size)}` : ''}`) : null,
      h('div', { class: 'tb-spacer' }),
      sel.length ? h('button', { class: 'link-btn danger', onclick: () => remove(sel) }, icon('trash', 14), 'Delete') : null,
    ].filter(Boolean));
  }

  // ---------- selection & actions ----------

  function clickItem(ev, e) {
    ev.stopPropagation();
    if (ev.pointerType === 'touch' || matchMedia('(pointer: coarse)').matches) { open(e); return; }
    if (ev.shiftKey && anchor) {
      const a = entries.findIndex((x) => x.path === anchor), b = entries.findIndex((x) => x.path === e.path);
      const [lo, hi] = a < b ? [a, b] : [b, a];
      if (!(ev.ctrlKey || ev.metaKey)) selected.clear();
      entries.slice(lo, hi + 1).forEach((x) => selected.add(x.path));
    } else if (ev.ctrlKey || ev.metaKey) {
      selected.has(e.path) ? selected.delete(e.path) : selected.add(e.path);
      anchor = e.path;
    } else {
      selected = new Set([e.path]);
      anchor = e.path;
    }
    markSelection();
  }

  function open(e) {
    if (e.dir) { navigate(e.path); return; }
    const siblings = entries.filter((x) => ['image', 'video'].includes(kindOf(x))).map((x) => x.path);
    openFile(e, { siblings });
  }

  function itemMenu(ev, e) {
    ev.preventDefault();
    ev.stopPropagation();
    if (!selected.has(e.path)) { selected = new Set([e.path]); anchor = e.path; markSelection(); }
    const sel = entries.filter((x) => selected.has(x.path));
    const single = sel.length === 1;
    menu(ev.clientX, ev.clientY, [
      { label: 'Open', icon: e.dir ? 'folder' : 'external', action: () => open(e), disabled: !single },
      single && !e.dir && !isEditable(e) && kindOf(e) === 'file' ? { label: 'Open in Text Editor', icon: 'code', action: () => openApp('editor', { path: e.path }) } : null,
      query && single ? { label: 'Show in folder', icon: 'folder', action: () => navigate(dirOf(e.path)) } : null,
      single && !e.dir ? { label: 'Download', icon: 'download', action: () => downloadFile(e.path) } : null,
      single && kindOf(e) === 'image' ? { label: 'Set as wallpaper', icon: 'wallpaper', action: () => { settings.set({ wallpaper: `file:${e.path}` }); toast('Wallpaper updated'); } } : null,
      single && kindOf(e) === 'audio' ? { label: 'Play in Music', icon: 'music', action: () => openApp('music', { play: e.path }) } : null,
      '-',
      { label: 'Rename', icon: 'pencil', hint: 'F2', action: () => rename(e), disabled: !single },
      { label: sel.length > 1 ? `Delete ${sel.length} items` : 'Delete', icon: 'trash', hint: 'Del', danger: true, action: () => remove(sel) },
    ]);
  }

  function blankMenu(ev) {
    if (ev.target.closest('[data-path]')) return;
    ev.preventDefault();
    selected.clear();
    markSelection();
    menu(ev.clientX, ev.clientY, [
      { label: 'New folder', icon: 'folderPlus', action: newFolder, disabled: !!query },
      { label: 'New text file', icon: 'filePlus', action: newTextFile, disabled: !!query },
      { label: 'Upload files…', icon: 'upload', action: () => fileInput.click(), disabled: !!query },
      '-',
      { label: settings.get('filesView') === 'list' ? 'View as grid' : 'View as list', icon: settings.get('filesView') === 'list' ? 'grid' : 'list', action: () => setView(settings.get('filesView') === 'list' ? 'grid' : 'list') },
      { label: 'Refresh', icon: 'refresh', action: load },
    ]);
  }

  async function newFolder() {
    const name = await ask('New folder', 'Folder name', 'New folder', 'Create');
    if (!name) return;
    try { await api('POST', '/api/files/mkdir', { path: joinPath(path, name) }); await load(); } catch (err) { report(err); }
  }

  async function newTextFile() {
    const name = await ask('New text file', 'File name', 'Untitled.txt', 'Create');
    if (!name) return;
    const p = joinPath(path, name);
    if (entries.some((e) => e.path === p)) { toast('A file with that name already exists', 'error'); return; }
    try {
      await api('POST', '/api/files/write', { path: p, content: '' });
      await load();
      openApp('editor', { path: p });
    } catch (err) { report(err); }
  }

  async function rename(e) {
    const name = await ask('Rename', 'New name', e.name);
    if (!name || name === e.name) return;
    try {
      await api('POST', '/api/files/rename', { from: e.path, to: joinPath(dirOf(e.path), name) });
      selected.delete(e.path);
      load();
    } catch (err) { report(err); }
  }

  async function remove(sel) {
    if (!sel.length) return;
    const what = sel.length === 1 ? `“${sel[0].name}”` : `${sel.length} items`;
    if (!(await confirmDialog('Delete permanently?', `${what} will be deleted. This cannot be undone.`))) return;
    try {
      await api('POST', '/api/files/delete', { paths: sel.map((e) => e.path) });
      sel.forEach((e) => selected.delete(e.path));
      toast(sel.length === 1 ? `Deleted ${sel[0].name}` : `Deleted ${sel.length} items`);
    } catch (err) { report(err); }
    load();
  }

  async function move(paths, destDir) {
    let moved = 0;
    for (const p of paths) {
      if (p === destDir || dirOf(p) === destDir || destDir.startsWith(p + '/')) continue;
      try { await api('POST', '/api/files/rename', { from: p, to: joinPath(destDir, baseOf(p)) }); moved++; } catch (err) { report(err); }
    }
    if (moved) toast(`Moved ${moved} item${moved === 1 ? '' : 's'} to ${destDir === '/' ? 'Home' : baseOf(destDir)}`);
    selected.clear();
    load();
  }

  function dropTarget(el, dest) {
    el.addEventListener('dragover', (ev) => {
      if (!ev.dataTransfer.types.includes(DRAG_TYPE)) return;
      ev.preventDefault();
      ev.stopPropagation();
      el.classList.add('drop-hover');
    });
    el.addEventListener('dragleave', () => el.classList.remove('drop-hover'));
    el.addEventListener('drop', (ev) => {
      if (!ev.dataTransfer.types.includes(DRAG_TYPE)) return;
      ev.preventDefault();
      ev.stopPropagation();
      el.classList.remove('drop-hover');
      move(JSON.parse(ev.dataTransfer.getData(DRAG_TYPE)), dest);
    });
  }

  async function upload(list) {
    if (!list.length || query) return;
    const dir = path;
    await uploadFiles(dir, list);
    if (dir === path) load();
  }

  // ---------- wiring ----------

  content.addEventListener('click', (ev) => { if (!ev.target.closest('[data-path]')) { selected.clear(); markSelection(); } });
  content.addEventListener('contextmenu', blankMenu);
  content.addEventListener('dragover', (ev) => {
    if (!ev.dataTransfer.types.includes('Files')) return;
    ev.preventDefault();
    content.classList.add('dragover');
  });
  content.addEventListener('dragleave', (ev) => { if (!content.contains(ev.relatedTarget)) content.classList.remove('dragover'); });
  content.addEventListener('drop', (ev) => {
    content.classList.remove('dragover');
    if (!ev.dataTransfer.types.includes('Files')) return;
    ev.preventDefault();
    upload([...ev.dataTransfer.files]);
  });
  content.addEventListener('keydown', (ev) => {
    if (ev.target !== content) return;
    const sel = entries.filter((e) => selected.has(e.path));
    if (ev.key === 'Delete') { remove(sel); }
    else if (ev.key === 'F2' && sel.length === 1) { rename(sel[0]); }
    else if (ev.key === 'Enter' && sel.length === 1) { open(sel[0]); }
    else if (ev.key === 'Backspace' || (ev.altKey && ev.key === 'ArrowUp')) { if (path !== '/') navigate(dirOf(path)); }
    else if ((ev.ctrlKey || ev.metaKey) && ev.key.toLowerCase() === 'a') { ev.preventDefault(); selected = new Set(entries.map((e) => e.path)); markSelection(); }
    else return;
    ev.preventDefault();
  });
  let searchTimer;
  search.addEventListener('input', () => {
    clearTimeout(searchTimer);
    searchTimer = setTimeout(() => {
      const q = search.value.trim();
      if (q === query) return;
      query = q.length >= 2 ? q : '';
      selected.clear();
      load();
    }, 250);
  });

  load();
  return {
    onArgs(a) { if (a.path) navigate(a.path); },
    saveState: () => ({ path }),
  };
}

// ---------- uploads (shared tray) ----------

let tray = null;

export function uploadFiles(dir, files) {
  return files.reduce((p, f) => p.then(() => uploadOne(dir, f).catch(report)), Promise.resolve());
}

function uploadOne(dir, file) {
  if (!tray || !tray.isConnected) { tray = h('div', { class: 'uploads' }); document.body.append(tray); }
  const bar = h('div');
  const status = h('span', { class: 'up-status' }, 'Waiting…');
  const item = h('div', { class: 'upload glass' },
    h('div', { class: 'up-head' }, icon('upload', 14), h('span', { class: 'up-name' }, file.name)),
    h('div', { class: 'meter thin' }, bar), status);
  tray.append(item);
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    xhr.open('POST', `/api/files/upload?path=${encodeURIComponent(dir)}`);
    xhr.setRequestHeader('X-Ultraspace', '1');
    const started = performance.now();
    xhr.upload.onprogress = (e) => {
      if (!e.lengthComputable) return;
      bar.style.width = `${((100 * e.loaded) / e.total).toFixed(1)}%`;
      const rate = e.loaded / Math.max(0.001, (performance.now() - started) / 1000);
      status.textContent = `${fmtBytes(e.loaded)} of ${fmtBytes(e.total)} · ${fmtBytes(rate)}/s`;
    };
    const finish = (err) => {
      item.classList.add(err ? 'failed' : 'done');
      setTimeout(() => item.remove(), err ? 6000 : 1800);
      err ? reject(err) : resolve();
    };
    xhr.onload = () => {
      if (xhr.status >= 200 && xhr.status < 300) { bar.style.width = '100%'; status.textContent = 'Uploaded'; finish(); return; }
      let msg = xhr.statusText || 'Upload failed';
      try { msg = JSON.parse(xhr.responseText).error || msg; } catch { /* not JSON */ }
      status.textContent = msg;
      finish(new Error(`${file.name}: ${msg}`));
    };
    xhr.onerror = () => { status.textContent = 'Network error'; finish(new Error(`${file.name}: network error`)); };
    const form = new FormData();
    form.append('file', file, file.name);
    xhr.send(form);
  });
}
