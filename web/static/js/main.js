import { h, icon, api, on, settings, ApiError } from './core.js';
import { buildDesktop, applyAppearance } from './desktop.js';
import { startSession, flushSession, stopSession } from './session.js';
import { stopEvents } from './clipboard.js';
import { patternPad } from './pattern.js';
import './apps/files.js';
import './apps/system.js';
import './apps/music.js';
import './apps/settings.js';
import './apps/editor.js';
import './apps/viewer.js';
import './apps/browser.js';
import './apps/terminal.js';
import './apps/code.js';
import './apps/notes.js';
import './apps/photos.js';
import './apps/video.js';

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
  stopEvents();
  await api('POST', '/api/logout').catch(() => {});
  user = null;
  window.ultraspaceUser = null;
  boot();
}

on('signout', signOut);
on('unauthorized', () => { if (user) { user = null; stopSession(); stopEvents(); boot(); } });

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
        else {
          const res = await api('POST', '/api/login', { username: v.username, password: v.password });
          if (res.pattern_required) { patternStep(res.ticket, v.username); return; }
        }
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

  const card = h('div', { class: 'lock-card glass' }, form);

  // Second step when the pattern lock is on: the password was right, now the pattern.
  function patternStep(ticket, username) {
    const msg = h('div', { class: 'lock-sub' }, 'Draw your pattern');
    const pad = patternPad({
      onDone: async (seq) => {
        if (seq.length < 4) { msg.textContent = 'Join at least 4 dots'; pad.error(); return; }
        try {
          await api('POST', '/api/login/pattern', { ticket, pattern: seq.join(',') });
          pad.ok();
          bg.classList.add('unlocking');
          setTimeout(boot, 280);
        } catch (err) {
          pad.error();
          if (/password again/.test(err.message)) {
            setTimeout(() => { card.replaceChildren(form); submit.disabled = false; error.textContent = err.message; inputs.password.value = ''; inputs.password.focus(); }, 900);
            msg.textContent = err.message;
          } else msg.textContent = 'Wrong pattern. Try again.';
        }
      },
    });
    card.replaceChildren(h('div', { class: 'lock-form lock-pattern' },
      h('div', { class: 'lock-avatar' }, h('span', { class: 'avatar big' }, username.slice(0, 1).toUpperCase())),
      h('div', { class: 'lock-title' }, username), msg, pad.el,
      h('button', { class: 'link-btn lock-back', type: 'button', onclick: () => { card.replaceChildren(form); submit.disabled = false; inputs.password.value = ''; inputs.password.focus(); } }, 'Use the password again')));
  }

  bg.append(h('div', { class: 'lock-clock' }, time, date), card);
  app.replaceChildren(bg);
  Object.values(inputs)[0].focus();
}

boot();
