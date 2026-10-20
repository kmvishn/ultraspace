// Videos: a VLC-style player for video files in Files (or on this device), torrents while
// they download, and Stremio streams. The server picks the cheapest way your browser can
// play each one: as it is, repackaged, with the audio converted, or fully converted.
// Positions are remembered on the server.
import { h, icon, api, report, toast, notice, menu, fileUrl, thumbUrl, baseOf, dirOf, fmtClock, debounce } from '../core.js';
import { registerApp, openApp } from '../wm.js';
import { device } from '../session.js';
import { pickFiles } from '../pick.js';

registerApp({
  id: 'video', name: 'Videos', icon: 'film', color: ['#fb923c', '#c2410c'], width: 960, height: 600, minWidth: 380, minHeight: 240,
  mount(win, args) { return mountVideo(win, args); },
});

export const VIDEO_EXTS = ['mp4', 'm4v', 'mov', 'webm', 'mkv', 'avi', 'wmv', 'flv', 'mpg', 'mpeg', 'ts', 'm2ts', 'mts', '3gp', 'ogv', 'vob', 'divx', 'asf'];
const SPEEDS = [0.5, 0.75, 1, 1.25, 1.5, 1.75, 2];
const QUALITIES = [['', 'Original'], ['1080', 'Convert to 1080p'], ['720', 'Convert to 720p'], ['480', 'Convert to 480p']];
const MODE_LABEL = { direct: 'Direct', remux: 'Repackaged', transcode: 'Converting', local: 'This device' };

// Codecs this browser can decode, so the server can pass them through.
function browserCaps() {
  const MS = window.MediaSource || window.ManagedMediaSource;
  const ok = (t) => (MS?.isTypeSupported?.(t)) || document.createElement('video').canPlayType(t) === 'probably';
  return [ok('video/mp4; codecs="hvc1.1.6.L93.B0"') && 'hevc', ok('video/mp4; codecs="av01.0.05M.08"') && 'av1'].filter(Boolean).join(',');
}

// ---------- resume positions (kept on the server) ----------

const positions = {
  data: null,
  async load() {
    if (this.data) return this.data;
    try { this.data = (await api('GET', '/api/state/media')).data?.positions || {}; } catch { this.data = {}; }
    return this.data;
  },
  set(p, t, d, extra) {
    if (!this.data || !p || !d) return;
    if (t < 10 || t > d - 20) delete this.data[p]; else this.data[p] = { ...extra, t: Math.floor(t), d: Math.floor(d), at: Date.now() };
    const keys = Object.keys(this.data).sort((a, b) => this.data[b].at - this.data[a].at);
    for (const k of keys.slice(200)) delete this.data[k];
    this.save();
  },
  save: debounce(() => {
    api('PUT', '/api/state/media', { device: device.id, device_name: device.name, data: { positions: positions.data } }).catch(() => {});
  }, 1500),
};

// ---------- WebVTT/SRT cues for the subtitle overlay ----------

function parseCues(text) {
  const ts = (s) => { const m = s.trim().replace(',', '.').match(/(?:(\d+):)?(\d+):(\d+)\.(\d+)/); return m ? (+(m[1] || 0)) * 3600 + +m[2] * 60 + +m[3] + +`0.${m[4]}` : NaN; };
  const cues = [];
  for (const block of text.replace(/\r/g, '').split(/\n\n+/)) {
    const lines = block.split('\n');
    const i = lines.findIndex((l) => l.includes('-->'));
    if (i < 0) continue;
    const [a, b] = lines[i].split('-->');
    const start = ts(a), end = ts(b.trim().split(/\s/)[0]);
    const body = lines.slice(i + 1).join('\n').replace(/<[^>]+>/g, '').replace(/\{\\[^}]*\}/g, '').trim();
    if (body && Number.isFinite(start) && Number.isFinite(end)) cues.push({ start, end, text: body });
  }
  return cues.sort((x, y) => x.start - y.start);
}

function mountVideo(win, args) {
  let item = null;      // { path } (Files), { src, key, poster } (torrent/Stremio) or { local, url }; all have name
  let info = null;      // probe result
  let mode = 'direct';
  let offset = 0;       // stream modes start at this second of the file
  let quality = '';
  let audioN = 0;
  let cues = [];
  let siblings = [];
  let wantTime = null;
  let hideTimer = null;
  let saveTimer = null;

  const video = h('video', { class: 'vp-video', playsinline: true, preload: 'metadata' });
  try { video.volume = Number(localStorage.getItem('us.video.volume') ?? 1); } catch { /* default */ }
  const subs = h('div', { class: 'vp-subs' });
  const spinNote = h('div', { class: 'vp-spin-note' });
  const spinner = h('div', { class: 'vp-spinner hidden' }, h('span', { class: 'bw-spin big' }), spinNote);
  const badge = h('span', { class: 'vp-badge hidden' });
  const flash = h('div', { class: 'vp-flash' });

  const played = h('div', { class: 'vp-played' });
  const buffered = h('div', { class: 'vp-buffered' });
  const hoverTime = h('div', { class: 'vp-hover hidden' });
  const seekbar = h('div', { class: 'vp-seek', role: 'slider', 'aria-label': 'Seek', tabindex: '0' }, h('div', { class: 'vp-track' }, buffered, played), hoverTime);
  const timeEl = h('span', { class: 'vp-time' }, '0:00 / 0:00');
  const btn = (ic, label, fn, cls = '') => h('button', { class: `vp-btn ${cls}`, title: label, 'aria-label': label, onclick: (e) => { e.stopPropagation(); fn(e); } }, icon(ic, 18));
  const playBtn = btn('play', 'Play (Space)', () => toggle());
  const prevBtn = btn('skipBack', 'Previous (P)', () => step(-1));
  const nextBtn = btn('skipForward', 'Next (N)', () => step(1));
  const volBtn = btn('volume', 'Mute (M)', () => { video.muted = !video.muted; showVol(); });
  const vol = h('input', { type: 'range', class: 'range vp-vol', min: '0', max: '100', value: String(Math.round(video.volume * 100)), 'aria-label': 'Volume' });
  const speedBtn = h('button', { class: 'vp-btn vp-text', title: 'Speed', onclick: (e) => { e.stopPropagation(); speedMenu(e); } }, '1×');
  const audioBtn = btn('music', 'Audio track', (e) => audioMenu(e), 'hidden');
  const subBtn = btn('fileText', 'Subtitles (C)', (e) => subMenu(e));
  const qualBtn = btn('sliders', 'Quality', (e) => qualityMenu(e), 'hidden');
  const pipBtn = btn('layers', 'Picture in picture', () => pip());
  const fsBtn = btn('maximize', 'Full screen (F)', () => fullscreen());
  const controls = h('div', { class: 'vp-controls' }, seekbar,
    h('div', { class: 'vp-row' }, prevBtn, playBtn, nextBtn, volBtn, vol, timeEl, h('span', { class: 'tb-spacer' }), badge, speedBtn, audioBtn, subBtn, qualBtn, pipBtn, fsBtn));
  const empty = h('div', { class: 'vp-empty' });
  const stage = h('div', { class: 'vp-stage', tabindex: '0' }, video, subs, flash, spinner, controls, empty);
  const localInput = h('input', { type: 'file', accept: 'video/*,.mkv,.avi,.mov,.wmv,.flv', class: 'hidden', onchange: () => { if (localInput.files[0]) openLocal(localInput.files[0]); localInput.value = ''; } });
  const subInput = h('input', { type: 'file', accept: '.srt,.vtt,.ass,.ssa,text/vtt', class: 'hidden', onchange: async () => { const f = subInput.files[0]; subInput.value = ''; if (f) { cues = parseCues(await f.text()); toast(cues.length ? `Subtitles: ${f.name}` : 'No subtitles found in that file', cues.length ? '' : 'error'); } } });
  win.body.append(h('div', { class: 'app app-video' }, stage, localInput, subInput));

  // ---------- opening ----------

  const isStream = () => mode === 'remux' || mode === 'transcode';
  const now = () => (isStream() ? offset : 0) + (video.currentTime || 0);
  const duration = () => (info?.duration || (Number.isFinite(video.duration) ? video.duration : 0)) || 0;

  // what the server endpoints need to find the current item
  const where = () => (item.src ? `src=${encodeURIComponent(item.src)}` : `path=${encodeURIComponent(item.path)}`);
  const posKey = () => item?.key || item?.path;

  function open(path, list) {
    return load({ path, name: baseOf(path) }, list?.length ? list : [path]);
  }

  // openSrc plays a media source: { src, name, key, poster, subtitles: [{ lang, url }], meta }.
  // meta ({ id, type, name, poster, video }) comes from Stremio; progress is reported to the
  // signed-in Stremio account so continue watching works in the official apps too.
  function openSrc(a) {
    return load({ src: a.src, name: a.name || 'Video', key: a.key || a.src, poster: a.poster || '', extraSubs: a.subtitles || [], meta: a.meta || null }, []);
  }

  let lastSync = 0;
  let stremioAccount = null; // checked once per window
  async function syncStremio(force) {
    const m = item?.meta;
    if (!m || !duration() || (!force && Date.now() - lastSync < 60_000)) return;
    lastSync = Date.now();
    const t = now(), d = duration();
    stremioAccount ||= api('GET', '/api/stremio/account').then((a) => a.signedIn).catch(() => false);
    if (!(await stremioAccount)) return;
    api('POST', '/api/stremio/account/library', { id: m.id, type: m.type, name: m.name, poster: m.poster, video: m.video, t, d }).catch(() => {});
  }

  async function load(next, list) {
    savePosition();
    item = next;
    const path = item.path;
    siblings = list;
    cues = [];
    audioN = 0;
    quality = '';
    info = null;
    win.setTitle(item.name);
    empty.classList.add('hidden');
    spinner.classList.remove('hidden');
    spinNote.textContent = item.src?.startsWith('torrent:') ? 'Finding peers and the first pieces…' : item.src ? 'Connecting to the stream…' : '';
    navState();
    const mine = item;
    const [res] = await Promise.all([
      api('GET', `/api/files/probe?${where()}&caps=${browserCaps()}`).catch((err) => ({ error: err.message })),
      positions.load(),
    ]);
    if (item !== mine) return;
    if (res.error) { spinner.classList.add('hidden'); toast(`${item.name}: ${res.error}`, 'error'); showEmpty(); return; }
    info = res;
    if (item.src && res.name && item.name === 'Video') { item.name = res.name; win.setTitle(item.name); }
    for (const s of item.extraSubs || []) {
      info.subtitles = [...(info.subtitles || []), { kind: 'src', src: `url:${s.url}`, lang: s.lang, title: s.lang ? `${s.lang} (addon)` : 'Addon subtitles' }];
    }
    audioBtn.classList.toggle('hidden', (info.audio || []).length < 2);
    qualBtn.classList.toggle('hidden', !info.ffmpeg);
    const saved = positions.data[posKey()];
    wantTime = saved && saved.t > 10 ? saved.t : 0;
    start(info.plan.mode, wantTime);
    if (wantTime) {
      const close = notice(`Resumed at ${fmtClock(wantTime)}`, [{ label: 'Start over', run: () => seekTo(0) }, { label: 'OK', primary: true }], 'history');
      setTimeout(close, 6000);
    }
    if ('mediaSession' in navigator) navigator.mediaSession.metadata = new MediaMetadata({ title: item.name, artist: (path && dirOf(path).split('/').pop()) || 'Videos' });
  }

  function openLocal(file) {
    savePosition();
    item = { local: true, name: file.name, url: URL.createObjectURL(file) };
    siblings = [];
    info = null;
    cues = [];
    win.setTitle(file.name);
    empty.classList.add('hidden');
    audioBtn.classList.add('hidden');
    qualBtn.classList.add('hidden');
    mode = 'local';
    offset = 0;
    video.src = item.url;
    video.play().catch(() => {});
    navState();
    setBadge();
  }

  // start loads the file in the given mode at second t.
  function start(m, t = 0) {
    mode = quality ? 'transcode' : m;
    spinner.classList.remove('hidden');
    if (mode === 'direct') {
      offset = 0;
      video.src = item.src ? `/api/media/raw?src=${encodeURIComponent(item.src)}` : fileUrl(item.path, true);
      wantTime = t;
    } else {
      offset = Math.max(0, t);
      const q = new URLSearchParams({ ...(item.src ? { src: item.src } : { path: item.path }), t: offset.toFixed(2), audio: audioN, caps: browserCaps() });
      if (quality) { q.set('mode', 'transcode'); q.set('h', quality); }
      video.src = `/api/files/stream?${q}`;
      wantTime = null;
    }
    video.play().catch(() => {});
    setBadge();
  }

  function setBadge() {
    const label = mode === 'transcode' && quality ? `Converting ${quality}p` : mode === 'transcode' ? 'Converting 720p' : MODE_LABEL[mode];
    badge.textContent = label || '';
    badge.title = info?.plan?.reason || '';
    badge.classList.toggle('hidden', !label);
    badge.classList.toggle('warn', mode === 'transcode');
  }

  function seekTo(t) {
    const d = duration();
    t = Math.max(0, d ? Math.min(t, d - 0.5) : t);
    if (mode === 'direct' || mode === 'local') { video.currentTime = t; return; }
    // Converted streams restart from the new point.
    const playing = !video.paused;
    start(mode, t);
    if (!playing) video.pause();
  }

  // ---------- controls ----------

  function toggle() {
    if (!item) { showEmpty(); return; }
    video.paused ? video.play().catch(report) : video.pause();
  }

  function step(d) {
    if (!item || item.local || siblings.length < 2) return;
    const i = siblings.indexOf(item.path);
    const next = siblings[(i + d + siblings.length) % siblings.length];
    if (next) open(next, siblings);
  }

  function navState() {
    const many = siblings.length > 1;
    prevBtn.classList.toggle('hidden', !many);
    nextBtn.classList.toggle('hidden', !many);
  }

  function showVol() {
    volBtn.replaceChildren(icon(video.muted || video.volume === 0 ? 'volumeX' : 'volume', 18));
    vol.value = String(Math.round((video.muted ? 0 : video.volume) * 100));
    vol.style.setProperty('--fill', `${vol.value}%`);
  }

  function showFlash(ic, text) {
    flash.replaceChildren(icon(ic, 26), text ? h('span', null, text) : null);
    flash.classList.remove('on');
    void flash.offsetWidth;
    flash.classList.add('on');
  }

  function speedMenu(e) {
    const r = e.currentTarget.getBoundingClientRect();
    menu(r.left, r.top - 8, SPEEDS.map((s) => ({ label: `${s}×`, icon: video.playbackRate === s ? 'check' : null, action: () => setSpeed(s) })));
  }
  function setSpeed(s) {
    video.playbackRate = s;
    speedBtn.textContent = `${s}×`;
    showFlash('play', `${s}×`);
  }

  function audioMenu(e) {
    const r = e.currentTarget.getBoundingClientRect();
    menu(r.left, r.top - 8, (info?.audio || []).map((a, i) => ({
      label: [a.title || a.lang?.toUpperCase() || `Track ${i + 1}`, a.codec?.toUpperCase(), a.channels > 2 ? `${a.channels}ch` : ''].filter(Boolean).join(' · '),
      icon: i === audioN ? 'check' : null,
      action: async () => {
        if (i === audioN) return;
        const t = now();
        audioN = i;
        const res = await api('GET', `/api/files/probe?${where()}&audio=${i}&caps=${browserCaps()}`).catch(report);
        if (res) { info.plan = res.plan; start(res.plan.mode === 'direct' ? 'remux' : res.plan.mode, t); }
      },
    })));
  }

  function subMenu(e) {
    const r = e.currentTarget.getBoundingClientRect();
    const tracks = item?.local ? [] : info?.subtitles || [];
    menu(r.left, r.top - 8, [
      { label: 'Off', icon: cues.length ? null : 'check', action: () => { cues = []; subs.replaceChildren(); } },
      ...tracks.map((s) => ({ label: s.title || s.lang || 'Subtitles', icon: 'fileText', action: () => loadSubs(s) })),
      '-',
      { label: 'Load from this device…', icon: 'upload', action: () => subInput.click() },
    ]);
  }

  async function loadSubs(s) {
    const url = s.kind === 'file' ? `/api/files/subtitle?path=${encodeURIComponent(s.path)}`
      : s.kind === 'src' ? `/api/files/subtitle?src=${encodeURIComponent(s.src)}`
        : `/api/files/subtitle?${where()}&stream=${s.index}`;
    try {
      const res = await fetch(url);
      if (!res.ok) throw new Error((await res.json().catch(() => ({}))).error || res.statusText);
      cues = parseCues(await res.text());
      toast(cues.length ? `Subtitles: ${s.title || s.lang}` : 'That track has no subtitles', cues.length ? '' : 'error');
    } catch (err) { report(err); }
  }

  function cycleSubs() {
    const tracks = info?.subtitles || [];
    if (!tracks.length) { if (cues.length) { cues = []; subs.replaceChildren(); } return; }
    if (cues.length) { cues = []; subs.replaceChildren(); showFlash('fileText', 'Subtitles off'); return; }
    loadSubs(tracks[0]);
  }

  function qualityMenu(e) {
    const r = e.currentTarget.getBoundingClientRect();
    menu(r.left - 120, r.top - 8, QUALITIES.map(([q, label]) => ({
      label: q ? label : `Original (${MODE_LABEL[info?.plan?.mode] || 'Direct'})`, icon: quality === q ? 'check' : null,
      hint: q ? 'uses server CPU' : '',
      action: () => { if (quality === q) return; const t = now(); quality = q; start(info.plan.mode, t); },
    })));
  }

  async function pip() {
    try {
      if (document.pictureInPictureElement) await document.exitPictureInPicture();
      else await video.requestPictureInPicture();
    } catch { toast('Picture in picture is not available here', 'error'); }
  }

  function fullscreen() {
    if (document.fullscreenElement) { document.exitFullscreen(); return; }
    if (stage.requestFullscreen) stage.requestFullscreen().catch(() => video.webkitEnterFullscreen?.());
    else video.webkitEnterFullscreen?.(); // iPhone
  }

  // ---------- seek bar ----------

  const pct = (t) => `${duration() ? Math.min(100, (100 * t) / duration()) : 0}%`;
  function paint() {
    const t = now();
    played.style.width = pct(t);
    let end = 0;
    for (let i = 0; i < video.buffered.length; i++) if (video.buffered.start(i) <= video.currentTime + 1) end = Math.max(end, video.buffered.end(i));
    buffered.style.width = pct((isStream() ? offset : 0) + end);
    timeEl.textContent = `${fmtClock(t)} / ${fmtClock(duration())}`;
    // subtitles
    const cue = cues.find((c) => c.start <= t && t < c.end);
    const text = cue?.text || '';
    if (subs.dataset.text !== text) {
      subs.dataset.text = text;
      subs.replaceChildren(...(text ? text.split('\n').map((l) => h('span', null, l)) : []));
    }
  }

  const atPointer = (e) => { const r = seekbar.getBoundingClientRect(); return Math.max(0, Math.min(1, (e.clientX - r.left) / r.width)) * duration(); };
  seekbar.addEventListener('pointermove', (e) => {
    const t = atPointer(e);
    hoverTime.textContent = fmtClock(t);
    hoverTime.style.left = `${(100 * t) / (duration() || 1)}%`;
    hoverTime.classList.remove('hidden');
  });
  seekbar.addEventListener('pointerleave', () => hoverTime.classList.add('hidden'));
  seekbar.addEventListener('pointerdown', (e) => {
    e.preventDefault();
    seekbar.setPointerCapture(e.pointerId);
    const move = (ev) => { played.style.width = pct(atPointer(ev)); };
    const up = (ev) => { seekbar.removeEventListener('pointermove', move); seekbar.removeEventListener('pointerup', up); seekTo(atPointer(ev)); };
    seekbar.addEventListener('pointermove', move);
    seekbar.addEventListener('pointerup', up);
  });

  // ---------- video events ----------

  video.addEventListener('loadedmetadata', () => {
    if (wantTime) { video.currentTime = wantTime; wantTime = null; }
  });
  video.addEventListener('playing', () => { spinner.classList.add('hidden'); spinNote.textContent = ''; });
  video.addEventListener('waiting', () => spinner.classList.remove('hidden'));
  video.addEventListener('canplay', () => spinner.classList.add('hidden'));
  video.addEventListener('timeupdate', paint);
  video.addEventListener('progress', paint);
  video.addEventListener('play', () => { playBtn.replaceChildren(icon('pause', 18)); stage.classList.remove('paused'); poke(); });
  video.addEventListener('pause', () => { playBtn.replaceChildren(icon('play', 18)); stage.classList.add('paused'); stage.classList.remove('idle'); savePosition(); });
  video.addEventListener('ended', () => {
    if (item && !item.local) { syncStremio(true); positions.set(posKey(), 0, duration()); }
    if (siblings.length > 1 && siblings.indexOf(item?.path) < siblings.length - 1) step(1);
  });
  video.addEventListener('error', () => {
    spinner.classList.add('hidden');
    if (!item || !video.getAttribute('src')) return;
    // A file the browser cannot play as it is: let the server convert it.
    if (mode === 'direct' && info?.ffmpeg) { toast('This browser cannot play the file as it is; converting it on the server'); quality = '720'; start('transcode', now()); return; }
    if (mode === 'local') toast('This browser cannot play that file. Put it in Files and the server will convert it.', 'error');
    else toast(`Cannot play ${item.name}`, 'error');
  });
  vol.addEventListener('input', () => {
    video.volume = vol.value / 100;
    video.muted = false;
    try { localStorage.setItem('us.video.volume', String(video.volume)); } catch { /* ignore */ }
    showVol();
  });

  function savePosition() {
    if (!item || item.local || !duration()) return;
    positions.set(posKey(), now(), duration(), item.src ? { src: item.src, name: item.name, poster: item.poster, meta: item.meta || undefined } : undefined);
    syncStremio(video.paused);
  }
  saveTimer = setInterval(() => { if (!video.paused) savePosition(); }, 10_000);

  // Controls fade out while playing and the pointer is still.
  function poke() {
    stage.classList.remove('idle');
    clearTimeout(hideTimer);
    hideTimer = setTimeout(() => { if (!video.paused && !controls.matches(':hover')) stage.classList.add('idle'); }, 2600);
  }
  stage.addEventListener('pointermove', poke);
  stage.addEventListener('click', (e) => {
    if (e.target.closest('.vp-controls, .vp-empty')) return;
    if (matchMedia('(pointer: coarse)').matches && stage.classList.contains('idle')) { poke(); return; } // first tap shows the controls
    toggle();
    showFlash(video.paused ? 'pause' : 'play');
  });
  stage.addEventListener('dblclick', (e) => { if (!e.target.closest('.vp-controls')) fullscreen(); });

  win.body.addEventListener('keydown', (e) => {
    if (e.target.closest('input[type=text], input[type=search], textarea') || !item) return;
    const k = e.key.toLowerCase();
    const act = {
      ' ': () => toggle(), k: () => toggle(),
      arrowleft: () => { seekTo(now() - (e.shiftKey ? 60 : 10)); showFlash('skipBack', '-' + (e.shiftKey ? 60 : 10) + 's'); },
      arrowright: () => { seekTo(now() + (e.shiftKey ? 60 : 10)); showFlash('skipForward', '+' + (e.shiftKey ? 60 : 10) + 's'); },
      j: () => seekTo(now() - 10), l: () => seekTo(now() + 10),
      arrowup: () => { video.muted = false; video.volume = Math.min(1, video.volume + 0.05); showVol(); showFlash('volume', `${Math.round(video.volume * 100)}%`); },
      arrowdown: () => { video.volume = Math.max(0, video.volume - 0.05); showVol(); showFlash('volume', `${Math.round(video.volume * 100)}%`); },
      m: () => { video.muted = !video.muted; showVol(); },
      f: () => fullscreen(), n: () => step(1), p: () => step(-1), c: () => cycleSubs(),
      ']': () => setSpeed(SPEEDS[Math.min(SPEEDS.length - 1, SPEEDS.indexOf(video.playbackRate) + 1)] || 1),
      '[': () => setSpeed(SPEEDS[Math.max(0, SPEEDS.indexOf(video.playbackRate) - 1)] || 1),
    }[k];
    if (act) { e.preventDefault(); act(); poke(); }
  });

  if ('mediaSession' in navigator) {
    navigator.mediaSession.setActionHandler('seekbackward', () => seekTo(now() - 10));
    navigator.mediaSession.setActionHandler('seekforward', () => seekTo(now() + 10));
  }

  // ---------- empty state: open something, or continue watching ----------

  async function showEmpty() {
    empty.classList.remove('hidden');
    const list = Object.entries(await positions.load()).sort((a, b) => b[1].at - a[1].at).slice(0, 8);
    empty.replaceChildren(h('div', { class: 'vp-empty-inner' },
      h('span', { class: 'vp-empty-icon' }, icon('film', 34)),
      h('h2', null, 'Play a video'),
      h('p', { class: 'muted' }, 'MP4, MKV, AVI, MOV, WebM, WMV and more. Files your browser cannot play are converted on the server.'),
      h('div', { class: 'hero-actions' },
        h('button', { class: 'btn primary', onclick: async () => { const p = await pickFiles({ title: 'Choose a video' }); if (p?.[0]) open(p[0]); } }, icon('folder', 15), 'From Files'),
        h('button', { class: 'btn', onclick: () => localInput.click() }, icon('upload', 15), 'From this device')),
      list.length ? h('div', { class: 'vp-continue' }, h('div', { class: 'bw-section-title' }, icon('history', 14), 'Continue watching'),
        list.map(([p, pos]) => h('button', { class: 'vp-cont-item', onclick: () => (pos.src ? openSrc({ ...pos, key: p }) : open(p)) },
          h('img', {
            src: pos.src ? (pos.poster ? `/api/stremio/img?u=${encodeURIComponent(pos.poster)}` : '') : thumbUrl(p, 256), alt: '', loading: 'lazy',
            onerror: (e) => e.target.replaceWith(h('span', { class: 'vp-cont-ph' }, icon('film', 18))),
          }),
          h('span', { class: 'vp-cont-text' }, h('span', { class: 'vp-cont-name' }, pos.name || baseOf(p)),
            h('span', { class: 'vp-cont-bar' }, h('i', { style: { width: `${(100 * pos.t) / pos.d}%` } })),
            h('span', { class: 'muted small' }, `${fmtClock(pos.t)} of ${fmtClock(pos.d)}`))))) : null));
  }

  showVol();
  if (args.path) open(args.path, args.siblings);
  else if (args.src) openSrc(args);
  else if (args.restore?.path) open(args.restore.path, args.restore.siblings);
  else if (args.restore?.src) openSrc(args.restore);
  else showEmpty();

  return {
    onArgs(a) { if (a.path) open(a.path, a.siblings); else if (a.src) openSrc(a); },
    onFocus() { stage.focus({ preventScroll: true }); },
    saveState: () => (!item || item.local ? {}
      : item.src ? { src: item.src, name: item.name, key: item.key, poster: item.poster, meta: item.meta }
        : { path: item.path, siblings: siblings.length <= 200 ? siblings : [item.path] }),
    onDestroy() {
      savePosition();
      clearInterval(saveTimer);
      clearTimeout(hideTimer);
      video.pause();
      video.removeAttribute('src');
      video.load();
      if (item?.local) URL.revokeObjectURL(item.url);
    },
  };
}
