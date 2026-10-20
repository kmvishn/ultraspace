import { h, icon, api, on, settings, ApiError } from './core.js';
import { buildDesktop, applyAppearance } from './desktop.js';
import { startSession, flushSession, stopSession } from './session.js';
import './apps/files.js';
import './apps/system.js';
import './apps/music.js';
import './apps/settings.js';
import './apps/editor.js';
import './apps/viewer.js';
import './apps/browser.js';
import './apps/terminal.js';
import './apps/code.js';

const app = document.getElementById('app');
let user = null;

async function boot() {
  try {
    user = await api('GET', '/api/me');
  } catch (err) {
    user = null;
    if (!(err instanceof ApiError) || err.status !== 401) {
      app.replaceChildren(h('div', { class: 'boot' }, `Cannot reach the server: ${err.message}`));
      return;
    }
    const setup = await api('GET', '/api/setup').catch(() => ({ required: false }));
    setup.required ? lockScreen('setup') : lockScreen('login');
    return;
  }
  window.ultraspaceUser = user;
  await settings.load();
  app.replaceChildren(buildDesktop(user, { onSignOut: signOut }));
  startSession();
}

async function signOut() {
  await flushSession();
  stopSession();
  await api('POST', '/api/logout').catch(() => {});
  user = null;
  window.ultraspaceUser = null;
  boot();
}

on('signout', signOut);
on('unauthorized', () => { if (user) { user = null; stopSession(); boot(); } });

function lockScreen(mode) {
  const bg = h('div', { class: 'lock' });
  applyAppearance(bg, settings.data);
  bg.className = `${bg.className} lock`;

  const time = h('div', { class: 'lock-time' });
  const date = h('div', { class: 'lock-date' });
  const tFmt = new Intl.DateTimeFormat(undefined, { hour: '2-digit', minute: '2-digit' });
  const dFmt = new Intl.DateTimeFormat(undefined, { weekday: 'long', day: 'numeric', month: 'long' });
  const tick = () => { const n = new Date(); time.textContent = tFmt.format(n); date.textContent = dFmt.format(n); };
  tick();
  const timer = setInterval(() => { if (!bg.isConnected) clearInterval(timer); else tick(); }, 10_000);

  const error = h('div', { class: 'form-error' });
  const fields = mode === 'setup'
    ? [
      { name: 'token', label: 'Setup token', type: 'text', hint: 'On the server run: sudo cat /var/lib/ultraspace/setup-token' },
      { name: 'username', label: 'Username', type: 'text', auto: 'username' },
      { name: 'password', label: 'Password (10+ characters)', type: 'password', auto: 'new-password' },
      { name: 'confirm', label: 'Confirm password', type: 'password', auto: 'new-password' },
    ]
    : [
      { name: 'username', label: 'Username', type: 'text', auto: 'username' },
      { name: 'password', label: 'Password', type: 'password', auto: 'current-password' },
    ];
  const inputs = Object.fromEntries(fields.map((f) => [f.name, h('input', {
    class: 'input lock-input', type: f.type, name: f.name, placeholder: f.label, 'aria-label': f.label,
    autocomplete: f.auto || 'off', required: true, spellcheck: 'false',
  })]));
  const submit = h('button', { class: 'lock-submit', type: 'submit', 'aria-label': mode === 'setup' ? 'Create account' : 'Sign in' }, icon('chevronRight', 20));

  const form = h('form', {
    class: 'lock-form',
    onsubmit: async (e) => {
      e.preventDefault();
      error.textContent = '';
      const v = Object.fromEntries(Object.entries(inputs).map(([k, el]) => [k, el.value]));
      if (mode === 'setup' && v.password !== v.confirm) { error.textContent = 'Passwords do not match'; return; }
      submit.disabled = true;
      try {
        if (mode === 'setup') await api('POST', '/api/setup', { token: v.token.trim(), username: v.username, password: v.password });
        else await api('POST', '/api/login', { username: v.username, password: v.password });
        bg.classList.add('unlocking');
        setTimeout(boot, 280);
      } catch (err) {
        error.textContent = err.message;
        form.classList.remove('shake');
        void form.offsetWidth;
        form.classList.add('shake');
        submit.disabled = false;
      }
    },
  },
  h('div', { class: 'lock-avatar' }, h('span', { class: 'logo-mark big' })),
  h('div', { class: 'lock-title' }, mode === 'setup' ? 'Welcome to ultraspace' : 'ultraspace'),
  mode === 'setup' ? h('div', { class: 'lock-sub' }, 'Create the administrator account to get started.') : null,
  fields.map((f, i) => [
    i === fields.length - 1 ? h('div', { class: 'lock-row' }, inputs[f.name], submit) : inputs[f.name],
    f.hint ? h('div', { class: 'lock-hint' }, icon('terminal', 13), f.hint) : null,
  ]),
  error);

  bg.append(h('div', { class: 'lock-clock' }, time, date), h('div', { class: 'lock-card glass' }, form));
  app.replaceChildren(bg);
  Object.values(inputs)[0].focus();
}

boot();
