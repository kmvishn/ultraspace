import { h, icon, api, toast, report, confirmDialog, dialog, settings, system, on, fmtBytes, fmtDuration, fmtAgo, fmtDate, baseOf, fileUrl, emit } from '../core.js';
import { deviceName } from '../session.js';
import { patternPad } from '../pattern.js';
import { chatbotConfigChanged } from '../chatbot.js';
import { registerApp, openApp } from '../wm.js';
import { WALLPAPERS, ACCENTS } from '../desktop.js';

registerApp({
  id: 'settings', name: 'Settings', icon: 'sliders', color: ['#94a3b8', '#475569'], width: 820, height: 600,
  mount(win, args) { return mountSettings(win, args); },
});

const SECTIONS = [
  { id: 'appearance', label: 'Appearance', icon: 'palette' },
  { id: 'account', label: 'Account', icon: 'user' },
  { id: 'devices', label: 'Devices', icon: 'laptop' },
  { id: 'chatbot', label: 'Ultra Chat Bot', icon: 'sparkles' },
  { id: 'about', label: 'About', icon: 'info' },
];

function mountSettings(win, args) {
  const nav = h('div', { class: 'side-list' });
  const main = h('div', { class: 'main scroll settings-main' });
  win.body.append(h('div', { class: 'app app-settings' }, h('aside', { class: 'side' }, h('div', { class: 'side-title' }, 'Settings'), nav), main));
  let current = null;
  let off = null;

  const show = (id) => {
    current = id;
    if (off) { off(); off = null; }
    nav.replaceChildren(...SECTIONS.map((s) => h('button', { class: `side-item ${s.id === id ? 'active' : ''}`, onclick: () => show(s.id) }, icon(s.icon, 16), h('span', null, s.label))));
    win.setTitle(`Settings — ${SECTIONS.find((s) => s.id === id).label}`);
    off = { appearance, account, devices, chatbot, about }[id](main) || null;
  };
  show(args.restore?.section || args.section || 'appearance');
  return {
    onArgs(a) { if (a.section && a.section !== current) show(a.section); },
    onDestroy() { if (off) off(); },
    saveState: () => ({ section: current }),
  };

  function group(title, desc, ...children) {
    return h('section', { class: 'set-group' }, h('h3', null, title), desc ? h('p', { class: 'set-desc' }, desc) : null, children);
  }

  function appearance(el) {
    const render = () => {
      const s = settings.data;
      const themeSeg = h('div', { class: 'seg big' }, [['light', 'Light', 'sun'], ['dark', 'Dark', 'moon'], ['auto', 'Auto', 'monitor']].map(([id, label, ic]) =>
        h('button', { class: `seg-btn text ${s.theme === id ? 'active' : ''}`, onclick: () => settings.set({ theme: id }) }, icon(ic, 15), label)));
      const accents = h('div', { class: 'swatches' }, ACCENTS.map((c) => h('button', {
        class: `swatch ${s.accent === c ? 'active' : ''}`, 'aria-label': `Accent ${c}`, style: { background: c },
        onclick: () => settings.set({ accent: c }),
      }, s.accent === c ? icon('check', 14) : null)));
      const custom = String(s.wallpaper).startsWith('file:') ? s.wallpaper.slice(5) : null;
      const walls = h('div', { class: 'wall-grid' },
        WALLPAPERS.map((w) => h('button', { class: `wall ${s.wallpaper === w.id ? 'active' : ''}`, onclick: () => settings.set({ wallpaper: w.id }) },
          h('span', { class: `wall-prev wp-${w.id}` }), h('span', { class: 'wall-name' }, w.name))),
        custom ? h('button', { class: 'wall active' },
          h('span', { class: 'wall-prev wp-image', style: { backgroundImage: `url("${fileUrl(custom, true)}")` } }), h('span', { class: 'wall-name' }, baseOf(custom))) : null);
      const buttonsSeg = h('div', { class: 'seg big' }, [['right', 'Right'], ['left', 'Left']].map(([id, label]) =>
        h('button', { class: `seg-btn text ${(s.buttons || 'right') === id ? 'active' : ''}`, onclick: () => settings.set({ buttons: id }) }, label)));
      const glass = h('label', { class: 'switch-row' },
        h('span', null, h('b', null, 'Transparency'), h('span', { class: 'set-desc' }, 'Frosted-glass menu bar, dock and widgets. Turn off on slow devices.')),
        h('input', { type: 'checkbox', class: 'switch', checked: !!s.transparency, onchange: (e) => settings.set({ transparency: e.target.checked }) }));
      el.replaceChildren(h('div', { class: 'settings-page' },
        group('Theme', null, themeSeg),
        group('Accent colour', null, accents),
        group('Window buttons', 'Minimise, maximise and close: on the right like Windows, or on the left like macOS.', buttonsSeg),
        group('Wallpaper', 'Or right-click any image in Files and choose “Set as wallpaper”.', walls,
          h('button', { class: 'btn small', onclick: () => openApp('files') }, icon('folder', 14), 'Choose from Files…')),
        group('Effects', null, glass)));
    };
    render();
    return on('settings', render);
  }

  function account(el) {
    const cur = h('input', { class: 'input', type: 'password', autocomplete: 'current-password' });
    const next = h('input', { class: 'input', type: 'password', autocomplete: 'new-password' });
    const again = h('input', { class: 'input', type: 'password', autocomplete: 'new-password' });
    const error = h('div', { class: 'form-error' });
    const submit = h('button', { class: 'btn primary', type: 'submit' }, 'Change password');
    const form = h('form', {
      class: 'set-form',
      onsubmit: async (e) => {
        e.preventDefault();
        error.textContent = '';
        if (next.value !== again.value) { error.textContent = 'The new passwords do not match.'; return; }
        submit.disabled = true;
        try {
          await api('POST', '/api/account/password', { current: cur.value, new: next.value });
          cur.value = next.value = again.value = '';
          toast('Password changed. Your other devices were signed out.');
        } catch (err) { error.textContent = err.message; }
        submit.disabled = false;
      },
    },
    h('label', { class: 'field-label' }, 'Current password'), cur,
    h('label', { class: 'field-label' }, 'New password (at least 10 characters)'), next,
    h('label', { class: 'field-label' }, 'Confirm new password'), again,
    error, submit);
    el.replaceChildren(h('div', { class: 'settings-page' },
      group('Signed in as', null, h('div', { class: 'account-card' },
        h('span', { class: 'avatar' }, (window.ultraspaceUser?.username || '?').slice(0, 1).toUpperCase()),
        h('div', null, h('div', { class: 'account-name' }, window.ultraspaceUser?.username || ''), h('div', { class: 'set-desc' }, 'Administrator')),
        h('div', { class: 'tb-spacer' }),
        h('button', { class: 'btn', onclick: () => emit('signout') }, icon('logOut', 15), 'Sign out'))),
      group('Password', 'Changing your password signs out every other device.', form),
      patternGroup()));
    return null;
  }

  // ---------- pattern lock ----------

  function patternGroup() {
    const on = !!window.ultraspaceUser?.pattern;
    const box = h('div', { class: 'switch-row pattern-row' },
      h('span', null, h('b', null, on ? 'Pattern lock is on' : 'Pattern lock is off'),
        h('span', { class: 'set-desc' }, on
          ? 'After your password, sign-in asks for your pattern. Forgot it? On the server run: sudo -u ultraspace ultraspace reset-pattern -user ' + (window.ultraspaceUser?.username || 'NAME')
          : 'Ask for a 3×3 pattern after the password when signing in. A stolen password alone is then not enough.')),
      h('div', { class: 'hero-actions' },
        h('button', { class: `btn ${on ? '' : 'primary'}`, onclick: () => setupPattern() }, on ? 'Change' : 'Set up'),
        on ? h('button', { class: 'btn', onclick: () => removePattern() }, 'Turn off') : null));
    return group('Pattern lock', null, box);
  }

  function setupPattern() {
    let first = null;
    const msg = h('p', { class: 'dialog-text pattern-msg' }, 'Draw a pattern joining at least 4 dots.');
    const pwd = h('input', { class: 'input hidden', type: 'password', placeholder: 'Your current password', autocomplete: 'current-password' });
    const save = h('button', { class: 'btn primary', disabled: true }, 'Save');
    const pad = patternPad({
      onDone: (seq) => {
        if (seq.length < 4) { msg.textContent = 'Join at least 4 dots. Try again.'; pad.error(); return; }
        if (!first) { first = seq.join(','); msg.textContent = 'Draw it again to confirm.'; setTimeout(() => pad.clear(), 350); return; }
        if (seq.join(',') !== first) { first = null; msg.textContent = 'The patterns did not match. Start again.'; pad.error(); return; }
        pad.ok();
        msg.textContent = 'Pattern confirmed. Enter your password to save it.';
        pwd.classList.remove('hidden');
        save.disabled = false;
        pwd.focus();
      },
    });
    save.addEventListener('click', async () => {
      save.disabled = true;
      try {
        await api('POST', '/api/account/pattern', { password: pwd.value, pattern: first });
        window.ultraspaceUser.pattern = true;
        close();
        toast('Pattern lock is on');
        show('account');
      } catch (err) { msg.textContent = err.message; save.disabled = false; }
    });
    pwd.addEventListener('keydown', (e) => { if (e.key === 'Enter' && !save.disabled) save.click(); });
    const close = dialog({
      title: window.ultraspaceUser?.pattern ? 'Change pattern' : 'Set up pattern lock', width: '360px',
      body: h('div', { class: 'pattern-setup' }, msg, pad.el, pwd),
      actions: [h('button', { class: 'btn', onclick: () => close() }, 'Cancel'), save],
    });
  }

  function removePattern() {
    const pwd = h('input', { class: 'input', type: 'password', placeholder: 'Your current password', autocomplete: 'current-password' });
    const msg = h('div', { class: 'form-error' });
    const ok = h('button', {
      class: 'btn danger-solid',
      onclick: async () => {
        try {
          await api('POST', '/api/account/pattern', { password: pwd.value, pattern: '' });
          window.ultraspaceUser.pattern = false;
          close();
          toast('Pattern lock is off');
          show('account');
        } catch (err) { msg.textContent = err.message; }
      },
    }, 'Turn off');
    pwd.addEventListener('keydown', (e) => { if (e.key === 'Enter') ok.click(); });
    const close = dialog({
      title: 'Turn off pattern lock?', width: '360px',
      body: h('div', null, h('p', { class: 'dialog-text' }, 'Sign-in will ask only for your password.'), pwd, msg),
      actions: [h('button', { class: 'btn', onclick: () => close() }, 'Cancel'), ok],
    });
    requestAnimationFrame(() => pwd.focus());
  }

  function devices(el) {
    const list = h('div', { class: 'device-list' }, h('div', { class: 'empty-state small' }, 'Loading…'));
    const othersBtn = h('button', { class: 'btn', onclick: () => signOutOthers() }, icon('logOut', 15), 'Sign out all other devices');
    let timer = null;

    async function load() {
      try {
        const { sessions } = await api('GET', '/api/account/sessions');
        othersBtn.disabled = sessions.length < 2;
        list.replaceChildren(...sessions.map((s) => {
          const mobile = /Mobile|Android|iPhone|iPad/.test(s.user_agent);
          return h('div', { class: `device ${s.current ? 'current' : ''}` },
            h('span', { class: 'device-icon' }, icon(mobile ? 'smartphone' : 'laptop', 20)),
            h('div', { class: 'device-text' },
              h('div', { class: 'device-name' }, s.user_agent ? deviceName(s.user_agent) : 'Unknown device', s.current ? h('span', { class: 'pill active' }, 'This device') : null),
              h('div', { class: 'set-desc' }, [s.ip, s.current ? 'active now' : s.last_seen ? `active ${fmtAgo(s.last_seen)}` : '', `signed in ${fmtDate(s.created)}`].filter(Boolean).join(' · '))),
            s.current ? null : h('button', { class: 'btn small', onclick: () => revoke(s) }, 'Sign out'));
        }));
      } catch (err) { report(err); }
    }

    async function revoke(s) {
      try {
        await api('POST', '/api/account/sessions/revoke', { id: s.id });
        toast(`Signed out ${deviceName(s.user_agent)}`);
      } catch (err) { report(err); }
      load();
    }

    async function signOutOthers() {
      if (!(await confirmDialog('Sign out other devices?', 'Every other browser signed in to ultraspace will need your password again.', 'Sign out'))) return;
      try {
        await api('POST', '/api/account/sessions/revoke', { others: true });
        toast('Signed out all other devices');
      } catch (err) { report(err); }
      load();
    }

    el.replaceChildren(h('div', { class: 'settings-page' },
      group('Your session follows you',
        'Open windows, the song and position in Music, and unsaved text in the editor are saved to this server every few seconds. The Browser, Terminal sessions and VS Code run on the server itself, so their tabs, logins, running programs and open files are simply there on every device. A device stays signed in until it has been unused for 30 days.'),
      group('Signed-in devices', 'Sign out any device you no longer use, for example a shared computer.', list, othersBtn)));
    load();
    timer = setInterval(load, 30_000);
    return () => clearInterval(timer);
  }

  // ---------- Ultra Chat Bot ----------

  function chatbot(el) {
    const KEYS = {
      gemini: { label: 'Gemini API key', where: 'aistudio.google.com/apikey' },
      mistral: { label: 'Mistral API key', where: 'console.mistral.ai → API keys' },
      brave: { label: 'Brave Search API key (optional, for web search)', where: 'api-dashboard.search.brave.com' },
    };
    const render = async () => {
      let c;
      try { c = await api('GET', '/api/ai/config'); } catch (err) { report(err); return; }
      chatbotConfigChanged(c);
      const all = [...c.providers, c.search];
      const toggle = h('label', { class: 'switch-row' },
        h('span', null, h('b', null, 'Show the chat bubble'), h('span', { class: 'set-desc' }, 'A floating Ultra Chat Bot button at the bottom-right. Ctrl+J opens it.')),
        h('input', { type: 'checkbox', class: 'switch', checked: c.enabled, onchange: async (e) => { await api('PUT', '/api/ai/config', { enabled: e.target.checked }).catch(report); render(); } }));
      const rows = all.map((p) => {
        const meta = KEYS[p.id] || { label: `${p.name} key`, where: '' };
        const input = h('input', { class: 'input', type: 'password', placeholder: p.configured ? `Saved (${p.hint}) — paste a new key to replace` : 'Paste the key', autocomplete: 'off', spellcheck: 'false' });
        const save = async () => {
          if (!input.value.trim()) return;
          try { await api('PUT', '/api/ai/config', { keys: { [p.id]: input.value.trim() } }); toast(`${p.name} key saved`); render(); } catch (err) { report(err); }
        };
        input.addEventListener('keydown', (e) => { if (e.key === 'Enter') save(); });
        return h('div', { class: 'ai-key' },
          h('label', { class: 'field-label' }, meta.label, p.configured ? h('span', { class: 'pill active' }, `saved ${p.hint}`) : null),
          h('div', { class: 'ai-key-row' }, input,
            h('button', { class: 'btn primary small', onclick: save }, 'Save'),
            p.configured ? h('button', {
              class: 'btn small', onclick: async () => {
                if (!(await confirmDialog(`Remove the ${p.name} key?`, 'It is deleted from the server.', 'Remove'))) return;
                await api('PUT', '/api/ai/config', { keys: { [p.id]: '' } }).catch(report);
                render();
              },
            }, 'Remove') : null),
          meta.where ? h('div', { class: 'set-desc' }, `Get one at ${meta.where}`) : null);
      });
      el.replaceChildren(h('div', { class: 'settings-page' },
        group('Ultra Chat Bot', 'Chat with Gemini or Mistral models from anywhere on the desktop. Keys are stored on your server and never sent to the browser; the server talks to the provider for you.', toggle),
        group('API keys', null, h('div', { class: 'ai-keys' }, rows)),
        group('Privacy', 'What you type, and any window you choose to include, is sent to the provider of the model you pick (Google or Mistral). With web search on, your question also goes to Brave.')));
    };
    render();
    return null;
  }

  function about(el) {
    const kv = h('dl', { class: 'kv' });
    const render = (s) => {
      if (!s) return;
      kv.replaceChildren(...[
        ['Version', `ultraspace ${s.app.version}`],
        ['Memory used by ultraspace', fmtBytes(s.app.rss)],
        ['Server', s.hostname],
        ['Operating system', s.os],
        ['Kernel', s.kernel],
        ['Uptime', fmtDuration(s.uptime)],
        ['CPU cores', s.cpu.cores],
        ['Memory', fmtBytes(s.memory.total)],
      ].flatMap(([k, v]) => [h('dt', null, k), h('dd', null, v)]));
    };
    render(system.latest);
    el.replaceChildren(h('div', { class: 'settings-page' },
      h('div', { class: 'about-hero' }, h('span', { class: 'logo-mark big' }),
        h('div', null, h('div', { class: 'about-name' }, 'ultraspace'), h('div', { class: 'set-desc' }, 'Your personal server, in one small Go binary.'))),
      group('This server', null, kv)));
    return on('system', render);
  }
}
