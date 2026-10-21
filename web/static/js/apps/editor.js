import { h, icon, api, report, toast, fileUrl, baseOf, dirOf, fmtBytes } from '../core.js';
import { registerApp, confirmDiscard } from '../wm.js';

registerApp({
  id: 'editor', name: 'Text Editor', icon: 'code', color: ['#a78bfa', '#7c3aed'], width: 820, height: 600, single: false, dock: false,
  mount(win, args) { return mountEditor(win, args); },
});

// Unsaved text up to this size travels with the session to other devices.
const MAX_DRAFT = 256 * 1024;

function mountEditor(win, args) {
  const restored = args.restore || null;
  const path = restored?.path || args.path;
  let saved = '';
  let loaded = false;
  let wrap = true;
  const area = h('textarea', { class: 'editor-area', spellcheck: 'false', autocapitalize: 'off', autocomplete: 'off', 'aria-label': 'File contents', disabled: true });
  const pos = h('span');
  const info = h('span');
  const state = h('span', { class: 'ed-state' });
  const saveBtn = h('button', { class: 'btn primary small', onclick: () => save(), disabled: true }, icon('save', 14), 'Save');
  const wrapBtn = h('button', { class: 'icon-btn on', title: 'Word wrap', 'aria-label': 'Toggle word wrap', onclick: () => { wrap = !wrap; area.classList.toggle('nowrap', !wrap); wrapBtn.classList.toggle('on', wrap); } }, icon('wrap', 16));

  win.body.append(h('div', { class: 'app app-editor' },
    h('div', { class: 'toolbar' }, h('span', { class: 'ed-path', title: path }, icon('fileText', 15), h('span', null, dirOf(path) === '/' ? '' : dirOf(path) + '/'), h('b', null, baseOf(path))),
      h('div', { class: 'tb-spacer' }), state, wrapBtn, saveBtn),
    area,
    h('div', { class: 'statusbar' }, pos, h('div', { class: 'tb-spacer' }), info)));

  const dirty = () => area.value !== saved;
  const refresh = () => {
    const d = dirty();
    saveBtn.disabled = !d;
    state.textContent = d ? 'Edited' : '';
    win.setTitle(`${d ? '• ' : ''}${baseOf(path)}`);
    const before = area.value.slice(0, area.selectionStart);
    const line = before.split('\n').length;
    const col = before.length - before.lastIndexOf('\n');
    pos.textContent = `Ln ${line}, Col ${col}`;
    info.textContent = `${area.value.split('\n').length} lines · ${fmtBytes(new Blob([area.value]).size)} · UTF-8`;
  };

  async function load() {
    try {
      const res = await fetch(fileUrl(path, true, '&text=1'), { headers: { 'X-Ultraspace': '1' } });
      if (!res.ok) throw new Error(res.status === 404 ? 'File not found' : res.statusText);
      const text = await res.text();
      if (text.includes('\u0000')) throw new Error('This looks like a binary file, so it cannot be edited as text.');
      saved = text;
      area.value = typeof restored?.draft === 'string' ? restored.draft : text;
      area.disabled = false;
      area.focus();
      const at = Math.min(restored?.cursor || 0, area.value.length);
      area.setSelectionRange(at, at);
    } catch (err) {
      if (typeof restored?.draft === 'string') {
        // The file is gone or unreadable, but the unsaved text survived; saving recreates the file.
        saved = '';
        area.value = restored.draft;
        area.disabled = false;
        toast(`${baseOf(path)}: ${err.message}. Your unsaved text was kept.`, 'error');
      } else {
        area.value = '';
        area.placeholder = err.message;
      }
    }
    loaded = true;
    refresh();
  }

  async function save() {
    if (!dirty()) return;
    const content = area.value;
    saveBtn.disabled = true;
    try {
      await api('POST', '/api/files/write', { path, content });
      saved = content;
      toast(`Saved ${baseOf(path)}`);
    } catch (err) { report(err); }
    refresh();
  }

  area.addEventListener('input', refresh);
  area.addEventListener('click', refresh);
  area.addEventListener('keyup', (e) => { if (e.key.startsWith('Arrow') || e.key === 'Home' || e.key === 'End') refresh(); });
  area.addEventListener('keydown', (e) => {
    if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 's') { e.preventDefault(); save(); return; }
    if (e.key === 'Tab' && !e.ctrlKey && !e.altKey) {
      e.preventDefault();
      const { selectionStart: s, selectionEnd: en } = area;
      area.setRangeText('\t', s, en, 'end');
      refresh();
    }
  });

  load();
  return {
    async onClose() { return !dirty() || confirmDiscard(path); },
    onFocus() { if (!area.disabled) area.focus({ preventScroll: true }); },
    // For Ultra Chat Bot: the selection if there is one, else the whole file.
    context() {
      const sel = area.value.slice(area.selectionStart, area.selectionEnd);
      return { title: baseOf(path), text: sel || area.value };
    },
    saveState() {
      if (!loaded) return restored || { path };
      const st = { path, cursor: area.selectionStart };
      if (dirty() && area.value.length <= MAX_DRAFT) st.draft = area.value;
      return st;
    },
  };
}
