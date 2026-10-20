// Window manager: app registry, windows (drag, resize, snap, minimize, maximize) and focus.
import { h, icon, emit, on, kindOf, isEditable, downloadFile, baseOf, confirmDialog, closeMenu } from './core.js';

const apps = new Map();
const windows = [];
let zTop = 100;
let layer = null; // element windows live in
let cascade = 0;

const MENUBAR = 32;
const DOCK = 86;
const isMobile = () => innerWidth < 760;

// def: { id, name, icon, color: [from, to], width, height, single, dock, mount(win, args) -> { onArgs, onClose, onFocus } }
export function registerApp(def) { apps.set(def.id, { single: true, dock: true, width: 860, height: 560, ...def }); }
export const allApps = () => [...apps.values()];
export const appById = (id) => apps.get(id);
export const windowsOf = (id) => windows.filter((w) => w.app.id === id);
export const focusedWindow = () => windows.find((w) => w.focused) || null;
// All windows, bottom of the stack first.
export const allWindows = () => [...windows].sort((a, b) => (Number(a.el.style.zIndex) || 0) - (Number(b.el.style.zIndex) || 0));

export function initWM(container) {
  layer = container;
  addEventListener('resize', () => windows.forEach((w) => w.clamp()));
}

export function closeAllWindows() {
  for (const w of [...windows]) w.destroy();
}

// opts.geom restores a saved window: { x, y, w, h, max, snap, min }.
export function openApp(id, args = {}, opts = {}) {
  const app = apps.get(id);
  if (!app) return null;
  closeMenu();
  if (app.single && !opts.geom) {
    const existing = windows.find((w) => w.app.id === id);
    if (existing) {
      existing.restore();
      existing.focus();
      existing.handle.onArgs?.(args);
      return existing;
    }
  }
  const win = new Win(app, opts.geom);
  win.handle = app.mount(win, args) || {};
  win.focus();
  if (opts.geom?.min) win.minimize();
  return win;
}

// Opens a file in the app that suits it, or downloads it.
export function openFile(entry, context = {}) {
  const kind = kindOf(entry);
  if (kind === 'image' || kind === 'video') return openApp('viewer', { path: entry.path, siblings: context.siblings });
  if (kind === 'audio') return openApp('music', { play: entry.path });
  if (isEditable(entry) && (entry.size ?? 0) <= 2 * 1024 * 1024) return openApp('editor', { path: entry.path });
  downloadFile(entry.path);
  return null;
}

function geomKey(id) { return `us.geom.${id}`; }

function loadGeom(id) {
  try { return JSON.parse(localStorage.getItem(geomKey(id)) || 'null'); } catch { return null; }
}

function saveGeom(id, g) {
  try { localStorage.setItem(geomKey(id), JSON.stringify(g)); } catch { /* storage unavailable */ }
}

class Win {
  constructor(app, geom = null) {
    this.app = app;
    this.handle = {};
    this.focused = false;
    this.minimized = false;
    this.maximized = false;
    this.snap = null;

    this.titleEl = h('div', { class: 'win-title' }, app.name);
    this.actionsEl = h('div', { class: 'win-actions' });
    const lights = h('div', { class: 'lights' },
      h('button', { class: 'light close', 'aria-label': 'Close', onclick: (e) => { e.stopPropagation(); this.close(); } }, icon('x', 9)),
      h('button', { class: 'light min', 'aria-label': 'Minimize', onclick: (e) => { e.stopPropagation(); this.minimize(); } }, icon('minus', 9)),
      h('button', { class: 'light max', 'aria-label': 'Maximize', onclick: (e) => { e.stopPropagation(); this.toggleMax(); } }, icon('square', 8)));
    this.bar = h('div', { class: 'win-bar' }, lights, this.titleEl, this.actionsEl);
    this.body = h('div', { class: 'win-body' });
    this.el = h('div', { class: 'win opening', dataset: { app: app.id }, role: 'dialog', 'aria-label': app.name },
      this.bar, this.body,
      ['n', 's', 'e', 'w', 'ne', 'nw', 'se', 'sw'].map((d) => h('div', { class: `rz rz-${d}`, dataset: { dir: d } })));

    this.el.addEventListener('pointerdown', () => this.focus(), true);
    this.bar.addEventListener('dblclick', (e) => { if (!e.target.closest('button, input')) this.toggleMax(); });
    this.bar.addEventListener('pointerdown', (e) => this.startDrag(e));
    this.el.querySelectorAll('.rz').forEach((r) => r.addEventListener('pointerdown', (e) => this.startResize(e, r.dataset.dir)));

    const saved = geom || loadGeom(app.id);
    const area = this.area();
    const w = Math.min(saved?.w || app.width, area.w - 16);
    const hgt = Math.min(saved?.h || app.height, area.h - 16);
    let x, y;
    if (geom || (saved && !windowsOf(app.id).length)) {
      ({ x, y } = saved);
    } else {
      x = Math.round((area.w - w) / 2) + cascade;
      y = Math.round(area.y + (area.h - hgt) / 4) + cascade;
      cascade = (cascade + 28) % 140;
    }
    this.rect = { x, y, w, h: hgt };
    this.clamp();
    if (saved?.max) this.setMax(true);
    else if (geom?.snap) this.setSnap(geom.snap);

    windows.push(this);
    layer.append(this.el);
    requestAnimationFrame(() => this.el.classList.remove('opening'));
    emit('windows');
  }

  area() {
    return { x: 0, y: MENUBAR, w: innerWidth, h: innerHeight - MENUBAR - (isMobile() ? DOCK - 10 : DOCK) };
  }

  apply() {
    const s = this.el.style;
    s.left = `${this.rect.x}px`;
    s.top = `${this.rect.y}px`;
    s.width = `${this.rect.w}px`;
    s.height = `${this.rect.h}px`;
  }

  clamp() {
    const a = this.area();
    this.rect.w = Math.min(this.rect.w, a.w);
    this.rect.h = Math.min(this.rect.h, a.h);
    this.rect.x = Math.min(Math.max(this.rect.x, -this.rect.w + 120), a.w - 120);
    this.rect.y = Math.min(Math.max(this.rect.y, a.y), a.y + a.h - 40);
    this.apply();
    this.handle.onResize?.();
  }

  persist() {
    saveGeom(this.app.id, { ...this.rect, max: this.maximized });
    emit('windows');
  }

  // Where the window is, for saving the session.
  geom() {
    const r = this.rect;
    return { x: Math.round(r.x), y: Math.round(r.y), w: Math.round(r.w), h: Math.round(r.h), max: this.maximized, snap: this.snap, min: this.minimized };
  }

  setTitle(text) {
    this.titleEl.textContent = text;
    this.el.setAttribute('aria-label', text);
    emit('windows');
  }

  focus() {
    if (this.focused && this.el.style.zIndex == zTop) return;
    for (const w of windows) if (w !== this && w.focused) { w.focused = false; w.el.classList.remove('focused'); }
    this.focused = true;
    this.el.classList.add('focused');
    this.el.style.zIndex = ++zTop;
    this.handle.onFocus?.();
    emit('windows');
  }

  async close() {
    if (this.handle.onClose && (await this.handle.onClose()) === false) return;
    this.el.classList.add('closing');
    setTimeout(() => this.destroy(), 160);
  }

  destroy() {
    this.handle.onDestroy?.();
    this.el.remove();
    const i = windows.indexOf(this);
    if (i >= 0) windows.splice(i, 1);
    const next = windows.filter((w) => !w.minimized).sort((a, b) => b.el.style.zIndex - a.el.style.zIndex)[0];
    if (next) next.focus();
    emit('windows');
  }

  minimize() {
    this.minimized = true;
    this.focused = false;
    this.el.classList.remove('focused');
    this.el.classList.add('minimized');
    const next = windows.filter((w) => !w.minimized).sort((a, b) => b.el.style.zIndex - a.el.style.zIndex)[0];
    if (next) next.focus();
    emit('windows');
  }

  restore() {
    if (!this.minimized) return;
    this.minimized = false;
    this.el.classList.remove('minimized');
    emit('windows');
  }

  setMax(on) {
    this.maximized = on;
    this.snap = null;
    this.el.classList.toggle('maximized', on);
    this.el.classList.remove('snap-left', 'snap-right');
    requestAnimationFrame(() => this.handle.onResize?.());
  }

  toggleMax() {
    this.setMax(!this.maximized);
    this.persist();
  }

  setSnap(side) {
    this.maximized = false;
    this.el.classList.remove('maximized');
    this.snap = side;
    this.el.classList.toggle('snap-left', side === 'left');
    this.el.classList.toggle('snap-right', side === 'right');
    requestAnimationFrame(() => this.handle.onResize?.());
  }

  startDrag(e) {
    if (e.button !== 0 || e.target.closest('button, input, select, .win-actions') || isMobile()) return;
    e.preventDefault();
    const start = { px: e.clientX, py: e.clientY, x: this.rect.x, y: this.rect.y };
    let moved = false;
    let snapTo = null;
    const preview = h('div', { class: 'snap-preview' });
    this.bar.setPointerCapture(e.pointerId);
    this.el.classList.add('dragging');

    const move = (ev) => {
      const dx = ev.clientX - start.px, dy = ev.clientY - start.py;
      if (!moved && Math.hypot(dx, dy) < 4) return;
      if (!moved && (this.maximized || this.snap)) {
        // Pull out of a maximized/snapped state, keeping the cursor at the same spot on the title bar.
        const ratio = (start.px - this.el.getBoundingClientRect().left) / this.el.offsetWidth;
        this.setMax(false);
        this.setSnap(null);
        start.x = start.px - this.rect.w * ratio;
        start.y = MENUBAR;
      }
      moved = true;
      const a = this.area();
      this.rect.x = start.x + dx;
      this.rect.y = Math.min(Math.max(start.y + dy, a.y), a.y + a.h - 40);
      this.apply();
      const next = ev.clientY <= MENUBAR + 2 ? 'max' : ev.clientX <= 2 ? 'left' : ev.clientX >= innerWidth - 3 ? 'right' : null;
      if (next !== snapTo) {
        snapTo = next;
        preview.className = `snap-preview ${snapTo || ''}`;
        if (snapTo && !preview.isConnected) layer.append(preview);
        if (!snapTo) preview.remove();
      }
    };
    const up = () => {
      this.bar.removeEventListener('pointermove', move);
      this.bar.removeEventListener('pointerup', up);
      this.bar.removeEventListener('pointercancel', up);
      this.el.classList.remove('dragging');
      preview.remove();
      if (snapTo === 'max') this.setMax(true);
      else if (snapTo) this.setSnap(snapTo);
      if (moved) this.persist();
    };
    this.bar.addEventListener('pointermove', move);
    this.bar.addEventListener('pointerup', up);
    this.bar.addEventListener('pointercancel', up);
  }

  startResize(e, dir) {
    if (e.button !== 0 || this.maximized || isMobile()) return;
    e.preventDefault();
    e.stopPropagation();
    if (this.snap) {
      const r = this.el.getBoundingClientRect();
      this.rect = { x: r.left, y: r.top, w: r.width, h: r.height };
      this.setSnap(null);
      this.apply();
    }
    const target = e.currentTarget;
    target.setPointerCapture(e.pointerId);
    const start = { px: e.clientX, py: e.clientY, ...this.rect };
    const minW = this.app.minWidth || 380, minH = this.app.minHeight || 260;
    this.el.classList.add('dragging');
    const move = (ev) => {
      const dx = ev.clientX - start.px, dy = ev.clientY - start.py;
      const a = this.area();
      if (dir.includes('e')) this.rect.w = Math.max(minW, start.w + dx);
      if (dir.includes('s')) this.rect.h = Math.max(minH, Math.min(start.h + dy, a.y + a.h - start.y));
      if (dir.includes('w')) { const w = Math.max(minW, start.w - dx); this.rect.x = start.x + start.w - w; this.rect.w = w; }
      if (dir.includes('n')) {
        const y = Math.max(a.y, Math.min(start.y + dy, start.y + start.h - minH));
        this.rect.h = start.h + start.y - y;
        this.rect.y = y;
      }
      this.apply();
      this.handle.onResize?.();
    };
    const up = () => {
      target.removeEventListener('pointermove', move);
      target.removeEventListener('pointerup', up);
      this.el.classList.remove('dragging');
      this.persist();
    };
    target.addEventListener('pointermove', move);
    target.addEventListener('pointerup', up);
  }
}

// Ask before closing a window that has unsaved work.
export async function confirmDiscard(name) {
  return confirmDialog('Unsaved changes', `Discard your changes to "${baseOf(name)}"?`, 'Discard');
}

export { on };
