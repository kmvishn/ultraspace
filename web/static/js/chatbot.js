// Ultra Chat Bot: a floating bubble at the bottom-right that opens a chat with hosted
// models (Gemini, Mistral, Groq, OpenRouter). Keys live on the server, answers stream in, conversations
// are saved on the server, and the open window (note, file, terminal) can be included
// as context. Optional Brave web search adds cited sources.
import { h, icon, api, report, toast, menu, confirmDialog, fmtAgo, emit, on } from './core.js';
import { focusedWindow, openApp } from './wm.js';
import { renderMarkdown } from './markdown.js';

let root = null;     // bubble + panel container
let cfg = null;      // /api/ai/config
let offs = [];

export async function startChatbot() {
  stopChatbot();
  root = h('div', { class: 'uc-root' });
  document.body.append(root);
  offs.push(on('chatbot-config', (c) => { cfg = c; renderBubble(); }));
  await loadConfig();
}

export function stopChatbot() {
  offs.forEach((f) => f());
  offs = [];
  root?.remove();
  root = null;
  panel = null;
}

async function loadConfig() {
  try { cfg = await api('GET', '/api/ai/config'); } catch { cfg = null; }
  renderBubble();
}

// ---------- bubble ----------

function renderBubble() {
  if (!root) return;
  const show = cfg?.enabled;
  let bubble = root.querySelector('.uc-bubble');
  if (!show) { bubble?.remove(); closePanel(); return; }
  if (!bubble) {
    bubble = h('button', { class: 'uc-bubble', 'aria-label': 'Ultra Chat Bot', title: 'Ultra Chat Bot (Ctrl+J)', onclick: () => togglePanel() },
      icon('sparkles', 24));
    root.append(bubble);
  }
}

addEventListener('keydown', (e) => {
  if (root && cfg?.enabled && (e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 'j') { e.preventDefault(); togglePanel(); }
});

// ---------- panel ----------

let panel = null;
let state = null; // { id, title, model, messages: [{role, content, sources?}], busy, abort }

function newState(model) {
  return { id: newID(), title: '', model, messages: [], busy: false, abort: null };
}

function newID() {
  const b = crypto.getRandomValues(new Uint8Array(8));
  return [...b].map((x) => x.toString(16).padStart(2, '0')).join('');
}

function closePanel() {
  state?.abort?.abort();
  panel?.remove();
  panel = null;
  root?.querySelector('.uc-bubble')?.classList.remove('open');
}

function togglePanel() {
  if (panel) { closePanel(); return; }
  openPanel();
}

async function openPanel() {
  root.querySelector('.uc-bubble')?.classList.add('open');
  const modelSel = h('select', { class: 'uc-model', 'aria-label': 'Model' });
  const body = h('div', { class: 'uc-body' });
  const input = h('textarea', { class: 'uc-input', rows: 1, placeholder: 'Message Ultra…', 'aria-label': 'Message' });
  const sendBtn = h('button', { class: 'uc-send', 'aria-label': 'Send', onclick: () => (state.busy ? state.abort?.abort() : send()) }, icon('send', 18));
  const searchBtn = h('button', { class: 'uc-chip hidden', title: 'Search the web with Brave first', onclick: () => { searchOn = !searchOn; searchBtn.classList.toggle('on', searchOn); } }, icon('globe', 13), 'Web');
  const ctxBtn = h('button', { class: 'uc-chip hidden', onclick: () => { ctxOn = !ctxOn; ctxBtn.classList.toggle('on', ctxOn); } });
  let searchOn = false;
  let ctxOn = false;
  let ctxWin = null;

  panel = h('div', { class: 'uc-panel', role: 'dialog', 'aria-label': 'Ultra Chat Bot' },
    h('div', { class: 'uc-head' },
      h('span', { class: 'uc-logo' }, icon('sparkles', 15)),
      h('div', { class: 'uc-titles' }, h('b', null, 'Ultra Chat Bot'), modelSel),
      h('button', { class: 'icon-btn', title: 'Conversations', 'aria-label': 'Conversations', onclick: () => showHistory() }, icon('history', 16)),
      h('button', { class: 'icon-btn', title: 'New chat', 'aria-label': 'New chat', onclick: () => { state.abort?.abort(); state = newState(modelSel.value); renderChat(); input.focus(); } }, icon('plus', 17)),
      h('button', { class: 'icon-btn', title: 'Close', 'aria-label': 'Close', onclick: () => closePanel() }, icon('x', 16))),
    body,
    h('div', { class: 'uc-foot' },
      h('div', { class: 'uc-chips' }, ctxBtn, searchBtn),
      h('div', { class: 'uc-compose' }, input, sendBtn),
      h('div', { class: 'uc-note' }, 'Messages go to the provider of the model you pick.')));
  root.append(panel);

  // Offer the focused window (note, file, terminal) as context.
  const fw = focusedWindow();
  if (fw?.handle?.context) {
    ctxWin = fw;
    ctxBtn.replaceChildren(icon('link', 13), `Include: ${fw.app.name}`);
    ctxBtn.title = 'Send what is open in this window along with your message';
    ctxBtn.classList.remove('hidden');
  }

  // ---------- models ----------
  if (!cfg?.configured) {
    modelSel.classList.add('hidden');
    body.replaceChildren(h('div', { class: 'uc-empty' }, h('span', { class: 'uc-big' }, icon('sparkles', 30)),
      h('h3', null, 'Set up Ultra Chat Bot'),
      h('p', null, 'Add a Gemini, Mistral, Groq or OpenRouter API key in Settings. Keys stay on your server.'),
      h('button', { class: 'btn primary', onclick: () => { closePanel(); openApp('settings', { section: 'chatbot' }); } }, icon('sliders', 15), 'Open settings')));
    input.disabled = true;
    return;
  }
  searchBtn.classList.toggle('hidden', !cfg.search?.configured);
  modelSel.replaceChildren(h('option', null, 'Loading models…'));
  try {
    const res = await api('GET', '/api/ai/models');
    const groups = {};
    for (const m of res.models) (groups[m.provider] ||= []).push(m);
    modelSel.replaceChildren(...Object.entries(groups).map(([p, list]) =>
      h('optgroup', { label: p }, list.map((m) => h('option', { value: m.id }, m.name)))));
    const want = state?.model || res.default_model;
    if (want && res.models.some((m) => m.id === want)) modelSel.value = want;
    else modelSel.value = pickDefault(res.models);
    const errs = Object.entries(res.errors || {});
    if (errs.length) toast(errs.map(([, e]) => e).join(' · '), 'error');
    if (!res.models.length) modelSel.replaceChildren(h('option', null, 'No models available'));
  } catch (err) { report(err); }
  modelSel.addEventListener('change', () => {
    state.model = modelSel.value;
    api('PUT', '/api/ai/config', { default_model: modelSel.value }).catch(() => {});
  });

  if (!state) state = newState(modelSel.value);
  else state.model = modelSel.value || state.model;
  renderChat();
  requestAnimationFrame(() => input.focus());

  // ---------- compose ----------
  const grow = () => { input.style.height = 'auto'; input.style.height = `${Math.min(160, input.scrollHeight)}px`; };
  input.addEventListener('input', grow);
  input.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) { e.preventDefault(); if (!state.busy) send(); }
    if (e.key === 'Escape') closePanel();
  });

  async function send(text = input.value.trim()) {
    if (!text || state.busy || !modelSel.value || !modelSel.value.includes(':')) return;
    input.value = '';
    grow();
    let content = text;
    if (ctxOn && ctxWin?.handle?.context) {
      const c = ctxWin.handle.context();
      if (c?.text) content = `${text}\n\n---\nContext from ${ctxWin.app.name}${c.title ? ` (${c.title})` : ''}:\n\`\`\`\n${c.text.slice(0, 16000)}\n\`\`\``;
      ctxOn = false;
      ctxBtn.classList.remove('on');
    }
    state.model = modelSel.value;
    if (!state.title) state.title = text.replace(/\s+/g, ' ').slice(0, 60);
    state.messages.push({ role: 'user', content, shown: text, attached: content !== text });
    const reply = { role: 'assistant', content: '', sources: null, notice: '', error: '' };
    state.messages.push(reply);
    renderChat();
    await stream(reply, searchOn);
    save();
  }

  async function stream(reply, search) {
    state.busy = true;
    sendBtn.replaceChildren(icon('square', 14));
    sendBtn.setAttribute('aria-label', 'Stop');
    const ctrl = new AbortController();
    state.abort = ctrl;
    const history = state.messages.filter((m) => m !== reply && !m.error).map((m) => ({ role: m.role, content: m.content }));
    try {
      const res = await fetch('/api/ai/chat', {
        method: 'POST', signal: ctrl.signal, credentials: 'same-origin',
        headers: { 'X-Ultraspace': '1', 'Content-Type': 'application/json' },
        body: JSON.stringify({ model: state.model, messages: history, search }),
      });
      if (!res.ok) throw new Error((await res.json().catch(() => ({}))).error || res.statusText);
      const reader = res.body.getReader();
      const dec = new TextDecoder();
      let buf = '';
      for (;;) {
        const { value, done } = await reader.read();
        if (done) break;
        buf += dec.decode(value, { stream: true });
        let i;
        while ((i = buf.indexOf('\n\n')) >= 0) {
          const block = buf.slice(0, i);
          buf = buf.slice(i + 2);
          const ev = /^event: (.*)$/m.exec(block)?.[1];
          const data = /^data: (.*)$/m.exec(block)?.[1];
          if (!ev || !data) continue;
          const d = JSON.parse(data);
          if (ev === 'delta') reply.content += d.text;
          else if (ev === 'sources') reply.sources = d.sources;
          else if (ev === 'notice') reply.notice = d.text;
          else if (ev === 'error') reply.error = d.error;
          paintLast();
        }
      }
    } catch (err) {
      if (err.name !== 'AbortError') reply.error = err.message;
    }
    if (!reply.content && !reply.error) reply.error = ctrl.signal.aborted ? 'Stopped.' : 'No answer.';
    state.busy = false;
    state.abort = null;
    sendBtn.replaceChildren(icon('send', 18));
    sendBtn.setAttribute('aria-label', 'Send');
    renderChat();
  }

  function save() {
    const messages = state.messages.filter((m) => !m.error || m.content).map((m) => ({ role: m.role, content: m.content }));
    if (!messages.length) return;
    api('PUT', `/api/ai/conversations/${state.id}`, { title: state.title, model: state.model, messages }).catch(() => {});
  }

  // ---------- rendering ----------
  let paintQueued = false;
  function paintLast() {
    if (paintQueued) return;
    paintQueued = true;
    requestAnimationFrame(() => { paintQueued = false; renderChat(true); });
  }

  function renderChat(onlyLast = false) {
    if (!state.messages.length) {
      body.replaceChildren(h('div', { class: 'uc-empty' }, h('span', { class: 'uc-big' }, icon('sparkles', 30)),
        h('h3', null, 'How can I help?'),
        h('div', { class: 'uc-suggest' }, [
          'Explain this error in plain words',
          'Summarise the note I have open',
          'Write a bash one-liner to find big files',
          "What's new in Go this year?",
        ].map((s) => h('button', { class: 'uc-sug', onclick: () => { input.value = s; input.focus(); grow(); } }, s)))));
      return;
    }
    const atBottom = body.scrollHeight - body.scrollTop - body.clientHeight < 60;
    if (onlyLast && body.lastElementChild?.classList.contains('uc-msg')) {
      body.lastElementChild.replaceWith(msgEl(state.messages[state.messages.length - 1], true));
    } else {
      body.replaceChildren(...state.messages.map((m, i) => msgEl(m, i === state.messages.length - 1)));
    }
    if (atBottom || !onlyLast) body.scrollTop = body.scrollHeight;
  }

  function msgEl(m, last) {
    if (m.role === 'user') {
      return h('div', { class: 'uc-msg user' }, h('div', { class: 'uc-bubble-text' }, m.shown ?? m.content.replace(/\n\n---\nContext from[\s\S]*$/, '')),
        m.attached || /\n\n---\nContext from/.test(m.content) ? h('div', { class: 'uc-attached' }, icon('link', 11), 'context included') : null);
    }
    const md = h('div', { class: 'uc-md md' });
    md.innerHTML = renderMarkdown(m.content || ''); // escaped by renderMarkdown
    md.querySelectorAll('pre').forEach((pre) => pre.append(h('button', {
      class: 'uc-copy', title: 'Copy', onclick: () => navigator.clipboard.writeText(pre.querySelector('code')?.textContent || '').then(() => toast('Copied'), report),
    }, icon('clipboard', 13))));
    const typing = state.busy && last && !m.error;
    return h('div', { class: 'uc-msg bot' },
      m.notice ? h('div', { class: 'uc-notice' }, m.notice) : null,
      md,
      typing ? h('span', { class: 'uc-caret' }) : null,
      m.error ? h('div', { class: 'uc-error' }, icon('info', 13), m.error, last && !state.busy ? h('button', { class: 'link-btn', onclick: () => retry() }, 'Retry') : null) : null,
      m.sources?.length ? h('div', { class: 'uc-sources' }, m.sources.map((s) => h('a', { href: s.url, target: '_blank', rel: 'noopener noreferrer', title: s.title },
        h('span', { class: 'uc-n' }, s.n), hostOf(s.url)))) : null,
      !typing && m.content ? h('div', { class: 'uc-actions' },
        h('button', { class: 'icon-btn', title: 'Copy answer', onclick: () => navigator.clipboard.writeText(m.content).then(() => toast('Copied'), report) }, icon('clipboard', 13))) : null);
  }

  async function retry() {
    const lastUser = [...state.messages].reverse().find((m) => m.role === 'user');
    state.messages = state.messages.slice(0, state.messages.lastIndexOf(lastUser));
    if (lastUser) { state.messages.push(lastUser); const reply = { role: 'assistant', content: '' }; state.messages.push(reply); renderChat(); await stream(reply, searchOn); save(); }
  }

  // ---------- history ----------
  async function showHistory() {
    let list = [];
    try { list = (await api('GET', '/api/ai/conversations')).conversations; } catch (err) { report(err); }
    body.replaceChildren(h('div', { class: 'uc-history' },
      h('div', { class: 'uc-hist-head' }, h('b', null, 'Conversations'), h('button', { class: 'link-btn', onclick: () => renderChat() }, 'Back')),
      list.length ? list.map((c) => h('div', { class: 'uc-hist-item' },
        h('button', { class: 'uc-hist-open', onclick: () => load(c.id) }, h('span', { class: 'uc-hist-title' }, c.title || 'Untitled'),
          h('span', { class: 'uc-hist-meta' }, `${(c.model || '').split(':')[1] || ''} · ${fmtAgo(c.updated)}`)),
        h('button', {
          class: 'icon-btn', title: 'Delete', 'aria-label': 'Delete',
          onclick: async () => { if (await confirmDialog('Delete this conversation?', c.title || 'Untitled', 'Delete')) { await api('DELETE', `/api/ai/conversations/${c.id}`).catch(report); showHistory(); } },
        }, icon('trash', 14)))) : h('div', { class: 'uc-empty small' }, 'No saved conversations yet.')));
  }

  async function load(id) {
    try {
      const c = await api('GET', `/api/ai/conversations/${id}`);
      state.abort?.abort();
      state = { id: c.id, title: c.title, model: c.model, messages: c.messages || [], busy: false, abort: null };
      if (c.model && [...modelSel.options].some((o) => o.value === c.model)) modelSel.value = c.model;
      renderChat();
    } catch (err) { report(err); }
  }
}

// Prefer a fast general model when nothing was chosen before.
function pickDefault(models) {
  const pref = [/gemini-[\d.]+-flash$/, /mistral-small-latest/, /mistral-medium-latest/, /llama-3\.3-70b-versatile/, /flash/, /latest/];
  for (const re of pref) { const m = models.find((x) => re.test(x.name)); if (m) return m.id; }
  return models[0]?.id || '';
}

function hostOf(u) { try { return new URL(u).hostname.replace(/^www\./, ''); } catch { return u; } }

// Settings changes (keys, on/off) refresh the bubble.
export function chatbotConfigChanged(c) { emit('chatbot-config', c); }
