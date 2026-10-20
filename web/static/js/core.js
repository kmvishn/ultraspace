// Shared helpers: DOM builder, API client, formatting, menus, dialogs, settings, live system stats.
import { icon } from './icons.js';

export { icon };

// ---------- DOM ----------

export function h(tag, attrs, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v == null || v === false) continue;
    if (k === 'class') el.className = v;
    else if (k === 'style') Object.assign(el.style, v); // CSSOM, allowed by the CSP
    else if (k === 'dataset') Object.assign(el.dataset, v);
    else if (k.startsWith('on')) el.addEventListener(k.slice(2), v);
    else if (k === 'value' || k === 'checked' || k === 'disabled') el[k] = v;
    else el.setAttribute(k, v === true ? '' : v);
  }
  for (const c of children.flat(Infinity)) {
    if (c == null || c === false) continue;
    el.append(c instanceof Node ? c : String(c));
  }
  return el;
}

export const $ = (sel, root = document) => root.querySelector(sel);

// ---------- events ----------

export const bus = new EventTarget();
export const emit = (name, detail) => bus.dispatchEvent(new CustomEvent(name, { detail }));
export function on(name, fn) {
  const handler = (e) => fn(e.detail);
  bus.addEventListener(name, handler);
  return () => bus.removeEventListener(name, handler);
}

// ---------- API ----------

export class ApiError extends Error {
  constructor(message, status) { super(message); this.status = status; }
}

export async function api(method, url, body) {
  const opts = { method, headers: { 'X-Ultraspace': '1' }, credentials: 'same-origin' };
  if (body !== undefined) {
    opts.headers['Content-Type'] = 'application/json';
    opts.body = JSON.stringify(body);
  }
  const res = await fetch(url, opts);
  const data = (res.headers.get('Content-Type') || '').includes('application/json') ? await res.json() : null;
  if (!res.ok) {
    const err = new ApiError((data && data.error) || res.statusText || 'Request failed', res.status);
    if (res.status === 401) emit('unauthorized');
    throw err;
  }
  return data;
}

export function report(err) {
  if (err instanceof ApiError && err.status === 401) return;
  toast(err.message || String(err), 'error');
}

// ---------- formatting ----------

export function fmtBytes(n, digits = 1) {
  if (!Number.isFinite(n)) return '–';
  const units = ['B', 'KB', 'MB', 'GB', 'TB'];
  let i = 0;
  while (Math.abs(n) >= 1024 && i < units.length - 1) { n /= 1024; i++; }
  return `${n.toFixed(i === 0 || n >= 100 ? 0 : digits)} ${units[i]}`;
}

export function fmtDuration(s) {
  s = Math.max(0, Math.floor(s));
  const d = Math.floor(s / 86400), hr = Math.floor((s % 86400) / 3600), m = Math.floor((s % 3600) / 60);
  if (d) return `${d}d ${hr}h`;
  if (hr) return `${hr}h ${m}m`;
  return `${m}m`;
}

export function fmtClock(sec) {
  if (!Number.isFinite(sec)) return '0:00';
  sec = Math.floor(sec);
  const m = Math.floor(sec / 60), s = String(sec % 60).padStart(2, '0');
  return m >= 60 ? `${Math.floor(m / 60)}:${String(m % 60).padStart(2, '0')}:${s}` : `${m}:${s}`;
}

const dateFmt = new Intl.DateTimeFormat(undefined, { dateStyle: 'medium', timeStyle: 'short' });
export const fmtDate = (v) => dateFmt.format(new Date(v));

const rtf = new Intl.RelativeTimeFormat(undefined, { numeric: 'auto' });
export function fmtAgo(v) {
  const diff = (new Date(v).getTime() - Date.now()) / 1000;
  const abs = Math.abs(diff);
  if (abs < 60) return rtf.format(Math.round(diff), 'second');
  if (abs < 3600) return rtf.format(Math.round(diff / 60), 'minute');
  if (abs < 86400) return rtf.format(Math.round(diff / 3600), 'hour');
  if (abs < 86400 * 30) return rtf.format(Math.round(diff / 86400), 'day');
  return fmtDate(v);
}

export function debounce(fn, ms) {
  let t;
  return (...args) => { clearTimeout(t); t = setTimeout(() => fn(...args), ms); };
}

// ---------- files ----------

const KINDS = {
  image: ['png', 'jpg', 'jpeg', 'gif', 'webp', 'avif', 'bmp', 'ico'],
  video: ['mp4', 'webm', 'mov', 'm4v', 'ogv'],
  audio: ['mp3', 'flac', 'ogg', 'oga', 'opus', 'wav', 'm4a', 'aac'],
  archive: ['zip', 'tar', 'gz', 'tgz', 'xz', 'zst', '7z', 'rar', 'bz2'],
  code: ['js', 'mjs', 'ts', 'tsx', 'jsx', 'go', 'py', 'rs', 'c', 'h', 'cpp', 'java', 'rb', 'php', 'sh', 'bash', 'zsh',
    'css', 'html', 'json', 'yaml', 'yml', 'toml', 'xml', 'sql', 'service', 'conf', 'ini', 'env', 'lua', 'kt', 'swift'],
  text: ['txt', 'md', 'log', 'csv', 'tsv', 'cfg', 'nfo', 'srt', 'rst'],
  pdf: ['pdf'],
};
const EXT_KIND = Object.fromEntries(Object.entries(KINDS).flatMap(([k, list]) => list.map((e) => [e, k])));

export const extOf = (name) => (name.includes('.') ? name.split('.').pop().toLowerCase() : '');
export const kindOf = (entry) => (entry.dir ? 'folder' : EXT_KIND[extOf(entry.name)] || 'file');
export const isEditable = (entry) => !entry.dir && (['text', 'code'].includes(kindOf(entry)) || !extOf(entry.name));

export const KIND_ICON = {
  folder: 'folder', image: 'image', video: 'film', audio: 'music', archive: 'archive',
  code: 'code', text: 'fileText', pdf: 'fileText', file: 'file',
};

export const joinPath = (dir, name) => `${dir.replace(/\/+$/, '')}/${name}`;
export const dirOf = (p) => p.replace(/\/[^/]*$/, '') || '/';
export const baseOf = (p) => p.replace(/^.*\//, '');
export const fileUrl = (p, inline = false, extra = '') =>
  `/api/files/download?path=${encodeURIComponent(p)}${inline ? '&inline=1' : ''}${extra}`;

export function downloadFile(p) {
  const a = h('a', { href: fileUrl(p), download: baseOf(p) });
  document.body.append(a);
  a.click();
  a.remove();
}

// ---------- toasts ----------

export function toast(msg, kind = '') {
  let box = $('#toasts');
  if (!box) { box = h('div', { id: 'toasts', 'aria-live': 'polite' }); document.body.append(box); }
  const el = h('div', { class: `toast ${kind}` }, icon(kind === 'error' ? 'info' : 'check', 16), h('span', null, msg));
  box.append(el);
  setTimeout(() => { el.classList.add('out'); setTimeout(() => el.remove(), 300); }, kind === 'error' ? 6000 : 3000);
}

// A toast that stays until one of its buttons is pressed. actions: [{ label, primary, run }]
export function notice(msg, actions, iconName = 'info') {
  let box = $('#toasts');
  if (!box) { box = h('div', { id: 'toasts', 'aria-live': 'polite' }); document.body.append(box); }
  const close = () => { el.classList.add('out'); setTimeout(() => el.remove(), 300); };
  const el = h('div', { class: 'toast notice', role: 'alertdialog' },
    h('div', { class: 'notice-text' }, icon(iconName, 16), h('span', null, msg)),
    h('div', { class: 'notice-actions' }, actions.map((a) => h('button', {
      class: `btn small ${a.primary ? 'primary' : ''}`, onclick: () => { close(); a.run?.(); },
    }, a.label))));
  box.append(el);
  return close;
}

// ---------- context menus ----------

let openMenu = null;

export function closeMenu() {
  if (openMenu) { openMenu.remove(); openMenu = null; }
}

// items: [{ label, icon, action, danger, disabled, hint }] or '-' for a separator.
export function menu(x, y, items) {
  closeMenu();
  const el = h('div', { class: 'menu', role: 'menu' },
    items.filter(Boolean).map((it) => it === '-' ? h('div', { class: 'menu-sep' }) :
      h('button', {
        class: `menu-item ${it.danger ? 'danger' : ''}`, role: 'menuitem', disabled: !!it.disabled,
        onclick: (e) => { e.stopPropagation(); closeMenu(); it.action && it.action(); },
      }, it.icon ? icon(it.icon, 16) : h('span', { class: 'i' }), h('span', { class: 'menu-label' }, it.label),
      it.hint ? h('span', { class: 'menu-hint' }, it.hint) : null)));
  document.body.append(el);
  const r = el.getBoundingClientRect();
  el.style.left = `${Math.max(6, Math.min(x, innerWidth - r.width - 6))}px`;
  el.style.top = `${Math.max(6, Math.min(y, innerHeight - r.height - 6))}px`;
  openMenu = el;
  return el;
}

addEventListener('pointerdown', (e) => { if (openMenu && !openMenu.contains(e.target)) closeMenu(); }, true);
addEventListener('keydown', (e) => { if (e.key === 'Escape') closeMenu(); });
addEventListener('blur', closeMenu);

// ---------- dialogs ----------

export function dialog({ title, body, actions = [], width }) {
  const back = h('div', { class: 'dialog-back' });
  const close = () => { back.remove(); document.removeEventListener('keydown', onKey, true); };
  const onKey = (e) => { if (e.key === 'Escape') { e.stopPropagation(); close(); } };
  const box = h('div', { class: 'dialog', role: 'dialog', 'aria-modal': 'true', style: width ? { maxWidth: width } : null },
    title ? h('h3', null, title) : null,
    h('div', { class: 'dialog-body' }, body),
    actions.length ? h('div', { class: 'dialog-actions' }, actions) : null);
  back.append(box);
  back.addEventListener('pointerdown', (e) => { if (e.target === back) close(); });
  document.addEventListener('keydown', onKey, true);
  document.body.append(back);
  return close;
}

export function ask(title, label, initial = '', okLabel = 'Save') {
  return new Promise((resolve) => {
    const input = h('input', { class: 'input', type: 'text', value: initial, 'aria-label': label });
    let done = false;
    const finish = (v) => { if (!done) { done = true; close(); resolve(v); } };
    const close = dialog({
      title,
      body: h('form', { onsubmit: (e) => { e.preventDefault(); finish(input.value.trim() || null); } },
        h('label', { class: 'field-label' }, label), input),
      actions: [
        h('button', { class: 'btn', onclick: () => finish(null) }, 'Cancel'),
        h('button', { class: 'btn primary', onclick: () => finish(input.value.trim() || null) }, okLabel),
      ],
    });
    requestAnimationFrame(() => {
      input.focus();
      const dot = initial.lastIndexOf('.');
      input.setSelectionRange(0, dot > 0 ? dot : initial.length);
    });
  });
}

export function confirmDialog(title, text, okLabel = 'Delete', danger = true) {
  return new Promise((resolve) => {
    let done = false;
    const finish = (v) => { if (!done) { done = true; close(); resolve(v); } };
    const ok = h('button', { class: `btn ${danger ? 'danger-solid' : 'primary'}`, onclick: () => finish(true) }, okLabel);
    const close = dialog({
      title, body: h('p', { class: 'dialog-text' }, text),
      actions: [h('button', { class: 'btn', onclick: () => finish(false) }, 'Cancel'), ok],
    });
    requestAnimationFrame(() => ok.focus());
  });
}

// ---------- settings ----------

export const DEFAULTS = { theme: 'dark', accent: '#7c6cf2', wallpaper: 'aurora', transparency: true, filesView: 'grid' };

export const settings = {
  data: { ...DEFAULTS },
  async load() {
    try { this.data = { ...DEFAULTS, ...(await api('GET', '/api/account/settings')) }; } catch { this.data = { ...DEFAULTS }; }
    emit('settings', this.data);
  },
  get(k) { return this.data[k]; },
  set(patch) {
    Object.assign(this.data, patch);
    emit('settings', this.data);
    this.save();
  },
  save: debounce(function () {
    api('PUT', '/api/account/settings', settings.data).catch(report);
  }, 400),
};

// ---------- live system stats (shared by menu bar, widgets and the System app) ----------

export const system = {
  latest: null,
  history: [],
  timer: null,
  async start() {
    if (this.timer) return;
    try {
      const res = await api('GET', '/api/system/history');
      this.history = res.points || [];
    } catch { /* fine, history fills as we poll */ }
    const tick = async () => {
      if (document.hidden) return;
      try {
        const s = await api('GET', '/api/system');
        this.latest = s;
        const mem = s.memory.total ? (100 * s.memory.used) / s.memory.total : 0;
        this.history.push({ t: Date.now(), cpu: s.cpu.percent, mem, rx: s.network.rx_rate, tx: s.network.tx_rate });
        if (this.history.length > 180) this.history.shift();
        emit('system', s);
      } catch { /* the next tick retries */ }
    };
    tick();
    this.timer = setInterval(tick, 2000);
  },
  stop() { clearInterval(this.timer); this.timer = null; },
};

// ---------- charts ----------

function cssVar(name, el = document.documentElement) {
  return getComputedStyle(el).getPropertyValue(name).trim();
}

// series: [{ values: number[], color: string, fill: boolean }]; values share one scale.
export function drawChart(canvas, series, { max, gridLines = 3, length = 180 } = {}) {
  const dpr = devicePixelRatio || 1;
  const w = canvas.clientWidth, hgt = canvas.clientHeight;
  if (!w || !hgt) return;
  if (canvas.width !== Math.round(w * dpr) || canvas.height !== Math.round(hgt * dpr)) {
    canvas.width = Math.round(w * dpr);
    canvas.height = Math.round(hgt * dpr);
  }
  const ctx = canvas.getContext('2d');
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  ctx.clearRect(0, 0, w, hgt);
  const top = max ?? Math.max(1, ...series.flatMap((s) => s.values)) * 1.15;

  ctx.strokeStyle = cssVar('--chart-grid') || 'rgba(127,127,127,.15)';
  ctx.lineWidth = 1;
  for (let i = 1; i <= gridLines; i++) {
    const y = Math.round((hgt * i) / (gridLines + 1)) + 0.5;
    ctx.beginPath(); ctx.moveTo(0, y); ctx.lineTo(w, y); ctx.stroke();
  }
  const step = w / (length - 1);
  for (const s of series) {
    const vals = s.values.slice(-length);
    if (vals.length < 2) continue;
    const x0 = w - (vals.length - 1) * step;
    const pt = (v, i) => [x0 + i * step, hgt - 2 - (Math.min(v, top) / top) * (hgt - 4)];
    ctx.beginPath();
    vals.forEach((v, i) => { const [x, y] = pt(v, i); i ? ctx.lineTo(x, y) : ctx.moveTo(x, y); });
    if (s.fill) {
      const grad = ctx.createLinearGradient(0, 0, 0, hgt);
      grad.addColorStop(0, s.color + '55');
      grad.addColorStop(1, s.color + '00');
      ctx.save();
      ctx.lineTo(w, hgt); ctx.lineTo(x0, hgt); ctx.closePath();
      ctx.fillStyle = grad; ctx.fill();
      ctx.restore();
      ctx.beginPath();
      vals.forEach((v, i) => { const [x, y] = pt(v, i); i ? ctx.lineTo(x, y) : ctx.moveTo(x, y); });
    }
    ctx.strokeStyle = s.color;
    ctx.lineWidth = 1.75;
    ctx.lineJoin = 'round';
    ctx.stroke();
  }
}

export const accent = () => cssVar('--accent') || '#7c6cf2';
