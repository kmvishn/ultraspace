// Shared clipboard: text sent from any device shows up on all the others, live.
import { h, icon, api, report, toast, emit, on, fmtAgo } from './core.js';
import { device } from './session.js';

let source = null;
let items = [];
let panel = null;

// startEvents opens the live event stream from the server (one per desktop).
export function startEvents() {
  stopEvents();
  source = new EventSource('/api/events');
  source.addEventListener('clipboard', (e) => {
    const { item, origin } = JSON.parse(e.data);
    items = [item, ...items.filter((x) => x.id !== item.id && x.text !== item.text)];
    emit('clipboard', items);
    if (origin !== device.id) incoming(item);
  });
  source.addEventListener('clipboard-changed', () => refresh());
  refresh();
}

export function stopEvents() {
  source?.close();
  source = null;
  closePanel();
}

async function refresh() {
  try { items = (await api('GET', '/api/clipboard')).items; emit('clipboard', items); } catch { /* signed out */ }
}

// share sends text to the shared clipboard (used by Copy in the Terminal and Browser too).
export async function share(text) {
  if (!text) return;
  try {
    await api('POST', '/api/clipboard', { text, device: device.name, origin: device.id });
  } catch (err) { report(err); }
}

async function copyLocal(text) {
  try {
    await navigator.clipboard.writeText(text);
    toast('Copied to this device');
  } catch {
    toast('This browser blocked copying; select the text and press Ctrl+C', 'error');
  }
}

function incoming(item) {
  const preview = item.text.length > 80 ? `${item.text.slice(0, 80)}…` : item.text;
  let box = document.getElementById('toasts');
  if (!box) { box = h('div', { id: 'toasts', 'aria-live': 'polite' }); document.body.append(box); }
  const close = () => { el.classList.add('out'); setTimeout(() => el.remove(), 300); };
  const el = h('div', { class: 'toast notice clip-toast' },
    h('div', { class: 'notice-text' }, icon('clipboard', 16), h('span', null, h('b', null, `From ${item.device || 'another device'}`), h('span', { class: 'clip-preview' }, preview))),
    h('div', { class: 'notice-actions' },
      h('button', { class: 'btn small', onclick: close }, 'Dismiss'),
      h('button', { class: 'btn small primary', onclick: () => { close(); copyLocal(item.text); } }, 'Copy')));
  box.append(el);
  setTimeout(close, 12000);
}

// ---------- the menu-bar panel ----------

function closePanel() {
  panel?.remove();
  panel = null;
  document.removeEventListener('pointerdown', outside, true);
}

function outside(e) {
  if (panel && !panel.contains(e.target) && !e.target.closest('.mb-clip')) closePanel();
}

export function togglePanel(anchor) {
  if (panel) { closePanel(); return; }
  const input = h('textarea', { class: 'input clip-input', rows: 3, placeholder: 'Type or paste text to send to your other devices', 'aria-label': 'Text to share' });
  const send = async () => {
    const text = input.value;
    if (!text.trim()) return;
    await share(text);
    input.value = '';
    toast('Sent to your other devices');
  };
  const pasteBtn = h('button', {
    class: 'btn small', title: 'Read this device\'s clipboard',
    onclick: async () => {
      try { input.value = await navigator.clipboard.readText(); input.focus(); } catch { toast('This browser blocked reading the clipboard; press Ctrl+V in the box', 'error'); input.focus(); }
    },
  }, icon('download', 14), 'Paste here');
  const list = h('div', { class: 'clip-list' });
  const render = () => {
    list.replaceChildren(...(items.length ? items.map((it) => h('div', { class: 'clip-item' },
      h('div', { class: 'clip-text' }, it.text),
      h('div', { class: 'clip-meta' }, h('span', null, `${it.device || 'Unknown'} · ${fmtAgo(it.t)}`),
        h('span', { class: 'tb-spacer' }),
        h('button', { class: 'icon-btn', title: 'Copy to this device', 'aria-label': 'Copy', onclick: () => copyLocal(it.text) }, icon('fileText', 14)),
        h('button', { class: 'icon-btn', title: 'Remove', 'aria-label': 'Remove', onclick: () => api('DELETE', `/api/clipboard/${it.id}`).catch(report) }, icon('trash', 14)))))
      : [h('div', { class: 'empty-state small' }, 'Nothing shared yet. Copy in the Terminal or Browser, or send text from here.')]));
  };
  panel = h('div', { class: 'clip-panel glass', role: 'dialog', 'aria-label': 'Shared clipboard' },
    h('div', { class: 'clip-head' }, icon('clipboard', 15), h('b', null, 'Shared clipboard'), h('span', { class: 'tb-spacer' }),
      h('button', { class: 'link-btn danger', onclick: () => api('DELETE', '/api/clipboard').catch(report) }, 'Clear')),
    input,
    h('div', { class: 'clip-actions' }, pasteBtn, h('span', { class: 'tb-spacer' }), h('button', { class: 'btn small primary', onclick: send }, 'Send')),
    list);
  input.addEventListener('keydown', (e) => { if (e.key === 'Enter' && (e.ctrlKey || e.metaKey)) { e.preventDefault(); send(); } });
  const off = on('clipboard', render);
  const obs = new MutationObserver(() => { if (!panel?.isConnected) { off(); obs.disconnect(); } });
  render();
  document.body.append(panel);
  obs.observe(document.body, { childList: true });
  const r = anchor.getBoundingClientRect();
  panel.style.top = `${r.bottom + 8}px`;
  panel.style.right = `${Math.max(8, innerWidth - r.right - 8)}px`;
  document.addEventListener('pointerdown', outside, true);
  input.focus();
}
