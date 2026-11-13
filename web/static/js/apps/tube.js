// Tube: a lightweight, account-free YouTube client in the spirit of NewPipe.
// Search, trending charts, playlists, channel pages and the subscription
// feed are all resolved by the server from YouTube's public endpoints — no
// Google account, no API key, nothing tracked. Subscriptions, playlists,
// history and the region follow the user between devices through the
// server's per-user state; nothing is stored with Google.
import { h, icon, api, toast, report, ask, confirmDialog, menu, fmtClock } from '../core.js';
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

const CATEGORIES = ['Shorts', 'Music', 'Live', 'Gaming', 'News', 'Movies', 'Technology', 'Sports',
  'Comedy', 'Podcasts', 'Cooking', 'Travel', 'Science', 'Fitness', 'Cars'];

const clean = (r) => ({
  id: r.id, kind: r.kind || 'video', title: r.title || '', channel: r.channel || '',
  channel_id: r.channel_id || '', duration: r.duration || '', views: r.views || '',
  published: r.published || '', live: !!r.live, count: r.count || '', thumb: r.thumb || '',
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
  const shortsShelf = h('div', { class: 'tube-shorts hidden' });
  const grid = h('div', { class: 'tube-grid' });
  const scroll = h('div', { class: 'tube-scroll' }, heading, status, chips, shortsShelf, grid);

  // ---- player elements ----
  const video = h('video', { class: 'tube-video', playsinline: true, preload: 'metadata' });
  try { video.volume = Number(localStorage.getItem('us.tube.volume') ?? 1); } catch { /* default */ }
  const spinner = h('div', { class: 'tube-spinner hidden' }, h('span', { class: 'bw-spin big' }));
  const flash = h('div', { class: 'tube-flash' });

  // Audio mode visual card
  const audioThumbBlur = h('div', { class: 'tube-audiocard-blur' });
  const audioThumbImg = h('img', { class: 'tube-audiocard-thumb', alt: '' });
  const audioCardTitle = h('div', { class: 'tube-audiocard-title' });
  const audioCardChan = h('div', { class: 'tube-audiocard-chan' });
  const audioCardBadge = h('div', { class: 'tube-audiocard-badge' }, icon('music', 13), 'Audio mode · Screen off saving');
  const audioCard = h('div', { class: 'tube-audiocard hidden' },
    audioThumbBlur,
    h('div', { class: 'tube-audiocard-body' },
      audioThumbImg,
      audioCardTitle,
      audioCardChan,
      audioCardBadge));

  // Custom Seekbar
  const played = h('div', { class: 'tube-seek-played' });
  const buffered = h('div', { class: 'tube-seek-buffered' });
  const segmentsBox = h('div', { class: 'tube-seek-segments' });
  const hoverTime = h('div', { class: 'tube-seek-hover hidden' });
  const seekbar = h('div', { class: 'tube-seek', role: 'slider', 'aria-label': 'Seek', tabindex: '0' },
    h('div', { class: 'tube-seek-track' }, buffered, segmentsBox, played),
    hoverTime);

  // Controls bar
  const timeEl = h('span', { class: 'tube-ctrl-time' }, '0:00 / 0:00');
  const prevBtn = h('button', { class: 'tube-ctrl-btn', type: 'button', title: 'Previous (P)' }, icon('skipBack', 18));
  const playBtn = h('button', { class: 'tube-ctrl-btn tube-play-btn', type: 'button', title: 'Play (Space)' }, icon('play', 20));
  const nextBtn = h('button', { class: 'tube-ctrl-btn', type: 'button', title: 'Next (N)' }, icon('skipForward', 18));
  const volBtn = h('button', { class: 'tube-ctrl-btn', type: 'button', title: 'Mute (M)' }, icon('volume', 18));
  const vol = h('input', { type: 'range', class: 'range tube-vol', min: '0', max: '100', value: String(Math.round(video.volume * 100)), 'aria-label': 'Volume' });

  const watchPill = h('button', { class: 'tube-ctrl-pill active', type: 'button', title: 'Video mode' }, icon('film', 13), 'Watch');
  const listenPill = h('button', { class: 'tube-ctrl-pill', type: 'button', title: 'Audio-only mode' }, icon('music', 13), 'Listen');
  const speedBtn = h('button', { class: 'tube-ctrl-pill', type: 'button', title: 'Playback speed' }, '1×');
  const qualBtn = h('button', { class: 'tube-ctrl-pill hidden', type: 'button', title: 'Video quality' }, 'Auto');
  const pipBtn = h('button', { class: 'tube-ctrl-btn', type: 'button', title: 'Picture in picture' }, icon('pip', 17));
  const fsBtn = h('button', { class: 'tube-ctrl-btn', type: 'button', title: 'Full screen (F)' }, icon('maximize', 17));

  const controls = h('div', { class: 'tube-controls' },
    seekbar,
    h('div', { class: 'tube-ctrl-row' },
      prevBtn, playBtn, nextBtn,
      volBtn, vol, timeEl,
      h('span', { class: 'tb-spacer' }),
      watchPill, listenPill,
      speedBtn, qualBtn, pipBtn, fsBtn));

  const stage = h('div', { class: 'tube-stage', tabindex: '0' },
    video, audioCard, spinner, flash, controls);

  // Watch Header (Back button + context breadcrumb)
  const backBtn = h('button', { class: 'btn small tube-back-btn', type: 'button' }, icon('chevronLeft', 15), 'Back');
  const watchCrumb = h('span', { class: 'tube-watch-breadcrumb' }, 'Now playing');
  const watchHeader = h('div', { class: 'tube-watch-header' }, backBtn, watchCrumb);

  // Now Playing Creator Row & Actions
  const nowTitle = h('h2', { class: 'tube-now-title' });
  const nowChannel = h('button', { type: 'button', class: 'tube-chan-link' });
  const nowSubDesc = h('span', { class: 'set-desc' });
  const avatarEl = h('span', { class: 'tube-avatar-lg' }, '?');
  const subBtn = h('button', { class: 'btn small primary', type: 'button' });
  const votesEl = h('div', { class: 'tube-votes-pill' });
  const saveBtn = h('button', { class: 'tube-action-pill', type: 'button' }, icon('bookmark', 14), 'Save');
  const dlBtn = h('button', { class: 'tube-action-pill', type: 'button' }, icon('download', 14), 'Download');
  const sbBtn = h('button', { class: 'tube-action-pill', type: 'button' }, icon('shield', 14), 'SponsorBlock');

  const nowRow = h('div', { class: 'tube-now-row' },
    h('div', { class: 'tube-now-creator' },
      avatarEl,
      h('div', { class: 'tube-now-chan' }, nowChannel, nowSubDesc),
      subBtn),
    h('div', { class: 'tube-now-actions' },
      votesEl, saveBtn, dlBtn, sbBtn));

  // Description Card
  const nowMeta = h('div', { class: 'tube-desc-header' });
  const descText = h('div', { class: 'tube-desc-text clamped' });
  const descMoreBtn = h('button', { class: 'tube-desc-more', type: 'button' }, 'Show more');
  const descCard = h('div', { class: 'tube-desc-card hidden' }, nowMeta, descText, descMoreBtn);

  // Up Next / Queue
  const upNextTitle = h('h3', null, 'Up next');
  const upNextBadge = h('span', { class: 'tube-upnext-badge hidden' });
  const upNextHeader = h('div', { class: 'tube-upnext-header' }, upNextTitle, upNextBadge);
  const upNext = h('div', { class: 'tube-upnext-list' });
  const aside = h('aside', { class: 'tube-upnext' }, upNextHeader, upNext);

  const watchView = h('div', { class: 'tube-watch hidden' },
    h('div', { class: 'tube-watch-main' },
      watchHeader,
      stage,
      nowTitle,
      nowRow,
      descCard),
    aside);

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

  let selectedHeight = '';
  let currentHeights = [];
  function streamURL() {
    let u = `/api/youtube/stream?id=${encodeURIComponent(current.id)}&mode=${mode}`;
    if (mode === 'video' && selectedHeight) u += `&h=${selectedHeight}`;
    return u;
  }

  function setMode(next) {
    if (!current) return;
    const t = Math.max(video.currentTime || 0, startAt);
    const wasPlaying = !video.paused && !video.ended;
    mode = next;
    watchPill.classList.toggle('active', mode === 'video');
    listenPill.classList.toggle('active', mode === 'audio');
    video.classList.toggle('tube-media-audio', mode === 'audio');
    audioCard.classList.toggle('hidden', mode !== 'audio');
    stage.classList.toggle('tube-audio-mode', mode === 'audio');
    if (mode === 'audio') {
      const thumb = `/api/youtube/thumb?id=${encodeURIComponent(current.id)}`;
      audioThumbBlur.style.backgroundImage = `url("${thumb}")`;
      audioThumbImg.src = thumb;
      audioCardTitle.textContent = current.title;
      audioCardChan.textContent = current.channel || '';
    }
    video.src = streamURL();
    video.playbackRate = Number(data.speed) || 1;
    video.addEventListener('loadedmetadata', () => {
      video.playbackRate = Number(data.speed) || 1;
      if (t > 0) video.currentTime = t;
      startAt = 0;
      renderSegments();
      paint();
      if (wasPlaying) video.play().catch(() => {});
    }, { once: true });
  }

  function remember(entry) {
    data.history = [entry, ...data.history.filter((e) => e.id !== entry.id)].slice(0, 60);
    persist();
  }

  function closeWatch() {
    watchView.classList.add('hidden');
    scroll.classList.remove('hidden');
    win.setTitle('Tube');
  }
  backBtn.onclick = () => closeWatch();

  let descExpanded = false;
  descMoreBtn.onclick = () => {
    descExpanded = !descExpanded;
    descText.classList.toggle('clamped', !descExpanded);
    descMoreBtn.textContent = descExpanded ? 'Show less' : 'Show more';
  };

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
    selectedHeight = '';
    currentHeights = [];
    qualBtn.textContent = 'Auto';
    qualBtn.classList.add('hidden');
    scroll.classList.add('hidden');
    watchView.classList.remove('hidden');
    stage.classList.toggle('tube-shorts-stage', current.kind === 'short');
    watchCrumb.textContent = queue.length > 1 ? `Playing from queue (${queueIndex + 1}/${queue.length})` : 'Now playing';
    nowTitle.textContent = current.title;
    nowMeta.textContent = [current.views, current.published].filter(Boolean).join(' · ');
    nowChannel.textContent = current.channel || '';
    nowSubDesc.textContent = '';
    nowChannel.onclick = () => { if (current.channel_id) openChannel(current.channel_id, current.channel); };
    avatarEl.textContent = (current.channel || '?').trim().charAt(0).toUpperCase() || '?';
    descText.textContent = '';
    descCard.classList.add('hidden');
    refreshSubBtn();
    saveBtn.onclick = (e) => openSaveMenu(current, e.currentTarget);
    prevBtn.disabled = queueIndex <= 0;
    nextBtn.disabled = queueIndex >= queue.length - 1;
    setMode(mode);
    video.play().catch(() => {});
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
        descText.textContent = info.description;
        descExpanded = false;
        descText.classList.add('clamped');
        descMoreBtn.textContent = 'Show more';
        descCard.classList.remove('hidden');
      }
      if ((res.heights || []).length > 1) {
        currentHeights = res.heights;
        qualBtn.classList.remove('hidden');
        qualBtn.textContent = selectedHeight ? `${selectedHeight}p` : 'Auto';
      } else {
        currentHeights = [];
        qualBtn.classList.add('hidden');
      }
    } catch { /* playback still works without details */ }
    try {
      const v = await api('GET', `/api/youtube/votes?id=${encodeURIComponent(entry.id)}`);
      if (!current || current.id !== entry.id) return;
      votesEl.replaceChildren(
        h('button', { class: 'tube-vote-btn', type: 'button' }, icon('thumbsUp', 14), fmtCount(v.likes)),
        h('span', { class: 'tube-vote-sep' }),
        h('button', { class: 'tube-vote-btn', type: 'button' }, icon('thumbsDown', 14), fmtCount(v.dislikes)));
    } catch { /* counts are a bonus, not a requirement */ }
    if (data.sponsorblock) {
      try {
        const sb = await api('GET', `/api/youtube/sponsorblock?id=${encodeURIComponent(entry.id)}`);
        if (current && current.id === entry.id) {
          segments = sb.segments || [];
          renderSegments();
        }
      } catch { segments = []; }
    }
    try {
      const res = await api('GET', `/api/youtube/related?id=${encodeURIComponent(entry.id)}`);
      if (!current || current.id !== entry.id) return;
      renderUpNext(queue.length > 1 ? queue : (res.results || []).map(clean));
    } catch {
      if (current && current.id === entry.id) {
        if (queue.length > 1) renderUpNext(queue);
        else upNext.replaceChildren();
      }
    }
  }

  function renderUpNext(list) {
    if (queue.length > 1) {
      upNextTitle.textContent = 'Queue';
      upNextBadge.textContent = `${queueIndex + 1} / ${queue.length}`;
      upNextBadge.classList.remove('hidden');
    } else {
      upNextTitle.textContent = 'Up next';
      upNextBadge.classList.add('hidden');
    }
    upNext.replaceChildren(...list.map((r) => {
      const isCur = current && r.id === current.id;
      return h('button', {
        type: 'button',
        class: `tube-upnext-row ${isCur ? 'active' : ''}`,
        onclick: () => play(r, list),
      },
        h('span', { class: 'tube-upnext-thumb' },
          h('img', { src: `/api/youtube/thumb?id=${encodeURIComponent(r.id)}`, alt: '', loading: 'lazy' }),
          r.live ? h('span', { class: 'tube-dur tube-live-badge' }, 'LIVE') : (r.duration ? h('span', { class: 'tube-dur' }, r.duration) : null)),
        h('span', { class: 'tube-upnext-meta' },
          h('span', { class: 'tube-upnext-title' }, r.title),
          h('span', { class: 'set-desc' }, [r.channel, r.views, r.published].filter(Boolean).join(' · '))));
    }));
  }

  function playStep(delta) {
    const next = queueIndex + delta;
    if (next < 0 || next >= queue.length) return;
    play(queue[next], queue);
  }
  prevBtn.onclick = (e) => { e.stopPropagation(); playStep(-1); };
  nextBtn.onclick = (e) => { e.stopPropagation(); playStep(1); };
  video.addEventListener('ended', () => playStep(1));
  watchPill.onclick = (e) => { e.stopPropagation(); setMode('video'); };
  listenPill.onclick = (e) => { e.stopPropagation(); setMode('audio'); };

  // Controls auto-hide
  let hideTimer = null;
  function pokeControls() {
    stage.classList.remove('idle');
    clearTimeout(hideTimer);
    hideTimer = setTimeout(() => {
      if (!video.paused && !controls.matches(':hover')) {
        stage.classList.add('idle');
      }
    }, 2600);
  }
  stage.addEventListener('pointermove', pokeControls);

  function togglePlay() {
    if (video.paused) {
      video.play().catch(report);
      showFlash('play');
    } else {
      video.pause();
      showFlash('pause');
    }
  }

  function showFlash(ic) {
    flash.replaceChildren(icon(ic, 28));
    flash.classList.remove('on');
    void flash.offsetWidth;
    flash.classList.add('on');
  }

  playBtn.onclick = (e) => { e.stopPropagation(); togglePlay(); };
  stage.onclick = (e) => {
    if (e.target.closest('.tube-controls, .tube-seek, button, select, input')) return;
    if (matchMedia('(pointer: coarse)').matches && stage.classList.contains('idle')) {
      pokeControls();
      return;
    }
    togglePlay();
  };
  stage.ondblclick = (e) => {
    if (e.target.closest('.tube-controls, .tube-seek, button, select, input')) return;
    fullscreen();
  };

  function fullscreen() {
    if (document.fullscreenElement) {
      document.exitFullscreen().catch(() => {});
      return;
    }
    if (stage.requestFullscreen) stage.requestFullscreen().catch(() => video.webkitEnterFullscreen?.());
    else video.webkitEnterFullscreen?.();
  }
  fsBtn.onclick = (e) => { e.stopPropagation(); fullscreen(); };

  function showVol() {
    volBtn.replaceChildren(icon(video.muted || video.volume === 0 ? 'volumeX' : 'volume', 18));
    vol.value = String(Math.round((video.muted ? 0 : video.volume) * 100));
  }
  volBtn.onclick = (e) => {
    e.stopPropagation();
    video.muted = !video.muted;
    showVol();
  };
  vol.oninput = (e) => {
    e.stopPropagation();
    video.volume = Number(vol.value) / 100;
    video.muted = false;
    try { localStorage.setItem('us.tube.volume', String(video.volume)); } catch {}
    showVol();
  };
  showVol();

  // Custom Seekbar logic
  const pct = (t) => {
    const d = video.duration || 0;
    return d > 0 ? `${Math.min(100, (100 * t) / d)}%` : '0%';
  };

  function paint() {
    const t = video.currentTime || 0;
    const d = video.duration || 0;
    if (d > 0) {
      played.style.width = pct(t);
      let end = 0;
      for (let i = 0; i < video.buffered.length; i++) {
        if (video.buffered.start(i) <= t + 1) end = Math.max(end, video.buffered.end(i));
      }
      buffered.style.width = pct(end);
      timeEl.textContent = `${fmtClock(t)} / ${fmtClock(d)}`;
    } else {
      timeEl.textContent = current?.live ? 'LIVE' : `${fmtClock(t)}`;
    }
  }

  function renderSegments() {
    segmentsBox.replaceChildren();
    const d = video.duration || 0;
    if (!d || !segments.length) return;
    for (const s of segments) {
      const start = s.segment[0];
      const end = s.segment[1];
      const segEl = h('div', { class: 'tube-seek-segment' });
      segEl.style.left = `${(start / d) * 100}%`;
      segEl.style.width = `${Math.max(0.5, ((end - start) / d) * 100)}%`;
      segmentsBox.append(segEl);
    }
  }

  const atPointer = (e) => {
    const r = seekbar.getBoundingClientRect();
    const d = video.duration || 0;
    return Math.max(0, Math.min(1, (e.clientX - r.left) / r.width)) * d;
  };

  seekbar.addEventListener('pointermove', (e) => {
    const d = video.duration || 0;
    if (!d) return;
    const t = atPointer(e);
    hoverTime.textContent = fmtClock(t);
    hoverTime.style.left = `${(100 * t) / d}%`;
    hoverTime.classList.remove('hidden');
  });

  seekbar.addEventListener('pointerleave', () => hoverTime.classList.add('hidden'));

  seekbar.addEventListener('pointerdown', (e) => {
    e.preventDefault();
    seekbar.setPointerCapture(e.pointerId);
    hoverTime.classList.remove('hidden');
    const move = (ev) => {
      const t = atPointer(ev);
      const d = video.duration || 0;
      if (d) played.style.width = `${(100 * t) / d}%`;
      hoverTime.textContent = fmtClock(t);
      hoverTime.style.left = `${d ? (100 * t) / d : 0}%`;
    };
    const up = (ev) => {
      seekbar.removeEventListener('pointermove', move);
      seekbar.removeEventListener('pointerup', up);
      hoverTime.classList.add('hidden');
      const t = atPointer(ev);
      if (Number.isFinite(t)) video.currentTime = t;
    };
    seekbar.addEventListener('pointermove', move);
    seekbar.addEventListener('pointerup', up);
    move(e);
  });

  // SponsorBlock & playback tracking
  let lastPosSave = 0;
  video.addEventListener('timeupdate', () => {
    paint();
    if (!current) return;
    const t = video.currentTime;
    if (data.sponsorblock && segments.length) {
      for (const s of segments) {
        if (t >= s.segment[0] && t < s.segment[1] - 0.3) {
          video.currentTime = s.segment[1];
          toast(`Skipped ${s.category === 'music_offtopic' ? 'non-music' : s.category}`);
          break;
        }
      }
    }
    const now = Date.now();
    if (now - lastPosSave > 5000 && video.duration > 30) {
      lastPosSave = now;
      if (t > 5 && t < video.duration - 15) data.positions[current.id] = Math.floor(t);
      else if (t >= video.duration - 15) delete data.positions[current.id];
      persist();
    }
  });

  video.addEventListener('progress', paint);
  video.addEventListener('waiting', () => spinner.classList.remove('hidden'));
  video.addEventListener('playing', () => spinner.classList.add('hidden'));
  video.addEventListener('canplay', () => spinner.classList.add('hidden'));
  video.addEventListener('play', () => {
    playBtn.replaceChildren(icon('pause', 20));
    playBtn.title = 'Pause (Space)';
    stage.classList.remove('paused');
    pokeControls();
  });
  video.addEventListener('pause', () => {
    playBtn.replaceChildren(icon('play', 20));
    playBtn.title = 'Play (Space)';
    stage.classList.add('paused');
    stage.classList.remove('idle');
  });

  const SPEEDS = [0.25, 0.5, 0.75, 1, 1.25, 1.5, 1.75, 2];
  speedBtn.onclick = (e) => {
    e.stopPropagation();
    const r = speedBtn.getBoundingClientRect();
    menu(r.left, r.top - 8, SPEEDS.map((s) => ({
      label: `${s}×`,
      icon: (Number(data.speed) || 1) === s ? 'check' : null,
      action: () => {
        data.speed = s;
        video.playbackRate = s;
        speedBtn.textContent = `${s}×`;
        persist();
      },
    })));
  };

  qualBtn.onclick = (e) => {
    e.stopPropagation();
    const r = qualBtn.getBoundingClientRect();
    const items = [
      {
        label: 'Auto',
        icon: !selectedHeight ? 'check' : null,
        action: () => {
          selectedHeight = '';
          qualBtn.textContent = 'Auto';
          if (mode === 'video') setMode('video');
        },
      },
      ...currentHeights.map((ht) => ({
        label: `${ht}p`,
        icon: selectedHeight === String(ht) ? 'check' : null,
        action: () => {
          selectedHeight = String(ht);
          qualBtn.textContent = `${ht}p`;
          if (mode === 'video') setMode('video');
        },
      })),
    ];
    menu(r.left, r.top - 8, items);
  };

  pipBtn.onclick = async (e) => {
    e.stopPropagation();
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
    sbBtn.classList.toggle('active', !!data.sponsorblock);
    sbBtn.title = `Skip sponsor segments (${data.sponsorblock ? 'ON' : 'OFF'})`;
  }
  sbBtn.onclick = () => {
    data.sponsorblock = !data.sponsorblock;
    if (!data.sponsorblock) segments = [];
    persist();
    refreshSbBtn();
    renderSegments();
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

  win.body.addEventListener('keydown', (e) => {
    if (e.target.closest('input, textarea, select')) return;
    if (watchView.classList.contains('hidden')) return;
    const k = e.key.toLowerCase();
    if (k === ' ' || k === 'k') { e.preventDefault(); togglePlay(); }
    else if (k === 'arrowleft' || k === 'j') { e.preventDefault(); video.currentTime = Math.max(0, video.currentTime - 5); }
    else if (k === 'arrowright' || k === 'l') { e.preventDefault(); video.currentTime = Math.min(video.duration || 0, video.currentTime + 5); }
    else if (k === 'arrowup') { e.preventDefault(); video.volume = Math.min(1, video.volume + 0.1); video.muted = false; showVol(); }
    else if (k === 'arrowdown') { e.preventDefault(); video.volume = Math.max(0, video.volume - 0.1); showVol(); }
    else if (k === 'm') { e.preventDefault(); video.muted = !video.muted; showVol(); }
    else if (k === 'f') { e.preventDefault(); fullscreen(); }
    else if (k === 'p') { e.preventDefault(); playStep(-1); }
    else if (k === 'n') { e.preventDefault(); playStep(1); }
  });

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

  const thumbURL = (r) => `/api/youtube/thumb?id=${encodeURIComponent(r.thumb || r.id)}`;

  function card(r, list) {
    if (r.kind === 'playlist') return playlistCard(r);
    const save = h('button', {
      type: 'button', class: 'tube-save', title: 'Save to playlist', 'aria-label': 'Save to playlist',
      onclick: (e) => { e.stopPropagation(); openSaveMenu(r, e.currentTarget); },
    }, icon('bookmark', 15));
    return h('div', { class: 'tube-card' },
      h('button', { type: 'button', class: 'tube-thumbbtn', onclick: () => play(r, list) },
        h('img', { src: thumbURL(r), alt: '', loading: 'lazy' }),
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

  function playlistCard(r) {
    return h('div', { class: 'tube-card' },
      h('button', { type: 'button', class: 'tube-thumbbtn', onclick: () => openSearchPlaylist(r) },
        h('img', { src: thumbURL(r), alt: '', loading: 'lazy' }),
        h('span', { class: 'tube-dur tube-playlist-badge' }, icon('listVideo', 12),
          r.count ? ` Playlist · ${r.count}` : ' Playlist')),
      h('div', { class: 'tube-card-meta' },
        h('span', { class: 'tube-avatar' }, (r.channel || '?').trim().charAt(0).toUpperCase() || '?'),
        h('div', { class: 'tube-card-text' },
          h('button', { type: 'button', class: 'tube-card-title', onclick: () => openSearchPlaylist(r) }, r.title),
          r.channel ? h('span', { class: 'set-desc' }, r.channel) : null,
          h('span', { class: 'set-desc' }, 'Playlist'))));
  }

  function shortCard(r, list) {
    return h('div', { class: 'tube-short-card' },
      h('button', { type: 'button', class: 'tube-short-thumb', onclick: () => play(r, list) },
        h('img', { src: thumbURL(r), alt: '', loading: 'lazy' }),
        h('span', { class: 'tube-dur tube-shorts-badge' }, 'Shorts')),
      h('button', { type: 'button', class: 'tube-short-title', onclick: () => play(r, list) }, r.title),
      r.views ? h('span', { class: 'set-desc' }, r.views) : null);
  }

  function renderGrid(list, emptyMsg) {
    grid.replaceChildren();
    shortsShelf.replaceChildren();
    shortsShelf.classList.add('hidden');
    const shorts = list.filter((r) => r.kind === 'short');
    const rest = list.filter((r) => r.kind !== 'short');
    if (!list.length) {
      status.textContent = emptyMsg || 'Nothing here yet.';
      return;
    }
    status.textContent = '';
    if (shorts.length) {
      shortsShelf.classList.remove('hidden');
      shortsShelf.append(h('h3', { class: 'tube-shorts-title' }, icon('flame', 15), ' Shorts'),
        h('div', { class: 'tube-shorts-row' }, ...shorts.map((r) => shortCard(r, shorts))));
    }
    grid.append(...rest.map((r) => card(r, rest)));
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
    shortsShelf.replaceChildren();
    shortsShelf.classList.add('hidden');
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

  // A playlist found in search: show it right away, with a Save button so it
  // only joins the library when asked (opening and saving are one tap apart).
  async function openSearchPlaylist(r) {
    const saved = data.playlists.find((x) => x.yt === r.id);
    if (saved) { openPlaylist(saved.id); return; }
    view = 'playlist';
    beginLoad(r.title || 'Playlist');
    try {
      const info = await api('GET', `/api/youtube/playlist?id=${encodeURIComponent(r.id)}`);
      if (view !== 'playlist') return;
      const temp = { id: '', name: info.title || r.title || 'Playlist', yt: info.id, videos: (info.results || []).map(clean) };
      if (info.title) heading.textContent = info.title;
      chips.replaceChildren(
        h('button', { class: 'btn small', type: 'button', onclick: showPlaylists }, icon('chevronLeft', 14), 'Playlists'),
        h('button', {
          class: 'btn small primary', type: 'button', disabled: !temp.videos.length,
          onclick: () => { if (temp.videos.length) play(temp.videos[0], temp.videos); },
        }, icon('play', 13), 'Play all'),
        h('button', {
          class: 'btn small', type: 'button',
          onclick: (e) => {
            const p = { id: `p${Date.now().toString(36)}`, ...temp };
            data.playlists = [...data.playlists, p];
            persist();
            e.currentTarget.textContent = 'Saved ✓';
            e.currentTarget.disabled = true;
            toast('Playlist saved.');
          },
        }, icon('bookmark', 13), 'Save playlist'),
        ...(info.count ? [h('span', { class: 'set-desc' }, info.count)] : []));
      renderGrid(temp.videos, 'This playlist has no playable videos.');
    } catch (err) {
      status.textContent = err.message || 'Could not open that playlist.';
    }
  }

  function openPlaylist(pid) {
    const p = data.playlists.find((x) => x.id === pid);
    if (!p) { showPlaylists(); return; }
    view = 'playlist';
    beginLoad(p.name);
    const renderButtons = () => {
      chips.replaceChildren(
        h('button', { class: 'btn small', type: 'button', onclick: showPlaylists }, icon('chevronLeft', 14), 'All playlists'),
        h('button', {
          class: 'btn small primary', type: 'button', disabled: !p.videos.length,
          onclick: () => { if (p.videos.length) play(p.videos[0], p.videos); },
        }, icon('play', 13), 'Play all'),
        ...(p.yt ? [h('button', {
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
        }, icon('history', 13), 'Refresh')] : []));
    };
    renderButtons();
    if (p.yt && (!p.videos || !p.videos.length)) {
      status.textContent = 'Loading playlist…';
      api('GET', `/api/youtube/playlist?id=${encodeURIComponent(p.yt)}`).then((info) => {
        if (view !== 'playlist') return;
        p.videos = (info.results || []).map(clean);
        if (info.title) { p.name = info.title; heading.textContent = info.title; }
        persist();
        renderButtons();
        renderGrid(p.videos, 'This playlist is empty.');
      }).catch((err) => {
        if (view !== 'playlist') return;
        status.textContent = err.message || 'Could not load playlist.';
      });
      return;
    }
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
      renderSearchFilters((res.results || []).map(clean));
    } catch (err) {
      status.textContent = err.message || 'Search failed.';
      if (err) report(err);
    }
  }

  // Search results mix videos, Shorts and playlists; these chips slice them.
  function renderSearchFilters(list) {
    const groups = [
      ['All', list],
      ['Videos', list.filter((r) => r.kind === 'video')],
      ['Shorts', list.filter((r) => r.kind === 'short')],
      ['Playlists', list.filter((r) => r.kind === 'playlist')],
    ].filter(([, items]) => items.length);
    if (groups.length <= 2) { renderGrid(list, 'Nothing found.'); return; }
    const draw = (items, btn) => {
      chips.querySelectorAll('.tube-chip').forEach((c) => c.classList.remove('active'));
      btn?.classList.add('active');
      renderGrid(items, 'Nothing in this slice.');
    };
    chips.replaceChildren(...groups.map(([label, items], i) => {
      const b = h('button', {
        type: 'button', class: `tube-chip${i === 0 ? ' active' : ''}`,
        onclick: () => draw(items, b),
      }, `${label} · ${items.length}`);
      return b;
    }));
    renderGrid(list, 'Nothing found.');
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
    speedBtn.textContent = `${Number(data.speed) || 1}×`;
    refreshSbBtn();
    showHome();
  });
  setTimeout(() => input.focus(), 50);

  return {
    onDestroy() {
      clearTimeout(saveTimer);
      clearTimeout(hideTimer);
      try {
        video.pause();
        if (document.pictureInPictureElement === video) {
          document.exitPictureInPicture().catch(() => {});
        }
        video.removeAttribute('src');
        video.load();
      } catch { /* already gone */ }
    },
    saveState: () => ({}),
  };
}
