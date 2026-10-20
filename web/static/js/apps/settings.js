import { h, icon, api, toast, report, confirmDialog, settings, system, on, fmtBytes, fmtDuration, fmtAgo, fmtDate, baseOf, fileUrl, emit } from '../core.js';
import { deviceName } from '../session.js';
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
    off = { appearance, account, devices, about }[id](main) || null;
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
      group('Password', 'Changing your password signs out every other device.', form)));
    return null;
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
