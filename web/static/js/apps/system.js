import { h, icon, api, report, on, system, fmtBytes, fmtDuration, fmtDate, drawChart, accent, debounce } from '../core.js';
import { registerApp } from '../wm.js';

registerApp({
  id: 'system', name: 'System', icon: 'activity', color: ['#34d399', '#059669'], width: 1000, height: 660,
  mount(win, args) { return mountSystem(win, args); },
});

const TABS = [
  { id: 'overview', label: 'Overview', icon: 'activity' },
  { id: 'processes', label: 'Processes', icon: 'cpu' },
  { id: 'services', label: 'Services', icon: 'server' },
  { id: 'logs', label: 'Logs', icon: 'scroll' },
];

function mountSystem(win, args) {
  const main = h('div', { class: 'main' });
  const nav = h('div', { class: 'side-list' });
  win.body.append(h('div', { class: 'app app-system' },
    h('aside', { class: 'side' }, h('div', { class: 'side-title' }, 'System'), nav), main));

  let current = null;
  let stop = null;
  const show = (id) => {
    current = id;
    if (stop) { stop(); stop = null; }
    nav.replaceChildren(...TABS.map((t) => h('button', {
      class: `side-item ${t.id === id ? 'active' : ''}`, onclick: () => show(t.id),
    }, icon(t.icon, 16), h('span', null, t.label))));
    main.replaceChildren();
    win.setTitle(`System — ${TABS.find((t) => t.id === id).label}`);
    stop = { overview, processes, services, logs }[id](main) || null;
  };
  show(args.restore?.tab || args.tab || 'overview');
  return {
    onArgs(a) { if (a.tab && a.tab !== current) show(a.tab); },
    saveState: () => ({ tab: current }),
    onDestroy() { if (stop) stop(); },
  };

  // ---------- overview ----------

  function overview(el) {
    const card = (title, iconName, big, chart, sub) => h('section', { class: 'card' },
      h('div', { class: 'card-head' }, icon(iconName, 15), h('span', null, title), h('span', { class: 'card-big' }, big)),
      chart, sub);
    const cpuBig = h('span'), memBig = h('span'), rxBig = h('span'), swapBig = h('span');
    const cpuC = h('canvas', { class: 'chart' }), memC = h('canvas', { class: 'chart' }), netC = h('canvas', { class: 'chart' });
    const cpuSub = h('div', { class: 'card-sub' }), memSub = h('div', { class: 'card-sub' }), netSub = h('div', { class: 'card-sub' });
    const disks = h('div', { class: 'disk-list' });
    const info = h('dl', { class: 'kv' });
    el.append(h('div', { class: 'pane' },
      h('div', { class: 'cards cards-3' },
        card('CPU', 'cpu', cpuBig, cpuC, cpuSub),
        card('Memory', 'memory', memBig, memC, memSub),
        card('Network', 'arrowDown', rxBig, netC, netSub)),
      h('div', { class: 'cards cards-2' },
        h('section', { class: 'card' }, h('div', { class: 'card-head' }, icon('hardDrive', 15), h('span', null, 'Storage'), h('span', { class: 'card-big' }, swapBig)), disks),
        h('section', { class: 'card' }, h('div', { class: 'card-head' }, icon('info', 15), h('span', null, 'About this server')), info))));

    const draw = () => {
      const pts = system.history;
      drawChart(cpuC, [{ values: pts.map((p) => p.cpu), color: accent(), fill: true }], { max: 100 });
      drawChart(memC, [{ values: pts.map((p) => p.mem), color: '#f59e0b', fill: true }], { max: 100 });
      drawChart(netC, [
        { values: pts.map((p) => p.rx), color: '#38bdf8', fill: true },
        { values: pts.map((p) => p.tx), color: '#f472b6', fill: false },
      ]);
    };
    const update = (s) => {
      if (!s) return;
      const memPct = (100 * s.memory.used) / s.memory.total;
      cpuBig.textContent = `${s.cpu.percent.toFixed(0)}%`;
      cpuSub.textContent = `${s.cpu.cores} cores · load ${s.load.map((n) => n.toFixed(2)).join(' ')}`;
      memBig.textContent = `${memPct.toFixed(0)}%`;
      memSub.textContent = `${fmtBytes(s.memory.used)} of ${fmtBytes(s.memory.total)} used`;
      rxBig.textContent = `${fmtBytes(s.network.rx_rate)}/s`;
      netSub.replaceChildren(
        h('span', { class: 'legend rx' }, `↓ ${fmtBytes(s.network.rx_rate)}/s`),
        h('span', { class: 'legend tx' }, `↑ ${fmtBytes(s.network.tx_rate)}/s`),
        h('span', { class: 'muted' }, `· ${fmtBytes(s.network.rx_total)} in, ${fmtBytes(s.network.tx_total)} out`));
      swapBig.textContent = '';
      const rows = (s.disks || []).map((d) => ({ label: d.label === 'System' ? 'System disk (/)' : `${d.label} folder`, used: d.used, total: d.total }));
      if (s.swap.total) rows.push({ label: 'Swap', used: s.swap.used, total: s.swap.total });
      disks.replaceChildren(...rows.map((d) => {
        const pct = (100 * d.used) / d.total;
        return h('div', { class: 'disk-row' },
          h('div', { class: 'disk-top' }, h('span', null, d.label), h('span', { class: 'muted' }, `${fmtBytes(d.used)} / ${fmtBytes(d.total)}`)),
          h('div', { class: `meter ${pct >= 90 ? 'crit' : pct >= 75 ? 'warn' : ''}` }, h('div', { style: { width: `${pct.toFixed(1)}%` } })));
      }));
      const kv = [
        ['Hostname', s.hostname], ['Operating system', s.os], ['Kernel', s.kernel], ['Uptime', fmtDuration(s.uptime)],
        ['ultraspace', `v${s.app.version} · ${fmtBytes(s.app.rss)} RAM`], ['Running since', fmtDate(s.app.started)],
      ];
      info.replaceChildren(...kv.flatMap(([k, v]) => [h('dt', null, k), h('dd', null, v)]));
      draw();
    };
    update(system.latest);
    const off = on('system', update);
    const ro = new ResizeObserver(draw);
    ro.observe(el);
    return () => { off(); ro.disconnect(); };
  }

  // ---------- processes ----------

  function processes(el) {
    let procs = [];
    let sort = { key: 'cpu', dir: -1 };
    let filter = '';
    const filterInput = h('input', { class: 'input search-input', type: 'search', placeholder: 'Filter processes', oninput: debounce(() => { filter = filterInput.value.toLowerCase(); render(); }, 120) });
    const count = h('span', { class: 'muted' });
    const tbody = h('tbody');
    const head = h('tr');
    el.append(h('div', { class: 'pane pane-flush' },
      h('div', { class: 'toolbar' }, filterInput, h('div', { class: 'tb-spacer' }), count),
      h('div', { class: 'scroll' }, h('table', { class: 'table proc-table' }, h('thead', null, head), tbody))));

    const cols = [
      { key: 'name', label: 'Process' }, { key: 'user', label: 'User' }, { key: 'pid', label: 'PID', num: true },
      { key: 'cpu', label: 'CPU', num: true }, { key: 'rss', label: 'Memory', num: true }, { key: 'threads', label: 'Threads', num: true },
    ];
    const renderHead = () => head.replaceChildren(...cols.map((c) => h('th', {
      class: `${c.num ? 'num' : ''} sortable ${sort.key === c.key ? 'sorted' : ''}`,
      onclick: () => { sort = { key: c.key, dir: sort.key === c.key ? -sort.dir : (c.num ? -1 : 1) }; renderHead(); render(); },
    }, c.label, sort.key === c.key ? (sort.dir < 0 ? ' ↓' : ' ↑') : '')));

    const render = () => {
      let list = procs;
      if (filter) list = list.filter((p) => p.name.toLowerCase().includes(filter) || p.command.toLowerCase().includes(filter) || String(p.pid) === filter || p.user.toLowerCase().includes(filter));
      list = [...list].sort((a, b) => {
        const x = a[sort.key], y = b[sort.key];
        return (typeof x === 'string' ? x.localeCompare(y) : x - y) * sort.dir;
      });
      count.textContent = `${list.length} of ${procs.length} processes`;
      const totalMem = system.latest?.memory.total || 0;
      tbody.replaceChildren(...list.slice(0, 300).map((p) => h('tr', { title: p.command || p.name },
        h('td', null, h('div', { class: 'proc-name' }, h('span', null, p.name), p.command ? h('span', { class: 'proc-cmd' }, p.command) : null)),
        h('td', { class: 'muted' }, p.user),
        h('td', { class: 'num muted' }, p.pid),
        h('td', { class: `num ${p.cpu >= 50 ? 'hot' : ''}` }, `${p.cpu.toFixed(1)}%`),
        h('td', { class: 'num' }, fmtBytes(p.rss), totalMem ? h('span', { class: 'muted small' }, ` ${((100 * p.rss) / totalMem).toFixed(1)}%`) : null),
        h('td', { class: 'num muted' }, p.threads))));
    };
    const load = async () => {
      if (document.hidden) return;
      try { procs = (await api('GET', '/api/system/processes')).processes; render(); } catch (err) { report(err); }
    };
    renderHead();
    load();
    const t = setInterval(load, 3000);
    return () => clearInterval(t);
  }

  // ---------- services ----------

  function services(el) {
    let units = [];
    let filter = '';
    let state = 'all';
    let selectedUnit = null;
    const filterInput = h('input', { class: 'input search-input', type: 'search', placeholder: 'Filter services', oninput: debounce(() => { filter = filterInput.value.toLowerCase(); render(); }, 120) });
    const stateSeg = h('div', { class: 'seg' });
    const list = h('div', { class: 'svc-list scroll' });
    const detail = h('div', { class: 'svc-detail' });
    el.append(h('div', { class: 'pane pane-flush' },
      h('div', { class: 'toolbar' }, filterInput, stateSeg, h('div', { class: 'tb-spacer' }),
        h('button', { class: 'icon-btn', 'aria-label': 'Refresh', onclick: () => load() }, icon('refresh', 16))),
      h('div', { class: 'split' }, list, detail)));

    const states = [['all', 'All'], ['active', 'Running'], ['failed', 'Failed'], ['inactive', 'Stopped']];
    const renderSeg = () => stateSeg.replaceChildren(...states.map(([id, label]) => h('button', {
      class: `seg-btn text ${state === id ? 'active' : ''}`, onclick: () => { state = id; renderSeg(); render(); },
    }, label, id === 'failed' && units.some((u) => u.active === 'failed') ? h('span', { class: 'badge' }, units.filter((u) => u.active === 'failed').length) : null)));

    const render = () => {
      const shown = units.filter((u) => (state === 'all' || u.active === state) &&
        (!filter || u.name.toLowerCase().includes(filter) || u.description.toLowerCase().includes(filter)));
      list.replaceChildren(...(shown.length ? shown.map((u) => h('button', {
        class: `svc ${selectedUnit === u.name ? 'active' : ''}`, onclick: () => { selectedUnit = u.name; render(); showLogs(u); },
      },
      h('span', { class: `dot ${u.active}` }),
      h('span', { class: 'svc-text' }, h('span', { class: 'svc-name' }, u.name.replace(/\.service$/, '')), h('span', { class: 'svc-desc' }, u.description)),
      h('span', { class: `pill ${u.active}` }, u.sub))) : [h('div', { class: 'empty-state small' }, 'No services match')]));
    };

    const showLogs = (u) => {
      const box = h('div', { class: 'logbox' }, h('div', { class: 'muted pad' }, 'Loading logs…'));
      detail.replaceChildren(
        h('div', { class: 'svc-head' },
          h('div', null, h('div', { class: 'svc-title' }, h('span', { class: `dot ${u.active}` }), u.name), h('div', { class: 'muted small' }, u.description)),
          h('div', { class: 'svc-state' }, h('span', { class: `pill ${u.active}` }, `${u.active} · ${u.sub}`))),
        box);
      api('GET', `/api/logs?unit=${encodeURIComponent(u.name)}&lines=300`)
        .then((res) => renderLog(box, res))
        .catch((err) => { box.replaceChildren(h('div', { class: 'muted pad' }, err.message)); });
    };

    const load = async () => {
      try {
        units = (await api('GET', '/api/services')).units;
        renderSeg();
        render();
      } catch (err) { report(err); }
    };
    detail.append(h('div', { class: 'empty-state small' }, icon('server', 32), 'Select a service to see its logs'));
    renderSeg();
    load();
    return null;
  }

  // ---------- logs ----------

  function logs(el) {
    let priority = '';
    let lines = 300;
    let follow = true;
    const box = h('div', { class: 'logbox big' });
    const prioSeg = h('div', { class: 'seg' });
    const followBtn = h('button', { class: 'btn small', onclick: () => { follow = !follow; renderControls(); } });
    el.append(h('div', { class: 'pane pane-flush' },
      h('div', { class: 'toolbar' }, prioSeg, h('div', { class: 'tb-spacer' }), followBtn,
        h('button', { class: 'icon-btn', 'aria-label': 'Refresh', onclick: () => load() }, icon('refresh', 16))),
      box));
    const prios = [['', 'All'], ['4', 'Warnings'], ['3', 'Errors']];
    const renderControls = () => {
      prioSeg.replaceChildren(...prios.map(([p, label]) => h('button', {
        class: `seg-btn text ${priority === p ? 'active' : ''}`, onclick: () => { priority = p; renderControls(); load(); },
      }, label)));
      followBtn.replaceChildren(icon(follow ? 'pause' : 'play', 13), follow ? 'Live' : 'Paused');
      followBtn.classList.toggle('primary', follow);
    };
    const load = async () => {
      if (document.hidden) return;
      try {
        const res = await api('GET', `/api/logs?lines=${lines}${priority ? `&priority=${priority}` : ''}`);
        const atBottom = box.scrollTop + box.clientHeight >= box.scrollHeight - 30;
        renderLog(box, res, true);
        if (atBottom || !box.dataset.loaded) box.scrollTop = box.scrollHeight;
        box.dataset.loaded = '1';
      } catch (err) { report(err); }
    };
    renderControls();
    load();
    const t = setInterval(() => { if (follow) load(); }, 4000);
    return () => clearInterval(t);
  }
}

const timeFmt = new Intl.DateTimeFormat(undefined, { month: 'short', day: '2-digit', hour: '2-digit', minute: '2-digit', second: '2-digit' });

function renderLog(box, res, showUnit = false) {
  const rows = res.entries.map((e) => h('div', { class: `log p${Math.min(e.priority, 7)}` },
    h('span', { class: 'log-time' }, timeFmt.format(new Date(e.t))),
    showUnit ? h('span', { class: 'log-src' }, e.ident || (e.unit || '').replace(/\.service$/, '') || '—') : null,
    h('span', { class: 'log-msg' }, e.message)));
  box.replaceChildren(...[
    res.note ? h('div', { class: 'log-note' }, icon('info', 14), res.note) : null,
    ...(rows.length ? rows : [h('div', { class: 'muted pad' }, 'No log entries.')]),
  ].filter(Boolean));
  if (!showUnit) box.scrollTop = box.scrollHeight;
}
