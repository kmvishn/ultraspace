// VS Code: code-server running on the server as your Linux user, shown in a window.
// Open folders, editors, unsaved changes and extensions live on the server, so they
// are the same on every device.
import { h, icon, api } from '../core.js';
import { registerApp } from '../wm.js';

registerApp({
  id: 'code', name: 'VS Code', icon: 'code', color: ['#38bdf8', '#1d4ed8'], width: 1200, height: 760, minWidth: 520,
  mount(win, args) { return mountCode(win, args); },
});

function mountCode(win) {
  const overlay = h('div', { class: 'rb-overlay spin' }, h('div', { class: 'rb-overlay-box' },
    h('span', { class: 'bw-spin big' }), h('div', null, 'Starting VS Code on your server…'), h('div', { class: 'small muted' }, 'The first start takes a few seconds.')));
  const stage = h('div', { class: 'code-stage' }, overlay);
  win.body.append(h('div', { class: 'app app-code' }, stage));
  win.actionsEl.append(
    h('button', { class: 'icon-btn', title: 'Reload VS Code', 'aria-label': 'Reload VS Code', onclick: () => load() }, icon('refresh', 15)),
    h('button', { class: 'icon-btn', title: 'Open in a new browser tab', 'aria-label': 'Open in a new browser tab', onclick: () => window.open('code/', '_blank', 'noopener') }, icon('external', 15)));

  let frame = null;
  function fail(title, sub) {
    overlay.className = 'rb-overlay error';
    overlay.replaceChildren(h('div', { class: 'rb-overlay-box' }, icon('info', 26), h('div', null, title), sub ? h('div', { class: 'small muted' }, sub) : null,
      h('div', { class: 'rb-overlay-actions' }, h('button', { class: 'btn', onclick: () => load() }, 'Try again'))));
  }

  async function load() {
    overlay.className = 'rb-overlay spin';
    overlay.replaceChildren(h('div', { class: 'rb-overlay-box' }, h('span', { class: 'bw-spin big' }), h('div', null, 'Starting VS Code on your server…'),
      h('div', { class: 'small muted' }, 'The first start takes a few seconds.')));
    frame?.remove();
    let st;
    try { st = await api('GET', '/api/agent/status'); } catch (err) { fail('Could not reach the server.', err.message); return; }
    if (!st.available) { fail('The VS Code service is not running on this server.', 'It is installed with ultraspace as the "ultraspace-agent" service.'); return; }
    if (!st.code_available) { fail('VS Code (code-server) is not installed on this server.'); return; }
    frame = h('iframe', { class: 'code-frame', src: 'code/', title: 'VS Code', allow: 'clipboard-read; clipboard-write; fullscreen' });
    frame.addEventListener('load', () => { overlay.className = 'rb-overlay hidden'; });
    stage.prepend(frame);
  }
  load();

  // A desktop left open in a background tab keeps VS Code's connection, so it would
  // never stop on the server. After 10 hidden minutes, let go; reconnect on return.
  let hiddenTimer = null;
  let released = false;
  const onVisibility = () => {
    if (document.hidden) {
      hiddenTimer = setTimeout(() => { released = true; frame?.remove(); frame = null; }, 10 * 60_000);
    } else {
      clearTimeout(hiddenTimer);
      if (released) { released = false; load(); }
    }
  };
  document.addEventListener('visibilitychange', onVisibility);

  return {
    onDestroy() { clearTimeout(hiddenTimer); document.removeEventListener('visibilitychange', onVisibility); },
    onFocus() { frame?.focus(); },
    saveState: () => ({}),
  };
}
