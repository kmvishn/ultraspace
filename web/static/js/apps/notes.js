// Notes: Markdown notes kept as plain .md files in Files → Notes, so they are
// ordinary files you can also open, back up or edit anywhere else.
import { h, icon, api, report, toast, confirmDialog, menu, fmtAgo, debounce, fileUrl, ApiError } from '../core.js';
import { registerApp } from '../wm.js';
import { renderMarkdown, toggleTask } from '../markdown.js';

registerApp({
  id: 'notes', name: 'Notes', icon: 'pencil', color: ['#fde047', '#eab308'], width: 1000, height: 660, minWidth: 460,
  mount(win, args) { return mountNotes(win, args); },
});

const DIR = '/Notes';
const MODES = [['edit', 'Edit', 'pencil'], ['split', 'Split', 'layers'], ['preview', 'Preview', 'fileText']];

const noteName = (p) => p.replace(/^.*\//, '').replace(/\.md$/i, '');
const safeTitle = (t) => (t.replace(/[\\/:*?"<>|\u0000-\u001f]/g, ' ').replace(/\s+/g, ' ').trim().slice(0, 100) || 'Untitled');

function mountNotes(win, args) {
  let notes = [];          // [{ path, title, mtime, size }]
  let current = null;      // { path, text, saved, mtime }
  let filter = '';
  let mode = 'split';
  try { mode = localStorage.getItem('us.notes.mode') || 'split'; } catch { /* default */ }
  let contents = new Map(); // path -> text, for full-text search
  let saving = null;

  const search = h('input', { class: 'input search-input', type: 'search', placeholder: 'Search notes', 'aria-label': 'Search notes' });
  const listEl = h('div', { class: 'notes-list' });
  const titleIn = h('input', { class: 'notes-title', type: 'text', placeholder: 'Title', 'aria-label': 'Note title', spellcheck: 'true' });
  const area = h('textarea', { class: 'notes-area', placeholder: 'Start writing… Markdown works: # heading, **bold**, - [ ] task', spellcheck: 'true', 'aria-label': 'Note text' });
  const preview = h('div', { class: 'notes-preview md' });
  const status = h('span', { class: 'notes-status' });
  const modeSeg = h('div', { class: 'seg' });
  const editor = h('div', { class: 'notes-editor' }, area, preview);
  const empty = h('div', { class: 'empty-state' }, icon('pencil', 40), h('div', { class: 'empty-title' }, 'No note open'),
    h('button', { class: 'btn primary', onclick: () => create() }, icon('plus', 15), 'New note'));

  const fmt = (before, after = before, placeholder = '') => () => {
    const { selectionStart: s, selectionEnd: e, value } = area;
    const sel = value.slice(s, e) || placeholder;
    area.setRangeText(before + sel + after, s, e, 'select');
    area.setSelectionRange(s + before.length, s + before.length + sel.length);
    area.focus();
    changed();
  };
  const linePrefix = (prefix) => () => {
    const { selectionStart: s, value } = area;
    const start = value.lastIndexOf('\n', s - 1) + 1;
    area.setRangeText(prefix, start, start, 'end');
    area.focus();
    changed();
  };
  const tool = (ic, label, fn) => h('button', { class: 'icon-btn', title: label, 'aria-label': label, onclick: fn }, icon(ic, 15));
  const tools = h('div', { class: 'tb-group notes-tools' },
    tool('heading', 'Heading', linePrefix('## ')), tool('bold', 'Bold (Ctrl B)', fmt('**', '**', 'bold')), tool('italic', 'Italic (Ctrl I)', fmt('*', '*', 'italic')),
    tool('list', 'List', linePrefix('- ')), tool('check', 'Checkbox', linePrefix('- [ ] ')), tool('code', 'Code', fmt('`', '`', 'code')),
    tool('link', 'Link', fmt('[', '](https://)', 'text')));

  const main = h('div', { class: 'main notes-main' },
    h('div', { class: 'toolbar' }, titleIn, h('div', { class: 'tb-spacer' }), status, tools, modeSeg,
      h('button', { class: 'icon-btn', title: 'More', 'aria-label': 'More', onclick: (e) => noteMenu(e) }, icon('moreH', 16))),
    editor);

  win.body.append(h('div', { class: 'app app-notes' },
    h('aside', { class: 'side notes-side' },
      h('div', { class: 'notes-side-top' }, search, h('button', { class: 'icon-btn', title: 'New note', 'aria-label': 'New note', onclick: () => create() }, icon('plus', 17))),
      listEl),
    main, empty));

  // ---------- list ----------

  async function loadList() {
    try {
      const res = await api('GET', `/api/files/list?path=${encodeURIComponent(DIR)}`);
      notes = res.entries.filter((e) => !e.dir && /\.md$/i.test(e.name))
        .map((e) => ({ path: `${DIR}/${e.name}`, title: noteName(e.name), mtime: e.mtime, size: e.size }))
        .sort((a, b) => new Date(b.mtime) - new Date(a.mtime));
    } catch (err) {
      if (err instanceof ApiError && err.status === 404) notes = []; // no Notes folder yet
      else report(err);
    }
    renderList();
  }

  function renderList() {
    const q = filter.toLowerCase();
    const shown = q ? notes.filter((n) => n.title.toLowerCase().includes(q) || (contents.get(n.path) || '').toLowerCase().includes(q)) : notes;
    listEl.replaceChildren(...(shown.length ? shown.map((n) => h('button', {
      class: `notes-item ${current?.path === n.path ? 'active' : ''}`, onclick: () => open(n.path),
      oncontextmenu: (e) => { e.preventDefault(); menu(e.clientX, e.clientY, [
        { label: 'Open', icon: 'fileText', action: () => open(n.path) },
        { label: 'Delete', icon: 'trash', danger: true, action: () => remove(n.path) },
      ]); },
    }, h('span', { class: 'notes-item-title' }, n.title), h('span', { class: 'notes-item-meta' }, fmtAgo(n.mtime), snippet(n.path))))
      : [h('div', { class: 'empty-state small' }, notes.length ? 'No notes match' : 'No notes yet')]));
  }

  function snippet(p) {
    const t = contents.get(p);
    if (!t) return null;
    const s = t.replace(/^#.*$/m, '').replace(/\[[ xX]\]/g, ' ').replace(/[#*_`>\[\]-]/g, ' ').replace(/\s+/g, ' ').trim().slice(0, 90);
    return s ? h('span', { class: 'notes-snippet' }, s) : null;
  }

  // Full-text search needs the contents; fetch them once (notes are small).
  async function loadContents() {
    await Promise.all(notes.filter((n) => !contents.has(n.path) && n.size < 512 << 10).map(async (n) => {
      try {
        const res = await fetch(fileUrl(n.path, true, '&text=1'));
        if (res.ok) contents.set(n.path, await res.text());
      } catch { /* skip */ }
    }));
    renderList();
  }

  search.addEventListener('input', debounce(() => { filter = search.value.trim(); renderList(); }, 120));

  // ---------- open, edit, save ----------

  async function open(p) {
    if (current?.path === p) return;
    await flush();
    try {
      const res = await fetch(fileUrl(p, true, '&text=1'));
      if (!res.ok) throw new Error(res.status === 404 ? 'That note no longer exists' : res.statusText);
      const text = await res.text();
      current = { path: p, text, saved: text, mtime: notes.find((n) => n.path === p)?.mtime };
      contents.set(p, text);
      show();
    } catch (err) { report(err); loadList(); }
  }

  function show() {
    const has = !!current;
    main.classList.toggle('hidden', !has);
    empty.classList.toggle('hidden', has);
    if (!has) { win.setTitle('Notes'); renderList(); return; }
    titleIn.value = noteName(current.path);
    area.value = current.text;
    applyMode();
    renderPreview();
    setStatus();
    win.setTitle(`Notes — ${noteName(current.path)}`);
    renderList();
  }

  function applyMode() {
    editor.dataset.mode = mode;
    modeSeg.replaceChildren(...MODES.map(([id, label, ic]) => h('button', {
      class: `seg-btn ${mode === id ? 'active' : ''}`, title: label, 'aria-label': label,
      onclick: () => { mode = id; try { localStorage.setItem('us.notes.mode', id); } catch { /* ignore */ } applyMode(); renderPreview(); },
    }, icon(ic, 15))));
    tools.classList.toggle('hidden', mode === 'preview');
  }

  function renderPreview() {
    if (mode === 'edit' || !current) return;
    preview.innerHTML = renderMarkdown(area.value) || '<p class="muted">Nothing here yet.</p>'; // escaped by renderMarkdown
  }

  const dirty = () => current && area.value !== current.saved;
  function setStatus(text) {
    status.textContent = text ?? (dirty() ? 'Editing…' : 'Saved');
  }

  const autosave = debounce(() => save(), 700);
  const preview2 = debounce(renderPreview, 120);
  function changed() {
    if (!current) return;
    current.text = area.value;
    setStatus();
    preview2();
    autosave();
  }
  area.addEventListener('input', changed);
  area.addEventListener('keydown', (e) => {
    const k = e.key.toLowerCase();
    if ((e.ctrlKey || e.metaKey) && k === 's') { e.preventDefault(); save(); }
    else if ((e.ctrlKey || e.metaKey) && k === 'b') { e.preventDefault(); fmt('**', '**', 'bold')(); }
    else if ((e.ctrlKey || e.metaKey) && k === 'i') { e.preventDefault(); fmt('*', '*', 'italic')(); }
    else if (e.key === 'Enter' && !e.shiftKey) {
      // Continue lists and task lists on Enter.
      const { selectionStart: s, value } = area;
      const start = value.lastIndexOf('\n', s - 1) + 1;
      const line = value.slice(start, s);
      const m = line.match(/^(\s*)([-*+]|\d+[.)])(\s+\[[ xX]\])?\s+/);
      if (!m) return;
      e.preventDefault();
      if (line.trim() === m[0].trim()) { area.setRangeText('', start, s, 'end'); changed(); return; } // empty item ends the list
      const bullet = /\d/.test(m[2]) ? `${parseInt(m[2], 10) + 1}${m[2].slice(-1)}` : m[2];
      area.setRangeText(`\n${m[1]}${bullet}${m[3] ? ' [ ]' : ''} `, s, area.selectionEnd, 'end');
      changed();
    }
  });

  // Tick boxes straight in the preview.
  preview.addEventListener('change', (e) => {
    const box = e.target.closest('input[type=checkbox][data-line]');
    if (!box || !current) return;
    area.value = toggleTask(area.value, Number(box.dataset.line), box.checked);
    changed();
  });

  async function save() {
    if (!current || !dirty()) return saving;
    const { path } = current;
    const text = area.value;
    setStatus('Saving…');
    saving = api('POST', '/api/files/write', { path, content: text }).then(() => {
      if (current?.path === path) current.saved = text;
      contents.set(path, text);
      const n = notes.find((x) => x.path === path);
      setStatus();
      // Learn the server's new modification time, so this save is not mistaken for an edit elsewhere.
      return loadList().then(() => { if (current?.path === path) current.mtime = notes.find((n) => n.path === path)?.mtime; });
    }, (err) => { setStatus('Not saved'); report(err); });
    return saving;
  }
  const flush = async () => { await save(); await saving; };

  // Renaming the title renames the file.
  titleIn.addEventListener('change', async () => {
    if (!current) return;
    const title = safeTitle(titleIn.value);
    const to = `${DIR}/${title}.md`;
    if (to === current.path) { titleIn.value = title; return; }
    await flush();
    try {
      await api('POST', '/api/files/rename', { from: current.path, to });
      contents.set(to, contents.get(current.path));
      contents.delete(current.path);
      current.path = to;
      await loadList();
      show();
    } catch (err) {
      titleIn.value = noteName(current.path);
      report(err.status === 409 ? new Error('A note with that title already exists') : err);
    }
  });
  titleIn.addEventListener('keydown', (e) => { if (e.key === 'Enter') { e.preventDefault(); area.focus(); } });

  async function create() {
    await flush();
    try {
      await api('POST', '/api/files/mkdir', { path: DIR }).catch((err) => { if (err.status !== 409) throw err; });
      const taken = new Set(notes.map((n) => n.title.toLowerCase()));
      let title = 'Untitled';
      for (let i = 2; taken.has(title.toLowerCase()); i++) title = `Untitled ${i}`;
      const path = `${DIR}/${title}.md`;
      await api('POST', '/api/files/write', { path, content: '' });
      await loadList();
      current = { path, text: '', saved: '' };
      contents.set(path, '');
      if (mode === 'preview') mode = 'split';
      show();
      titleIn.focus();
      titleIn.select();
    } catch (err) { report(err); }
  }

  async function remove(p) {
    if (!(await confirmDialog('Delete this note?', `"${noteName(p)}" will be deleted.`, 'Delete'))) return;
    try {
      await api('POST', '/api/files/delete', { path: p });
      contents.delete(p);
      if (current?.path === p) current = null;
      await loadList();
      show();
    } catch (err) { report(err); }
  }

  function noteMenu(e) {
    const r = e.currentTarget.getBoundingClientRect();
    menu(r.right - 200, r.bottom + 6, [
      { label: 'New note', icon: 'plus', action: () => create() },
      { label: 'Download', icon: 'download', action: () => { const a = h('a', { href: fileUrl(current.path), download: `${noteName(current.path)}.md` }); document.body.append(a); a.click(); a.remove(); } },
      { label: 'Copy as text', icon: 'clipboard', action: () => navigator.clipboard.writeText(area.value).then(() => toast('Copied'), report) },
      '-',
      { label: 'Delete note', icon: 'trash', danger: true, action: () => remove(current.path) },
    ]);
  }

  // Pick up edits made on another device (only when the file really changed, and never
  // over unsaved typing here).
  let lastCheck = 0;
  async function checkElsewhere() {
    if (Date.now() - lastCheck < 5000 || dirty()) return;
    lastCheck = Date.now();
    await loadList();
    const n = current && notes.find((x) => x.path === current.path);
    if (!current || dirty()) return;
    if (!n) { current = null; show(); return; }
    if (n.mtime !== current.mtime) {
      const p = current.path, pos = area.selectionStart;
      current = null;
      await open(p);
      area.setSelectionRange(pos, pos);
    }
  }
  const onVisible = () => { if (!document.hidden) checkElsewhere(); };
  document.addEventListener('visibilitychange', onVisible);

  // ---------- start ----------

  (async () => {
    await loadList();
    loadContents();
    const want = args.restore?.path || args.path;
    if (want && notes.some((n) => n.path === want)) await open(want);
    else if (notes[0] && !matchMedia('(max-width: 760px)').matches) await open(notes[0].path);
    else show();
  })();

  return {
    onArgs(a) { if (a.path) open(a.path); },
    async onClose() { await flush(); return true; },
    onDestroy() { document.removeEventListener('visibilitychange', onVisible); },
    onFocus() { checkElsewhere(); },
    saveState: () => ({ path: current?.path || null }),
  };
}
