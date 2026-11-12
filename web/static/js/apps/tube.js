// Tube: a lightweight, account-free YouTube client in the spirit of NewPipe.
// Search, trending charts, playlists, channel pages and the subscription
// feed are all resolved by the server from YouTube's public endpoints — no
// Google account, no API key, nothing tracked. Subscriptions, playlists,
// history and the region follow the user between devices through the
// server's per-user state; nothing is stored with Google.
import { h, icon, api, toast, report, ask, confirmDialog } from '../core.js';
import { registerApp } from '../wm.js';
import { device } from '../session.js';

registerApp({
  id: 'tube', name: 'Tube', icon: 'play', color: ['#f87171', '#dc2626'], width: 1060, height: 680,
  mount(win, args) { return mountTube(win, args); },
});

const REGIONS = [
  ['US', 'United States'], ['IN', 'India'], ['GB', 'United Kingdom'], ['CA', 'Canada'],
  ['AU', 'Australia'], ['DE', 'Germany'], ['FR', 'France'], ['ES', 'Spain'], ['IT', 'Italy'],
  ['NL', 'Netherlands'], ['SE', 'Sweden'], ['PL', 'Poland'], ['TR', 'Türkiye'], ['BR', 'Brazil'],
  ['MX', 'Mexico'], ['JP', 'Japan'], ['KR', 'South Korea'], ['ID', 'Indonesia'], ['PH', 'Philippines'],
  ['VN', 'Vietnam'], ['TH', 'Thailand'], ['PK', 'Pakistan'], ['BD', 'Bangladesh'], ['NG', 'Nigeria'],
  ['ZA', 'South Africa'], ['AE', 'UAE'], ['SA', 'Saudi Arabia'], ['EG', 'Egypt'], ['KE', 'Kenya'],
  ['AR', 'Argentina'],
];

const CATEGORIES = ['Music', 'Live', 'Gaming', 'News', 'Movies', 'Technology', 'Sports',
  'Comedy', 'Podcasts', 'Cooking', 'Travel', 'Science', 'Fitness', 'Cars'];

const clean = (r) => ({
  id: r.id, title: r.title || '', channel: r.channel || '', channel_id: r.channel_id || '',
  duration: r.duration || '', views: r.views || '', published: r.published || '', live: !!r.live,
});

function mountTube(win) {
  // ---- saved state (server-synced, with a local first-run fallback) ----
  const freshData = () => ({
    region: 'US', subscriptions: [], playlists: [], history: [],
    searches: [], positions: {}, sponsorblock: true, speed: 1,
  });
  let data = freshData();
  try {
    const legacy = JSON.parse(localStorage.getItem('us.tube.history') || '[]');
    if (legacy.length) data.history = legacy.map(clean);
  } catch { /* private mode */ }

  let saveTimer = 0;
  function persist() {
    clearTimeout(saveTimer);
    saveTimer = setTimeout(() => {
      api('PUT', '/api/state/tube', { device: device.id, device_name: device.name, data }).catch(() => {});
    }, 350);
  }
  async function loadState() {
    try {
      const res = await api('GET', '/api/state/tube');
      if (res.data) {
        data = { ...freshData(), ...res.data };
        regionSel.value = data.region || 'US';
      }
    } catch { /* offline: keep defaults */ }
  }

  // ---- chrome ----
  const input = h('input', {
    class: 'tube-search-input', type: 'search', placeholder: 'Search',
    'aria-label': 'Search YouTube', enterkeyhint: 'search',
  });
  const regionSel = h('select', { class: 'input tube-region', 'aria-label': 'Region', title: 'Region for search and trending' },
    ...REGIONS.map(([code, name]) => h('option', { value: code }, `${code} · ${name}`)));
  regionSel.value = data.region;

  const heading = h('h2', { class: 'tube-heading' });
  const status = h('p', { class: 'msg tube-status' });
  const chips = h('div', { class: 'tube-chips' });
  const grid = h('div', { class: 'tube-grid' });
  const scroll = h('div', { class: 'tube-scroll' }, heading, status, chips, grid);

  // ---- player ----
  const video = h('video', { class: 'tube-media', controls: true, playsinline: true });
  const audio = h('audio', { class: 'tube-audio hidden', controls: true });
  const nowTitle = h('div', { class: 'tube-now-title' });
  const nowMeta = h('div', { class: 'set-desc' });
  const nowChannel = h('button', { type: 'button', class: 'tube-chan-link' });
  const subBtn = h('button', { class: 'btn small', type: 'button' });
  const watchBtn = h('button', { class: 'btn small', type: 'button' }, icon('film', 14), 'Watch');
  const listenBtn = h('button', { class: 'btn small', type: 'button' }, icon('music', 14), 'Listen');
  const saveBtn = h('button', { class: 'btn small', type: 'button' }, icon('bookmark', 14), 'Save');
  const prevBtn = h('button', { class: 'btn small', type: 'button', title: 'Previous in queue' }, icon('skipBack', 14));
  const nextBtn = h('button', { class: 'btn small', type: 'button', title: 'Next in queue' }, icon('skipForward', 14));
  const votesEl = h('span', { class: 'tube-votes' });
  const qualitySel = h('select', { class: 'input tube-mini-sel', 'aria-label': 'Quality', title: 'Quality' },
    h('option', { value: '' }, 'Auto quality'));
  const speedSel = h('select', { class: 'input tube-mini-sel', 'aria-label': 'Playback speed', title: 'Playback speed' },
    ...[0.25, 0.5, 0.75, 1, 1.25, 1.5, 1.75, 2].map((s) =>
      h('option', { value: String(s) }, s === 1 ? '1× speed' : `${s}×`)));
  const pipBtn = h('button', { class: 'btn small', type: 'button', title: 'Picture in picture' }, icon('pip', 14));
  const dlBtn = h('button', { class: 'btn small', type: 'button', title: 'Download into Files' }, icon('download', 14), 'Download');
  const sbBtn = h('button', { class: 'btn small', type: 'button', title: 'Skip sponsor segments (SponsorBlock)' });
  const desc = h('div', { class: 'tube-desc' });
  const upNext = h('div', { class: 'tube-upnext-list' });
  const watchView = h('div', { class: 'tube-watch hidden' },
    h('div', { class: 'tube-watch-main' },
      video, audio,
      nowTitle,
      h('div', { class: 'tube-now-row' },
        h('span', { class: 'tube-avatar tube-avatar-lg' }, '?'),
        h('div', { class: 'tube-now-chan' }, nowChannel, nowMeta),
        subBtn),
      h('div', { class: 'row tube-now-actions' }, watchBtn, listenBtn, saveBtn, prevBtn, nextBtn,
        qualitySel, speedSel, pipBtn, dlBtn, sbBtn, votesEl),
      desc),
    h('aside', { class: 'tube-upnext' }, h('h3', null, 'Up next'), upNext));

  const content = h('div', { class: 'tube-content' }, scroll, watchView);

  const TABS = [
    ['home', 'Home', 'home'], ['trending', 'Trending', 'flame'],
    ['subs', 'Subscriptions', 'bell'], ['playlists', 'Playlists', 'listVideo'],
    ['history', 'History', 'history'],
  ];
  const railBtns = {};
  const rail = h('nav', { class: 'tube-rail', 'aria-label': 'Tube sections' },
    ...TABS.map(([id, label, ic]) => {
      const b = h('button', { type: 'button', class: 'tube-rail-btn', onclick: () => show(id) }, icon(ic, 20), h('span', null, label));
      railBtns[id] = b;
      return b;
    }));
  const tabBtns = {};
  const tabbar = h('nav', { class: 'tube-tabbar', 'aria-label': 'Tube sections' },
    ...TABS.map(([id, label, ic]) => {
      const b = h('button', { type: 'button', class: 'tube-tab-btn', onclick: () => show(id) }, icon(ic, 20), h('span', null, label));
      tabBtns[id] = b;
      return b;
    }));

  const searchBtn = h('button', { class: 'tube-search-btn', type: 'button', 'aria-label': 'Search' }, icon('search', 17));
  const root = h('div', { class: 'app app-tube' },
    h('header', { class: 'tube-top' },
      h('div', { class: 'tube-brand' }, h('span', { class: 'tube-logo' }, icon('play', 15)), 'Tube'),
      h('div', { class: 'tube-searchbox' }, input, searchBtn),
      regionSel),
    h('div', { class: 'tube-main' }, rail, content),
    tabbar);
  win.body.append(root);

  // ---- playback state ----
  let current = null; // entry being played
  let mode = 'video';
  let queue = [];
  let queueIndex = -1;
  let view = 'home';
  let segments = []; // SponsorBlock ranges for the current video
  let startAt = 0; // resume position for the current video

  const region = () => regionSel.value || 'US';
  const subscribed = (id) => !!id && data.subscriptions.some((s) => s.id === id);

  function streamURL() {
    let u = `/api/youtube/stream?id=${encodeURIComponent(current.id)}&mode=${mode}`;
    if (mode === 'video' && qualitySel.value) u += `&h=${qualitySel.value}`;
    return u;
  }

  function setMode(next) {
    if (!current) return;
    const from = mode === 'video' ? video : audio;
    const t = Math.max(from.currentTime || 0, startAt);
    const wasPlaying = !from.paused && !from.ended;
    mode = next;
    video.classList.toggle('hidden', mode !== 'video');
    audio.classList.toggle('hidden', mode !== 'audio');
    watchBtn.classList.toggle('primary', mode === 'video');
    listenBtn.classList.toggle('primary', mode === 'audio');
    const el = mode === 'video' ? video : audio;
    el.src = streamURL();
    el.playbackRate = Number(data.speed) || 1;
    el.addEventListener('loadedmetadata', () => {
      el.playbackRate = Number(data.speed) || 1;
      if (t > 0) el.currentTime = t;
      startAt = 0;
      if (wasPlaying) el.play().catch(() => {});
    }, { once: true });
  }

  function remember(entry) {
    data.history = [entry, ...data.history.filter((e) => e.id !== entry.id)].slice(0, 60);
    persist();
  }

  function play(entry, list) {
    current = clean(entry);
    if (Array.isArray(list) && list.length) {
      queue = list.map(clean);
      queueIndex = queue.findIndex((e) => e.id === current.id);
    } else {
      queue = [current];
      queueIndex = 0;
    }
    remember(current);
    segments = [];
    startAt = (!current.live && data.positions?.[current.id]) || 0;
    votesEl.replaceChildren();
    qualitySel.replaceChildren(h('option', { value: '' }, 'Auto quality'));
    scroll.classList.add('hidden');
    watchView.classList.remove('hidden');
    nowTitle.textContent = current.title;
    nowMeta.textContent = [current.views, current.published].filter(Boolean).join(' · ');
    nowChannel.textContent = current.channel || '';
    nowChannel.onclick = () => { if (current.channel_id) openChannel(current.channel_id, current.channel); };
    root.querySelector('.tube-avatar-lg').textContent = (current.channel || '?').trim().charAt(0).toUpperCase() || '?';
    desc.textContent = '';
    desc.classList.add('hidden');
    refreshSubBtn();
    saveBtn.onclick = (e) => openSaveMenu(current, e.currentTarget);
    prevBtn.disabled = queueIndex <= 0;
    nextBtn.disabled = queueIndex >= queue.length - 1;
    setMode(mode);
    (mode === 'video' ? video : audio).play().catch(() => {});
    win.setTitle(current.title.slice(0, 60));
    loadWatchExtras(current);
  }

  const fmtCount = (n) => {
    n = Number(n) || 0;
    if (n >= 1e6) return `${(n / 1e6).toFixed(1).replace(/\.0$/, '')}M`;
    if (n >= 1e3) return `${(n / 1e3).toFixed(1).replace(/\.0$/, '')}K`;
    return String(n);
  };

  async function loadWatchExtras(entry) {
    upNext.replaceChildren(h('p', { class: 'set-desc' }, 'Loading…'));
    try {
      const res = await api('GET', `/api/youtube/video?id=${encodeURIComponent(entry.id)}`);
      const info = res.info || {};
      if (!current || current.id !== entry.id) return;
      if (info.channel_id && !current.channel_id) {
        current.channel_id = info.channel_id;
        current.channel = info.channel || current.channel;
        nowChannel.textContent = current.channel;
        refreshSubBtn();
      }
      if (info.views) nowMeta.textContent = [`${Number(info.views).toLocaleString()} views`, current.published].filter(Boolean).join(' · ');
      if (info.description) {
        desc.textContent = info.description;
        desc.classList.remove('hidden');
      }
      if ((res.heights || []).length > 1) {
        qualitySel.replaceChildren(h('option', { value: '' }, 'Auto quality'),
          ...res.heights.map((ht) => h('option', { value: String(ht) }, `${ht}p`)));
      } else if ((res.heights || []).length === 1) {
        qualitySel.replaceChildren(h('option', { value: '' }, `Auto · ${res.heights[0]}p`));
      }
    } catch { /* playback still works without details */ }
    try {
      const v = await api('GET', `/api/youtube/votes?id=${encodeURIComponent(entry.id)}`);
      if (!current || current.id !== entry.id) return;
      votesEl.replaceChildren(
        h('span', { class: 'tube-vote' }, icon('thumbsUp', 14), fmtCount(v.likes)),
        h('span', { class: 'tube-vote' }, icon('thumbsDown', 14), fmtCount(v.dislikes)));
    } catch { /* counts are a bonus, not a requirement */ }
    if (data.sponsorblock) {
      try {
        const sb = await api('GET', `/api/youtube/sponsorblock?id=${encodeURIComponent(entry.id)}`);
        if (current && current.id === entry.id) segments = sb.segments || [];
      } catch { segments = []; }
    }
    try {
      const res = await api('GET', `/api/youtube/related?id=${encodeURIComponent(entry.id)}`);
      if (!current || current.id !== entry.id) return;
      renderUpNext((res.results || []).map(clean));
    } catch {
      if (current && current.id === entry.id) upNext.replaceChildren();
    }
  }

  function renderUpNext(list) {
    upNext.replaceChildren(...list.map((r) =>
      h('button', { type: 'button', class: 'tube-upnext-row', onclick: () => play(r, list) },
        h('span', { class: 'tube-upnext-thumb' },
          h('img', { src: `/api/youtube/thumb?id=${encodeURIComponent(r.id)}`, alt: '', loading: 'lazy' }),
          r.live ? h('span', { class: 'tube-dur tube-live-badge' }, 'LIVE') : (r.duration ? h('span', { class: 'tube-dur' }, r.duration) : null)),
        h('span', { class: 'tube-upnext-meta' },
          h('span', { class: 'tube-upnext-title' }, r.title),
          h('span', { class: 'set-desc' }, [r.channel, r.views, r.published].filter(Boolean).join(' · '))))));
  }

  function playStep(delta) {
    const next = queueIndex + delta;
    if (next < 0 || next >= queue.length) return;
    play(queue[next], queue);
  }
  prevBtn.onclick = () => playStep(-1);
  nextBtn.onclick = () => playStep(1);
  for (const el of [video, audio]) el.addEventListener('ended', () => playStep(1));
  watchBtn.onclick = () => setMode('video');
  listenBtn.onclick = () => setMode('audio');

  // SponsorBlock: step over flagged segments; keep a resume position.
  let lastPosSave = 0;
  for (const el of [video, audio]) {
    el.addEventListener('timeupdate', () => {
      if (!current || el !== (mode === 'video' ? video : audio)) return;
      const t = el.currentTime;
      if (data.sponsorblock) {
        for (const s of segments) {
          if (t >= s.segment[0] && t < s.segment[1] - 0.3) {
            el.currentTime = s.segment[1];
            toast(`Skipped ${s.category === 'music_offtopic' ? 'non-music' : s.category}`);
            break;
          }
        }
      }
      const now = Date.now();
      if (now - lastPosSave > 5000 && el.duration > 30) {
        lastPosSave = now;
        if (t > 5 && t < el.duration - 15) data.positions[current.id] = Math.floor(t);
        else if (t >= el.duration - 15) delete data.positions[current.id];
        persist();
      }
    });
  }

  speedSel.value = String(data.speed || 1);
  speedSel.onchange = () => {
    data.speed = Number(speedSel.value) || 1;
    for (const el of [video, audio]) el.playbackRate = data.speed;
    persist();
  };
  qualitySel.onchange = () => { if (current && mode === 'video') setMode('video'); };
  pipBtn.onclick = async () => {
    try {
      if (document.pictureInPictureElement) await document.exitPictureInPicture();
      else if (mode === 'video') await video.requestPictureInPicture();
    } catch { toast('Picture in picture is not available here.'); }
  };
  dlBtn.onclick = async () => {
    if (!current) return;
    dlBtn.disabled = true;
    try {
      await api('POST', '/api/youtube/download', { id: current.id, mode });
      toast('Downloading into Files, in the Downloads folder.');
    } catch (err) {
      toast(err.message || 'Download failed to start.');
    }
    dlBtn.disabled = false;
  };
  function refreshSbBtn() {
    sbBtn.textContent = `SponsorBlock ${data.sponsorblock ? 'on' : 'off'}`;
    sbBtn.classList.toggle('primary', !!data.sponsorblock);
  }
  sbBtn.onclick = () => {
    data.sponsorblock = !data.sponsorblock;
    if (!data.sponsorblock) segments = [];
    persist();
    refreshSbBtn();
  };
  refreshSbBtn();

  function refreshSubBtn() {
    const on = subscribed(current?.channel_id);
    subBtn.textContent = on ? 'Subscribed' : 'Subscribe';
    subBtn.classList.toggle('primary', !on);
    subBtn.onclick = () => {
      if (!current?.channel_id) { toast('This video has no channel to subscribe to.'); return; }
      toggleSubscribe({ id: current.channel_id, name: current.channel || 'Channel' });
      refreshSubBtn();
    };
  }

  function toggleSubscribe(ch) {
    if (subscribed(ch.id)) {
      data.subscriptions = data.subscriptions.filter((s) => s.id !== ch.id);
      toast(`Unsubscribed from ${ch.name}`);
    } else {
      data.subscriptions = [...data.subscriptions, { id: ch.id, name: ch.name }];
      toast(`Subscribed to ${ch.name}`);
    }
    persist();
    if (view === 'subs') showSubs();
  }

  // ---- cards ----
  function openSaveMenu(entry, anchor) {
    document.querySelectorAll('.tube-menu').forEach((m) => m.remove());
    const menu = h('div', { class: 'tube-menu' },
      h('div', { class: 'tube-menu-title' }, 'Save to playlist'),
      ...data.playlists.map((p) => h('button', {
        type: 'button', class: 'tube-menu-item',
        onclick: () => { addToPlaylist(p, entry); menu.remove(); },
      }, icon(p.videos.some((v) => v.id === entry.id) ? 'check' : 'plus', 14), p.name)),
      h('button', {
        type: 'button', class: 'tube-menu-item',
        onclick: async () => {
          menu.remove();
          const name = await ask('New playlist', 'Playlist name');
          if (!name) return;
          const p = { id: `p${Date.now().toString(36)}`, name, videos: [] };
          data.playlists = [...data.playlists, p];
          addToPlaylist(p, entry);
        },
      }, icon('plus', 14), 'New playlist…'));
    anchor.closest('.tube-card, .tube-watch-main, .tube-upnext-row')?.append(menu) || root.append(menu);
    menu.classList.add('open');
    setTimeout(() => document.addEventListener('pointerdown', function off(e) {
      if (!menu.contains(e.target)) { menu.remove(); document.removeEventListener('pointerdown', off); }
    }), 0);
  }

  function addToPlaylist(p, entry) {
    const e = clean(entry);
    if (p.videos.some((v) => v.id === e.id)) {
      p.videos = p.videos.filter((v) => v.id !== e.id);
      toast(`Removed from ${p.name}`);
    } else {
      p.videos = [...p.videos, e];
      toast(`Saved to ${p.name}`);
    }
    persist();
    if (view === 'playlists') showPlaylists();
    if (view === 'playlist') openPlaylist(p.id);
  }

  function card(r, list) {
    const save = h('button', {
      type: 'button', class: 'tube-save', title: 'Save to playlist', 'aria-label': 'Save to playlist',
      onclick: (e) => { e.stopPropagation(); openSaveMenu(r, e.currentTarget); },
    }, icon('bookmark', 15));
    return h('div', { class: 'tube-card' },
      h('button', { type: 'button', class: 'tube-thumbbtn', onclick: () => play(r, list) },
        h('img', { src: `/api/youtube/thumb?id=${encodeURIComponent(r.id)}`, alt: '', loading: 'lazy' }),
        r.live ? h('span', { class: 'tube-dur tube-live-badge' }, 'LIVE') : (r.duration ? h('span', { class: 'tube-dur' }, r.duration) : null),
        save),
      h('div', { class: 'tube-card-meta' },
        h('span', { class: 'tube-avatar' }, (r.channel || '?').trim().charAt(0).toUpperCase() || '?'),
        h('div', { class: 'tube-card-text' },
          h('button', { type: 'button', class: 'tube-card-title', onclick: () => play(r, list) }, r.title),
          r.channel ? h('button', {
            type: 'button', class: 'tube-card-chan',
            onclick: () => { if (r.channel_id) openChannel(r.channel_id, r.channel); },
          }, r.channel) : null,
          h('span', { class: 'set-desc' }, [r.views, r.published].filter(Boolean).join(' · ')))));
  }

  function renderGrid(list, emptyMsg) {
    grid.replaceChildren();
    if (!list.length) {
      status.textContent = emptyMsg || 'Nothing here yet.';
      return;
    }
    status.textContent = '';
    grid.append(...list.map((r) => card(r, list)));
  }

  function setChips(cats) {
    chips.replaceChildren(...cats.map((c) => h('button', {
      type: 'button', class: 'tube-chip', onclick: () => runSearch(c === 'Live' ? 'live stream' : c.toLowerCase()),
    }, c)));
  }

  function setActiveTab() {
    for (const [id] of TABS) {
      railBtns[id]?.classList.toggle('active', view === id);
      tabBtns[id]?.classList.toggle('active', view === id);
    }
  }

  function beginLoad(title) {
    watchView.classList.add('hidden');
    scroll.classList.remove('hidden');
    heading.textContent = title;
    status.textContent = 'Loading…';
    grid.replaceChildren();
    chips.replaceChildren();
    setActiveTab();
    content.scrollTop = 0;
  }

  // ---- views ----
  async function showHome() {
    view = 'home';
    beginLoad('Home');
    setChips(CATEGORIES);
    try {
      const res = await api('GET', `/api/youtube/trending?region=${region()}`);
      if (view !== 'home') return;
      heading.textContent = res.title || 'Popular right now';
      renderGrid((res.results || []).map(clean), 'Nothing to show right now. Try a search.');
    } catch (err) {
      status.textContent = err.message || 'Could not load.';
    }
  }

  async function showTrending() {
    view = 'trending';
    beginLoad('Trending');
    try {
      const res = await api('GET', `/api/youtube/trending?region=${region()}`);
      if (view !== 'trending') return;
      heading.textContent = res.title || `Trending in ${region()}`;
      renderGrid((res.results || []).map(clean), 'Nothing trending right now.');
    } catch (err) {
      status.textContent = err.message || 'Could not load.';
    }
  }

  let subGroup = 'All';

  async function showSubs() {
    view = 'subs';
    beginLoad('Subscriptions');
    if (!data.subscriptions.length) {
      status.textContent = 'No subscriptions yet. Open a video and tap Subscribe — new uploads land here, no account needed.';
      return;
    }
    const groups = [...new Set(data.subscriptions.map((s) => s.group).filter(Boolean))].sort();
    if (subGroup !== 'All' && !groups.includes(subGroup)) subGroup = 'All';
    const groupSel = h('select', { class: 'input tube-mini-sel', 'aria-label': 'Subscription group' },
      h('option', { value: 'All' }, 'All channels'),
      ...groups.map((g) => h('option', { value: g }, g)));
    groupSel.value = subGroup;
    groupSel.onchange = () => { subGroup = groupSel.value; showSubs(); };
    chips.replaceChildren(
      h('span', { class: 'tube-import' }, h('span', { class: 'set-desc' }, 'Group:'), groupSel),
      ...data.subscriptions.map((s) => h('span', { class: 'tube-chip tube-sub-chip' },
        h('button', { type: 'button', class: 'tube-sub-name', onclick: () => openChannel(s.id, s.name) }, s.name),
        h('button', {
          type: 'button', class: 'tube-sub-group', title: 'Set group',
          onclick: async () => {
            const g = await ask(`Group for ${s.name}`, 'Group name (leave empty to clear)', s.group || '');
            if (g === null) return;
            s.group = g || undefined;
            persist();
            showSubs();
          },
        }, s.group || '+ group'),
        h('button', {
          type: 'button', class: 'tube-sub-x', 'aria-label': `Unsubscribe from ${s.name}`,
          onclick: () => toggleSubscribe(s),
        }, icon('x', 12)))));
    try {
      const res = await api('POST', '/api/youtube/feed', { channels: data.subscriptions.map((s) => s.id) });
      if (view !== 'subs') return;
      let list = (res.results || []).map(clean);
      if (subGroup !== 'All') {
        const inGroup = new Set(data.subscriptions.filter((s) => s.group === subGroup).map((s) => s.id));
        list = list.filter((r) => inGroup.has(r.channel_id));
      }
      renderGrid(list, subGroup === 'All' ? 'No new uploads from your channels.' : `No new uploads in ${subGroup}.`);
    } catch (err) {
      status.textContent = err.message || 'Could not load the feed.';
    }
  }

  function showPlaylists() {
    view = 'playlists';
    beginLoad('Playlists');
    const importInput = h('input', {
      class: 'input tube-import-input', type: 'url',
      placeholder: 'Paste a YouTube playlist link to save it', 'aria-label': 'Playlist link',
    });
    const doImport = async () => {
      const ref = importInput.value.trim();
      if (!ref) return;
      status.textContent = 'Importing…';
      try {
        const info = await api('GET', `/api/youtube/playlist?id=${encodeURIComponent(ref)}`);
        if (data.playlists.some((p) => p.yt === info.id)) { toast('That playlist is already saved.'); return; }
        data.playlists = [...data.playlists, {
          id: `p${Date.now().toString(36)}`, name: info.title || 'YouTube playlist',
          yt: info.id, videos: (info.results || []).map(clean),
        }];
        persist();
        toast('Playlist saved.');
        showPlaylists();
      } catch (err) {
        status.textContent = err.message || 'Could not import that playlist.';
      }
    };
    importInput.addEventListener('keydown', (e) => { if (e.key === 'Enter') doImport(); });
    chips.replaceChildren(
      h('div', { class: 'tube-import' }, importInput,
        h('button', { class: 'btn small', type: 'button', onclick: doImport }, icon('download', 14), 'Import'),
        h('button', {
          class: 'btn small', type: 'button',
          onclick: async () => {
            const name = await ask('New playlist', 'Playlist name');
            if (!name) return;
            data.playlists = [...data.playlists, { id: `p${Date.now().toString(36)}`, name, videos: [] }];
            persist();
            showPlaylists();
          },
        }, icon('plus', 14), 'New')));
    if (!data.playlists.length) {
      status.textContent = 'No playlists yet. Import one from YouTube, or save videos with the bookmark button.';
      return;
    }
    status.textContent = '';
    grid.replaceChildren(...data.playlists.map((p) => h('div', { class: 'tube-card' },
      h('button', { type: 'button', class: 'tube-thumbbtn', onclick: () => openPlaylist(p.id) },
        p.videos[0]
          ? h('img', { src: `/api/youtube/thumb?id=${encodeURIComponent(p.videos[0].id)}`, alt: '', loading: 'lazy' })
          : h('span', { class: 'tube-thumb-empty' }, icon('listVideo', 34)),
        h('span', { class: 'tube-dur' }, `${p.videos.length} videos`)),
      h('div', { class: 'tube-card-text' },
        h('button', { type: 'button', class: 'tube-card-title', onclick: () => openPlaylist(p.id) }, p.name),
        h('span', { class: 'set-desc' }, p.yt ? 'From YouTube' : 'Your playlist')),
      h('div', { class: 'row' },
        h('button', {
          class: 'btn small primary', type: 'button', disabled: !p.videos.length,
          onclick: () => { if (p.videos.length) play(p.videos[0], p.videos); },
        }, icon('play', 13), 'Play all'),
        h('button', {
          class: 'btn small danger', type: 'button',
          onclick: async () => {
            if (!await confirmDialog('Delete playlist?', `"${p.name}" will be removed from Tube. The videos stay on YouTube.`)) return;
            data.playlists = data.playlists.filter((x) => x.id !== p.id);
            persist();
            showPlaylists();
          },
        }, icon('trash', 13))))));
  }

  function openPlaylist(pid) {
    const p = data.playlists.find((x) => x.id === pid);
    if (!p) { showPlaylists(); return; }
    view = 'playlist';
    beginLoad(p.name);
    chips.replaceChildren(
      h('button', { class: 'btn small', type: 'button', onclick: showPlaylists }, icon('chevronLeft', 14), 'All playlists'),
      h('button', {
        class: 'btn small primary', type: 'button', disabled: !p.videos.length,
        onclick: () => { if (p.videos.length) play(p.videos[0], p.videos); },
      }, icon('play', 13), 'Play all'),
      p.yt ? h('button', {
        class: 'btn small', type: 'button',
        onclick: async () => {
          status.textContent = 'Refreshing…';
          try {
            const info = await api('GET', `/api/youtube/playlist?id=${encodeURIComponent(p.yt)}`);
            p.videos = (info.results || []).map(clean);
            if (info.title) p.name = info.title;
            persist();
            openPlaylist(p.id);
          } catch (err) { status.textContent = err.message || 'Refresh failed.'; }
        },
      }, icon('history', 13), 'Refresh') : null);
    renderGrid(p.videos, 'This playlist is empty. Save videos with the bookmark button.');
  }

  function showHistory() {
    view = 'history';
    beginLoad('History');
    const bits = [];
    if (data.searches.length) {
      bits.push(h('span', { class: 'set-desc tube-search-label' }, 'Recent searches:'),
        ...data.searches.map((s) => h('button', {
          type: 'button', class: 'tube-chip', onclick: () => runSearch(s),
        }, icon('search', 13), ` ${s}`)));
    }
    if (data.history.length) {
      bits.push(h('button', {
        class: 'btn small danger', type: 'button',
        onclick: async () => {
          if (!await confirmDialog('Clear history?', 'Your Tube watch and search history on this account will be cleared.')) return;
          data.history = [];
          data.searches = [];
          data.positions = {};
          persist();
          showHistory();
        },
      }, icon('trash', 13), 'Clear history'));
    }
    if (bits.length) chips.replaceChildren(...bits);
    renderGrid(data.history, 'Nothing watched yet. History stays on your ultraspace account, not with Google.');
  }

  async function openChannel(id, name) {
    view = 'channel';
    beginLoad(name || 'Channel');
    const on = subscribed(id);
    chips.replaceChildren(h('button', {
      class: `btn small${on ? '' : ' primary'}`, type: 'button',
      onclick: (e) => {
        toggleSubscribe({ id, name: heading.textContent || name || 'Channel' });
        e.currentTarget.textContent = subscribed(id) ? 'Subscribed' : 'Subscribe';
        e.currentTarget.classList.toggle('primary', !subscribed(id));
      },
    }, on ? 'Subscribed' : 'Subscribe'));
    try {
      const info = await api('GET', `/api/youtube/channel?id=${encodeURIComponent(id)}`);
      if (view !== 'channel') return;
      if (info.name) heading.textContent = info.name;
      renderGrid((info.results || []).map(clean), 'No videos found for this channel.');
    } catch (err) {
      status.textContent = err.message || 'Could not load this channel.';
    }
  }

  async function runSearch(q) {
    const query = (q ?? input.value).trim();
    if (!query) { showHome(); return; }
    if (q !== undefined) input.value = query;
    data.searches = [query, ...data.searches.filter((s) => s.toLowerCase() !== query.toLowerCase())].slice(0, 15);
    persist();
    view = 'search';
    beginLoad(`Results for “${query}”`);
    try {
      const res = await api('GET', `/api/youtube/search?q=${encodeURIComponent(query)}&region=${region()}`);
      if (view !== 'search') return;
      renderGrid((res.results || []).map(clean), 'Nothing found.');
    } catch (err) {
      status.textContent = err.message || 'Search failed.';
      if (err) report(err);
    }
  }

  function show(tab) {
    ({ home: showHome, trending: showTrending, subs: showSubs, playlists: showPlaylists, history: showHistory })[tab]?.();
  }

  searchBtn.onclick = () => runSearch();
  input.addEventListener('keydown', (e) => { if (e.key === 'Enter') runSearch(); });
  regionSel.onchange = () => {
    data.region = region();
    persist();
    if (view === 'home' || view === 'trending') show(view);
    else if (view === 'search' && input.value.trim()) runSearch();
    toast(`Region set to ${region()}`);
  };

  loadState().then(() => {
    regionSel.value = data.region || 'US';
    speedSel.value = String(data.speed || 1);
    refreshSbBtn();
    showHome();
  });
  setTimeout(() => input.focus(), 50);

  return {
    onDestroy() {
      clearTimeout(saveTimer);
      for (const el of [video, audio]) {
        try { el.pause(); el.removeAttribute('src'); el.load(); } catch { /* already gone */ }
      }
    },
    saveState: () => ({}),
  };
}
