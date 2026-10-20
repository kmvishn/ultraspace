// A compact picker for files in the Files library, e.g. to answer a web page's
// "choose file" with something already on the server.
import { h, icon, api, report, dialog, fmtBytes, kindOf, KIND_ICON, joinPath, dirOf } from './core.js';

export function pickFiles({ title = 'Choose from Files', multiple = false, start = '/' } = {}) {
  return new Promise((resolve) => {
    let dir = start;
    const chosen = new Set();
    const crumbs = h('div', { class: 'pick-crumbs' });
    const list = h('div', { class: 'pick-list' });
    const ok = h('button', { class: 'btn primary', disabled: true, onclick: () => done([...chosen]) }, 'Choose');
    let finished = false;
    const done = (v) => { if (!finished) { finished = true; close(); resolve(v); } };

    async function load() {
      list.replaceChildren(h('div', { class: 'empty-state small' }, 'Loading…'));
      crumbs.replaceChildren(h('button', { class: 'crumb', onclick: () => go('/') }, icon('home', 14)),
        ...dir.split('/').filter(Boolean).map((name, i, all) => [h('span', { class: 'crumb-sep' }, '›'),
          h('button', { class: 'crumb', onclick: () => go(`/${all.slice(0, i + 1).join('/')}`) }, name)]));
      try {
        const res = await api('GET', `/api/files/list?path=${encodeURIComponent(dir)}`);
        const entries = res.entries.filter((e) => !e.name.startsWith('.'))
          .sort((a, b) => (b.dir - a.dir) || a.name.localeCompare(b.name));
        list.replaceChildren(...(entries.length ? entries.map((e) => {
          const p = joinPath(dir, e.name);
          const row = h('button', {
            class: `pick-row ${chosen.has(p) ? 'selected' : ''}`,
            onclick: () => {
              if (e.dir) { go(p); return; }
              if (!multiple) chosen.clear();
              chosen.has(p) ? chosen.delete(p) : chosen.add(p);
              list.querySelectorAll('.pick-row').forEach((r) => r.classList.toggle('selected', chosen.has(r.dataset.path)));
              ok.disabled = !chosen.size;
              ok.textContent = chosen.size > 1 ? `Choose ${chosen.size}` : 'Choose';
            },
            ondblclick: () => { if (!e.dir) done([p]); },
            dataset: { path: p },
          }, h('span', { class: `pick-icon k-${kindOf(e)}` }, icon(KIND_ICON[kindOf(e)], 16)), h('span', { class: 'pick-name' }, e.name),
          h('span', { class: 'pick-size' }, e.dir ? '' : fmtBytes(e.size)));
          return row;
        }) : [h('div', { class: 'empty-state small' }, 'This folder is empty')]));
      } catch (err) {
        report(err);
        if (dir !== '/') go(dirOf(dir));
      }
    }
    const go = (p) => { dir = p || '/'; load(); };

    const close = dialog({
      title, width: '520px',
      body: h('div', { class: 'pick' }, crumbs, list),
      actions: [h('button', { class: 'btn', onclick: () => done(null) }, 'Cancel'), ok],
    });
    // dialog() closes on Escape or a click outside; treat that as cancel.
    const watch = setInterval(() => { if (!list.isConnected) { clearInterval(watch); done(null); } }, 300);
    load();
  });
}
