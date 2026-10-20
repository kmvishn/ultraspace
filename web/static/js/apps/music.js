// Music: plays audio from Files (folders become albums) and from a connected Navidrome
// server, through one player. The queue and position follow you between devices.
import { h, icon, api, report, toast, emit, fmtClock, fmtBytes, fmtDuration, extOf, dirOf, baseOf, fileUrl, debounce } from '../core.js';
import { registerApp } from '../wm.js';
import { nd } from './music-navidrome.js';

registerApp({
  id: 'music', name: 'Music', icon: 'music', color: ['#fb7185', '#e11d48'], width: 1080, height: 700, minWidth: 520,
  mount(win, args) { return mountMusic(win, args); },
});

const COVER_NAMES = ['cover', 'folder', 'front', 'album', 'albumart'];

const ND_SORTS = [
  ['newest', 'Recently added'], ['alphabeticalByName', 'A–Z'], ['frequent', 'Most played'],
  ['recent', 'Recently played'], ['starred', 'Favourites'], ['random', 'Random'],
];

function parseTrack(f) {
  let title = f.name.replace(/\.[^.]+$/, '');
  let number = null;
  const m = title.match(/^(\d{1,3})[\s._-]+(.*)$/);
  if (m) { number = Number(m[1]); title = m[2]; }
  let artist = '';
  const dash = title.indexOf(' - ');
  if (dash > 0) { artist = title.slice(0, dash); title = title.slice(dash + 3); }
  const albumPath = dirOf(f.path);
  return {
    key: `f:${f.path}`, path: f.path, title: title.trim() || f.name, artist, number, size: f.size,
    format: extOf(f.name).toUpperCase(), albumPath, album: albumPath === '/' ? 'Loose tracks' : baseOf(albumPath),
    src: fileUrl(f.path, true), coverUrl: null,
  };
}

function hue(str) {
  let x = 0;
  for (const c of str) x = (x * 31 + c.charCodeAt(0)) >>> 0;
  return x % 360;
}

// album: { name, coverUrl }
function cover(album, size = 'md', round = false) {
  const cls = `cover ${size} ${round ? 'round' : ''}`;
  if (album.coverUrl) return h('span', { class: cls }, h('img', { src: album.coverUrl, alt: '', loading: 'lazy', draggable: 'false', onerror: (e) => e.target.remove() }));
  const hu = hue(album.name || '');
  return h('span', {
    class: `${cls} gen`,
    style: { background: `linear-gradient(135deg, hsl(${hu} 70% 58%), hsl(${(hu + 50) % 360} 70% 38%))` },
  }, icon(round ? 'user' : 'disc', { lg: 56, fill: 48, md: 30, sm: 22 }[size] || 15));
}

const plural = (n, word) => `${n} ${word}${n === 1 ? '' : 's'}`;

function mountMusic(win, args) {
  let tracks = [];
  let albums = [];
  let view = { type: 'albums' };
  let queue = [];
  let index = -1;
  let shuffle = false;
  let repeat = 'off'; // off | all | one
  let filter = '';
  let source = null; // what the queue was built from, so it can be rebuilt on another device
  let restoring = args.restore || null;
  let pendingSeek = null;
  let renderSeq = 0;
  let scrobbled = false;
  const cache = new Map(); // Navidrome responses for the current window, by view

  const audio = new Audio();
  audio.preload = 'metadata';
  try { audio.volume = Number(localStorage.getItem('us.volume') ?? 0.9); } catch { audio.volume = 0.9; }

  const nav = h('div', { class: 'side-list' });
  const ndNav = h('div', { class: 'side-list' });
  const albumNav = h('div', { class: 'side-list side-albums' });
  const content = h('div', { class: 'music-content' });
  const searchInput = h('input', { class: 'input search-input', type: 'search', placeholder: 'Search songs', oninput: debounce(() => onSearch(), 250) });

  // Player bar
  const pCover = h('span', { class: 'p-cover' });
  const pTitle = h('div', { class: 'p-title' }, 'Nothing playing');
  const pSub = h('div', { class: 'p-sub' }, 'Pick a song');
  const pHeart = h('button', { class: 'icon-btn p-heart hidden', 'aria-label': 'Favourite', onclick: () => { const t = queue[index]; if (t) toggleStar(t); } }, icon('heart', 16));
  const playBtn = h('button', { class: 'p-play', 'aria-label': 'Play', onclick: () => toggle() }, icon('play', 20));
  const prevBtn = h('button', { class: 'icon-btn', 'aria-label': 'Previous', onclick: () => prev() }, icon('skipBack', 17));
  const nextBtn = h('button', { class: 'icon-btn', 'aria-label': 'Next', onclick: () => next(true) }, icon('skipForward', 17));
  const shuffleBtn = h('button', { class: 'icon-btn', 'aria-label': 'Shuffle', onclick: () => { shuffle = !shuffle; updateModes(); } }, icon('shuffle', 16));
  const repeatBtn = h('button', { class: 'icon-btn', 'aria-label': 'Repeat', onclick: () => { repeat = repeat === 'off' ? 'all' : repeat === 'all' ? 'one' : 'off'; updateModes(); } }, icon('repeat', 16));
  const seek = h('input', { type: 'range', class: 'range seek', min: '0', max: '1000', value: '0', 'aria-label': 'Seek' });
  const tCur = h('span', { class: 'p-time' }, '0:00');
  const tDur = h('span', { class: 'p-time' }, '0:00');
  const volBtn = h('button', { class: 'icon-btn', 'aria-label': 'Mute', onclick: () => { audio.muted = !audio.muted; updateVol(); } }, icon('volume', 16));
  const vol = h('input', { type: 'range', class: 'range vol', min: '0', max: '100', value: String(Math.round(audio.volume * 100)), 'aria-label': 'Volume' });

  const player = h('footer', { class: 'player' },
    h('div', { class: 'p-now' }, pCover, h('div', { class: 'p-meta' }, pTitle, pSub), pHeart),
    h('div', { class: 'p-center' },
      h('div', { class: 'p-controls' }, shuffleBtn, prevBtn, playBtn, nextBtn, repeatBtn),
      h('div', { class: 'p-seek' }, tCur, seek, tDur)),
    h('div', { class: 'p-right' }, volBtn, vol));

  const ndTitle = h('div', { class: 'side-title' }, 'Navidrome');
  win.body.append(h('div', { class: 'app app-music' },
    h('aside', { class: 'side' },
      h('div', { class: 'side-title' }, 'On this server'), nav,
      ndTitle, ndNav,
      h('div', { class: 'side-title' }, 'Folders'), albumNav),
    h('div', { class: 'main' }, h('div', { class: 'toolbar' }, searchInput, h('div', { class: 'tb-spacer' }),
      h('button', { class: 'icon-btn', 'aria-label': 'Refresh', title: 'Refresh', onclick: () => { cache.clear(); loadLibrary(); } }, icon('refresh', 16))), content),
    player));

  const isNd = (v = view) => v.type.startsWith('nd-');
  const go = (v) => { view = v; filter = ''; searchInput.value = ''; render(); content.querySelector('.scroll')?.scrollTo(0, 0); };

  // ---------- local library ----------

  async function loadLibrary() {
    if (!isNd()) content.replaceChildren(h('div', { class: 'empty-state' }, icon('music', 40), h('div', { class: 'empty-title' }, 'Scanning your music…')));
    try {
      const [audioRes, imageRes] = await Promise.all([
        api('GET', '/api/files/search?kind=audio'),
        api('GET', '/api/files/search?kind=image'),
        nd.load(),
      ]);
      tracks = audioRes.results.map(parseTrack);
      const covers = new Map();
      for (const img of imageRes.results) {
        const dir = dirOf(img.path);
        const stem = img.name.replace(/\.[^.]+$/, '').toLowerCase();
        const score = COVER_NAMES.includes(stem) ? 2 : 1;
        if (!covers.has(dir) || covers.get(dir).score < score) covers.set(dir, { path: img.path, score });
      }
      const byAlbum = new Map();
      for (const t of tracks) {
        if (!byAlbum.has(t.albumPath)) {
          const c = covers.get(t.albumPath)?.path;
          byAlbum.set(t.albumPath, { path: t.albumPath, name: t.album, tracks: [], coverUrl: c ? fileUrl(c, true) : null, artist: '' });
        }
        byAlbum.get(t.albumPath).tracks.push(t);
      }
      albums = [...byAlbum.values()].sort((a, b) => a.name.localeCompare(b.name));
      for (const a of albums) {
        a.tracks.sort((x, y) => (x.number ?? 999) - (y.number ?? 999) || x.title.localeCompare(y.title));
        const artists = new Set(a.tracks.map((t) => t.artist).filter(Boolean));
        a.artist = artists.size === 1 ? [...artists][0] : artists.size ? 'Various artists' : '';
        a.tracks.forEach((t) => { t.coverUrl = a.coverUrl; t.albumObj = a; });
      }
      tracks.sort((a, b) => a.title.localeCompare(b.title));
      // With no local music but a connected Navidrome, start there.
      if (!tracks.length && nd.connected && view.type === 'albums' && !restoring) view = { type: 'nd-albums', sort: 'newest' };
    } catch (err) { report(err); }
    render();
  }

  // ---------- sidebar ----------

  function renderNav() {
    const item = (label, ic, v, count, active) => h('button', {
      class: `side-item ${active ? 'active' : ''}`, onclick: () => go(v),
    }, icon(ic, 16), h('span', null, label), count != null ? h('span', { class: 'side-count' }, count) : null);
    nav.replaceChildren(
      item('Albums', 'disc', { type: 'albums' }, albums.length, view.type === 'albums'),
      item('Songs', 'music', { type: 'songs' }, tracks.length, view.type === 'songs'));
    if (nd.connected) {
      ndNav.replaceChildren(
        item('Albums', 'disc', { type: 'nd-albums', sort: 'newest' }, null, view.type === 'nd-albums' || view.type === 'nd-album'),
        item('Artists', 'user', { type: 'nd-artists' }, null, view.type === 'nd-artists' || view.type === 'nd-artist'),
        item('Playlists', 'list', { type: 'nd-playlists' }, null, view.type === 'nd-playlists' || view.type === 'nd-playlist'),
        item('Favourites', 'heart', { type: 'nd-starred' }, null, view.type === 'nd-starred'),
        h('button', { class: 'side-item', onclick: () => playRandom() }, icon('shuffle', 16), h('span', null, 'Mix for me')));
    } else {
      ndNav.replaceChildren(item('Connect Navidrome', 'link', { type: 'nd-connect' }, null, view.type === 'nd-connect'));
    }
    albumNav.replaceChildren(...albums.map((a) => h('button', {
      class: `side-item album-item ${view.type === 'album' && view.album === a.path ? 'active' : ''}`, onclick: () => go({ type: 'album', album: a.path }),
    }, cover(a, 'xs'), h('span', null, a.name))));
    albumNav.previousElementSibling.classList.toggle('hidden', !albums.length);
    searchInput.placeholder = isNd() ? 'Search Navidrome' : 'Search songs';
  }

  // ---------- views ----------

  function render() {
    const seq = ++renderSeq;
    content.classList.toggle('paused', audio.paused);
    renderNav();
    if (isNd()) { renderNd(seq); return; }

    if (!tracks.length) {
      content.replaceChildren(h('div', { class: 'empty-state' }, icon('music', 44),
        h('div', { class: 'empty-title' }, 'No music in Files yet'),
        h('div', { class: 'empty-sub' }, 'Upload audio files (MP3, FLAC, OGG, M4A…) anywhere in Files. A folder becomes an album; a cover.jpg inside it becomes the artwork.'),
        nd.connected ? h('button', { class: 'btn primary', onclick: () => go({ type: 'nd-albums', sort: 'newest' }) }, icon('disc', 15), 'Browse Navidrome')
          : h('button', { class: 'btn', onclick: () => go({ type: 'nd-connect' }) }, icon('link', 15), 'Connect a Navidrome server')));
      win.setTitle('Music');
      return;
    }
    if (view.type === 'album') {
      const a = albums.find((x) => x.path === view.album);
      if (!a) { view = { type: 'albums' }; render(); return; }
      win.setTitle(`Music — ${a.name}`);
      content.replaceChildren(h('div', { class: 'scroll' },
        hero(a, 'Album', [a.artist, plural(a.tracks.length, 'song'), fmtBytes(a.tracks.reduce((n, t) => n + t.size, 0))], a.tracks, { album: a.path }),
        songTable(a.tracks, false, { album: a.path })));
      return;
    }
    if (view.type === 'songs') {
      const list = filter ? tracks.filter((t) => `${t.title} ${t.artist} ${t.album}`.toLowerCase().includes(filter)) : tracks;
      win.setTitle('Music — Songs');
      content.replaceChildren(h('div', { class: 'scroll' },
        sectionHead(filter ? `Results for “${filter}”` : 'All songs', list, { filter }),
        list.length ? songTable(list, true, { filter }) : h('div', { class: 'empty-state small' }, 'No songs match')));
      return;
    }
    win.setTitle('Music');
    content.replaceChildren(h('div', { class: 'scroll' },
      sectionHead('Albums', tracks, { filter: '' }),
      albumGrid(albums.map((a) => ({ ...a, open: () => go({ type: 'album', album: a.path }), play: () => playList(a.tracks, 0, { album: a.path }), sub: a.artist || plural(a.tracks.length, 'song') })))));
  }

  function sectionHead(title, list, src, extra) {
    return h('div', { class: 'section-head' }, h('h2', null, title), extra || null,
      list?.length ? h('button', { class: 'btn primary small', onclick: () => { shuffle = true; updateModes(); playList(list, Math.floor(Math.random() * list.length), src); } }, icon('shuffle', 14), 'Shuffle all') : null);
  }

  function hero(a, kicker, subParts, list, src, extra) {
    return h('div', { class: 'hero' }, cover(a, 'lg', kicker === 'Artist'), h('div', { class: 'hero-text' },
      h('div', { class: 'hero-kicker' }, kicker), h('div', { class: 'hero-title' }, a.name),
      h('div', { class: 'hero-sub' }, subParts.filter(Boolean).map((p, i) => [i ? ' · ' : '', p])),
      h('div', { class: 'hero-actions' },
        list.length ? h('button', { class: 'btn primary', onclick: () => playList(list, 0, src) }, icon('play', 15), 'Play') : null,
        list.length ? h('button', { class: 'btn', onclick: () => { shuffle = true; updateModes(); playList(list, Math.floor(Math.random() * list.length), src); } }, icon('shuffle', 15), 'Shuffle') : null,
        extra || null)));
  }

  // items: [{ name, coverUrl, sub, open, play, round }]
  function albumGrid(items) {
    return h('div', { class: 'album-grid' }, items.map((a) => h('div', { class: 'album-card', onclick: a.open },
      h('div', { class: `album-art ${a.round ? 'round' : ''}` }, cover(a, 'fill', a.round),
        a.play ? h('button', { class: 'album-play', 'aria-label': `Play ${a.name}`, onclick: (e) => { e.stopPropagation(); a.play(); } }, icon('play', 18)) : null),
      h('div', { class: 'album-name' }, a.name),
      h('div', { class: 'album-sub' }, a.sub || ''))));
  }

  function songTable(list, showAlbum, src) {
    const current = queue[index]?.key;
    const hasTime = list.some((t) => t.duration);
    const hasHeart = list.some((t) => t.nd);
    return h('table', { class: 'table song-table' },
      h('thead', null, h('tr', null, h('th', { class: 'num w-num' }, '#'), h('th', null, 'Title'), showAlbum ? h('th', null, 'Album') : null,
        hasHeart ? h('th', { class: 'w-heart' }) : null, h('th', { class: 'num' }, hasTime ? 'Time' : 'Format'))),
      h('tbody', null, list.map((t, i) => h('tr', {
        class: t.key === current ? 'playing' : '', dataset: { key: t.key }, ondblclick: () => playList(list, i, src),
        onclick: () => { if (matchMedia('(pointer: coarse)').matches) playList(list, i, src); },
      },
      h('td', { class: 'num w-num' }, h('span', { class: 'n' }, showAlbum ? i + 1 : t.number ?? i + 1), h('span', { class: 'eq' }, h('i'), h('i'), h('i')),
        h('button', { class: 'row-play', 'aria-label': `Play ${t.title}`, onclick: (e) => { e.stopPropagation(); playList(list, i, src); } }, icon('play', 12))),
      h('td', null, h('div', { class: 'song' }, showAlbum ? cover({ name: t.album, coverUrl: t.coverUrl }, 'xs') : null,
        h('div', null, h('div', { class: 'song-title' }, t.title), t.artist ? h('div', { class: 'song-artist' }, t.artist) : null))),
      showAlbum ? h('td', { class: 'muted song-album' }, t.nd && t.albumId
        ? h('button', { class: 'link-plain', onclick: (e) => { e.stopPropagation(); go({ type: 'nd-album', id: t.albumId }); } }, t.album) : t.album) : null,
      hasHeart ? h('td', { class: 'w-heart' }, t.nd ? heartBtn(t) : null) : null,
      h('td', { class: 'num muted' }, hasTime ? fmtClock(t.duration) : t.format)))));
  }

  function heartBtn(t) {
    return h('button', {
      class: `heart ${t.starred ? 'on' : ''}`, 'aria-label': t.starred ? 'Remove from favourites' : 'Add to favourites', dataset: { heart: t.key },
      onclick: (e) => { e.stopPropagation(); toggleStar(t); },
    }, icon('heart', 14));
  }

  async function toggleStar(t) {
    if (!t.nd) return;
    const on = !t.starred;
    try {
      await nd.call(on ? 'star' : 'unstar', { id: t.id });
      // The same song may appear in several lists; update every copy we hold.
      for (const x of [...queue, ...[...cache.values()].flatMap((c) => c.songs || [])]) if (x.key === t.key) x.starred = on;
      t.starred = on;
      content.querySelectorAll(`[data-heart="${CSS.escape(t.key)}"]`).forEach((b) => b.classList.toggle('on', on));
      updateHeart();
      cache.delete('starred');
    } catch (err) { report(err); }
  }

  // ---------- Navidrome views ----------

  async function cached(key, fn) {
    if (cache.has(key)) return cache.get(key);
    const v = await fn();
    cache.set(key, v);
    return v;
  }

  function loading() {
    content.replaceChildren(h('div', { class: 'empty-state' }, h('span', { class: 'bw-spin big' }), h('div', { class: 'empty-sub' }, 'Loading from Navidrome…')));
  }

  async function renderNd(seq) {
    if (view.type === 'nd-connect') { renderConnect(); return; }
    if (!nd.connected) { view = { type: 'nd-connect' }; renderConnect(); return; }
    const key = JSON.stringify(view);
    if (!cache.has(key)) loading();
    try {
      const node = await ndView(key);
      if (seq !== renderSeq) return;
      content.replaceChildren(node);
      markPlaying();
    } catch (err) {
      if (seq !== renderSeq) return;
      content.replaceChildren(h('div', { class: 'empty-state' }, icon('info', 36), h('div', { class: 'empty-title' }, 'Navidrome did not answer'),
        h('div', { class: 'empty-sub' }, err.message),
        h('div', { class: 'hero-actions' }, h('button', { class: 'btn', onclick: () => { cache.clear(); render(); } }, 'Try again'),
          h('button', { class: 'btn', onclick: () => go({ type: 'nd-connect' }) }, 'Connection settings'))));
    }
  }

  async function ndView(key) {
    const v = view;
    switch (v.type) {
      case 'nd-albums': {
        const sort = v.sort || 'newest';
        const data = await cached(key, async () => ({ albums: await fetchAlbums(sort, 0), done: false }));
        win.setTitle('Music — Navidrome');
        const seg = h('div', { class: 'seg' }, ND_SORTS.map(([id, label]) => h('button', { class: `seg-btn text ${id === sort ? 'active' : ''}`, onclick: () => go({ type: 'nd-albums', sort: id }) }, label)));
        const grid = albumGrid(data.albums.map(ndAlbumCard));
        const more = h('button', {
          class: `btn ${data.done || data.albums.length < 60 ? 'hidden' : ''}`,
          onclick: async () => {
            more.disabled = true;
            try {
              const next = await fetchAlbums(sort, data.albums.length);
              data.albums.push(...next);
              if (next.length < 60) { data.done = true; more.classList.add('hidden'); }
              grid.append(...albumGrid(next.map(ndAlbumCard)).children);
            } catch (err) { report(err); }
            more.disabled = false;
          },
        }, 'Load more');
        return h('div', { class: 'scroll' }, h('div', { class: 'section-head' }, h('h2', null, 'Albums'), seg), grid, h('div', { class: 'load-more' }, more));
      }
      case 'nd-album': {
        const { album, songs } = await cached(key, async () => {
          const r = await nd.call('getAlbum', { id: v.id });
          return { album: nd.album(r.album), songs: (r.album.song || []).map((s) => nd.track(s)) };
        });
        win.setTitle(`Music — ${album.name}`);
        const artistLink = album.artistId ? h('button', { class: 'link-plain', onclick: () => go({ type: 'nd-artist', id: album.artistId }) }, album.artist) : album.artist;
        return h('div', { class: 'scroll' },
          hero(album, 'Album', [artistLink, album.year, plural(songs.length, 'song'), fmtDuration(album.duration)], songs, { nd: 'album', id: v.id }),
          songTable(songs, false, { nd: 'album', id: v.id }));
      }
      case 'nd-artists': {
        const artists = await cached(key, async () => {
          const r = await nd.call('getArtists');
          return (r.artists.index || []).flatMap((i) => i.artist || []);
        });
        win.setTitle('Music — Artists');
        return h('div', { class: 'scroll' }, h('div', { class: 'section-head' }, h('h2', null, `Artists`), h('span', { class: 'muted' }, plural(artists.length, 'artist'))),
          albumGrid(artists.map((a) => ({
            name: a.name, round: true, coverUrl: nd.coverUrl(a.coverArt || a.id, 300), sub: plural(a.albumCount || 0, 'album'),
            open: () => go({ type: 'nd-artist', id: a.id }),
          }))));
      }
      case 'nd-artist': {
        const { artist, albums: list } = await cached(key, async () => {
          const r = await nd.call('getArtist', { id: v.id });
          return { artist: r.artist, albums: (r.artist.album || []).map((a) => nd.album(a)) };
        });
        win.setTitle(`Music — ${artist.name}`);
        const playAll = async () => {
          try {
            const all = (await Promise.all(list.map((a) => nd.call('getAlbum', { id: a.id })))).flatMap((r) => (r.album.song || []).map((s) => nd.track(s)));
            playList(all, 0, { nd: 'list', ids: all.slice(0, 300).map((t) => t.id) });
          } catch (err) { report(err); }
        };
        return h('div', { class: 'scroll' },
          h('div', { class: 'hero' }, cover({ name: artist.name, coverUrl: nd.coverUrl(artist.coverArt || artist.id, 300) }, 'lg', true),
            h('div', { class: 'hero-text' }, h('div', { class: 'hero-kicker' }, 'Artist'), h('div', { class: 'hero-title' }, artist.name),
              h('div', { class: 'hero-sub' }, plural(list.length, 'album')),
              h('div', { class: 'hero-actions' }, h('button', { class: 'btn primary', onclick: playAll }, icon('play', 15), 'Play all')))),
          albumGrid(list.map(ndAlbumCard)));
      }
      case 'nd-playlists': {
        const lists = await cached(key, async () => (await nd.call('getPlaylists')).playlists.playlist || []);
        win.setTitle('Music — Playlists');
        return h('div', { class: 'scroll' }, h('div', { class: 'section-head' }, h('h2', null, 'Playlists')),
          lists.length ? albumGrid(lists.map((p) => ({
            name: p.name, coverUrl: nd.coverUrl(p.coverArt, 300), sub: `${plural(p.songCount || 0, 'song')} · ${fmtDuration(p.duration || 0)}`,
            open: () => go({ type: 'nd-playlist', id: p.id }),
            play: async () => { try { const s = await playlistSongs(p.id); playList(s, 0, { nd: 'playlist', id: p.id }); } catch (err) { report(err); } },
          }))) : h('div', { class: 'empty-state small' }, 'No playlists yet. Create them in Navidrome.'));
      }
      case 'nd-playlist': {
        const { pl, songs } = await cached(key, async () => {
          const r = await nd.call('getPlaylist', { id: v.id });
          return { pl: r.playlist, songs: (r.playlist.entry || []).map((s) => nd.track(s)) };
        });
        win.setTitle(`Music — ${pl.name}`);
        return h('div', { class: 'scroll' },
          hero({ name: pl.name, coverUrl: nd.coverUrl(pl.coverArt, 300) }, 'Playlist', [pl.owner, plural(songs.length, 'song'), fmtDuration(pl.duration || 0)], songs, { nd: 'playlist', id: v.id }),
          songTable(songs, true, { nd: 'playlist', id: v.id }));
      }
      case 'nd-starred': {
        const data = await cached('starred', async () => {
          const r = await nd.call('getStarred2');
          return { songs: (r.starred2.song || []).map((s) => nd.track(s)), albums: (r.starred2.album || []).map((a) => nd.album(a)) };
        });
        win.setTitle('Music — Favourites');
        return h('div', { class: 'scroll' },
          sectionHead('Favourite songs', data.songs, { nd: 'starred' }),
          data.songs.length ? songTable(data.songs, true, { nd: 'starred' }) : h('div', { class: 'empty-state small' }, 'Tap the heart next to a song to add it here.'),
          data.albums.length ? [h('div', { class: 'section-head' }, h('h2', null, 'Favourite albums')), albumGrid(data.albums.map(ndAlbumCard))] : null);
      }
      case 'nd-search': {
        const r = await cached(key, async () => (await nd.call('search3', { query: v.q, artistCount: 12, albumCount: 24, songCount: 100 })).searchResult3);
        const songs = (r.song || []).map((s) => nd.track(s));
        const albumsFound = (r.album || []).map((a) => nd.album(a));
        const artists = r.artist || [];
        win.setTitle('Music — Search');
        if (!songs.length && !albumsFound.length && !artists.length) return h('div', { class: 'empty-state' }, icon('search', 36), h('div', { class: 'empty-title' }, `Nothing found for “${v.q}”`));
        return h('div', { class: 'scroll' },
          artists.length ? [h('div', { class: 'section-head' }, h('h2', null, 'Artists')), albumGrid(artists.map((a) => ({
            name: a.name, round: true, coverUrl: nd.coverUrl(a.coverArt || a.id, 300), sub: plural(a.albumCount || 0, 'album'), open: () => go({ type: 'nd-artist', id: a.id }),
          })))] : null,
          albumsFound.length ? [h('div', { class: 'section-head' }, h('h2', null, 'Albums')), albumGrid(albumsFound.map(ndAlbumCard))] : null,
          songs.length ? [sectionHead('Songs', songs, { nd: 'list', ids: songs.map((t) => t.id) }), songTable(songs, true, { nd: 'list', ids: songs.map((t) => t.id) })] : null);
      }
    }
    return h('div');
  }

  function ndAlbumCard(a) {
    return {
      ...a, sub: [a.artist, a.year].filter(Boolean).join(' · '),
      open: () => go({ type: 'nd-album', id: a.id }),
      play: async () => {
        try { const r = await nd.call('getAlbum', { id: a.id }); playList((r.album.song || []).map((s) => nd.track(s)), 0, { nd: 'album', id: a.id }); } catch (err) { report(err); }
      },
    };
  }

  async function fetchAlbums(sort, offset) {
    const r = await nd.call('getAlbumList2', { type: sort, size: 60, offset });
    return (r.albumList2.album || []).map((a) => nd.album(a));
  }

  async function playlistSongs(id) {
    return ((await nd.call('getPlaylist', { id })).playlist.entry || []).map((s) => nd.track(s));
  }

  async function playRandom() {
    try {
      const r = await nd.call('getRandomSongs', { size: 60 });
      const songs = (r.randomSongs.song || []).map((s) => nd.track(s));
      if (!songs.length) { toast('Navidrome has no songs yet'); return; }
      playList(songs, 0, { nd: 'list', ids: songs.map((t) => t.id) });
      toast('Playing a random mix from Navidrome');
    } catch (err) { report(err); }
  }

  function onSearch() {
    const q = searchInput.value.trim();
    if (isNd() || (!tracks.length && nd.connected)) {
      if (q.length >= 2) { view = { type: 'nd-search', q }; render(); } else if (!q && view.type === 'nd-search') { view = { type: 'nd-albums', sort: 'newest' }; render(); }
      return;
    }
    filter = q.toLowerCase();
    if (filter) view = { type: 'songs' };
    render();
  }

  function renderConnect() {
    win.setTitle('Music — Navidrome');
    const urlIn = h('input', { class: 'input', type: 'url', placeholder: 'https://music.example.com', value: nd.status?.url || '', autocomplete: 'url', spellcheck: 'false' });
    const userIn = h('input', { class: 'input', type: 'text', value: nd.status?.user || '', autocomplete: 'username', spellcheck: 'false' });
    const passIn = h('input', { class: 'input', type: 'password', autocomplete: 'current-password' });
    const error = h('div', { class: 'form-error' });
    const submit = h('button', { class: 'btn primary', type: 'submit' }, icon('link', 15), nd.connected ? 'Reconnect' : 'Connect');
    content.replaceChildren(h('div', { class: 'scroll' }, h('div', { class: 'nd-connect' },
      h('div', { class: 'nd-connect-head' }, h('span', { class: 'nd-logo' }, icon('disc', 26)),
        h('div', null, h('h2', null, nd.connected ? 'Navidrome connection' : 'Connect your Navidrome'),
          h('p', { class: 'set-desc' }, 'Browse and play your Navidrome library here, alongside music in Files. Works with any Subsonic-compatible server.'))),
      h('form', {
        class: 'set-form',
        onsubmit: async (e) => {
          e.preventDefault();
          error.textContent = '';
          submit.disabled = true;
          try {
            const res = await api('POST', '/api/navidrome/connect', { url: urlIn.value, username: userIn.value, password: passIn.value });
            nd.status = res;
            cache.clear();
            toast(`Connected to ${res.server || 'Navidrome'}`);
            go({ type: 'nd-albums', sort: 'newest' });
          } catch (err) { error.textContent = err.message; }
          submit.disabled = false;
        },
      },
      h('label', { class: 'field-label' }, 'Server address'), urlIn,
      h('label', { class: 'field-label' }, 'Navidrome username'), userIn,
      h('label', { class: 'field-label' }, 'Navidrome password'), passIn,
      error, h('div', { class: 'hero-actions' }, submit,
        nd.connected ? h('button', {
          class: 'btn', type: 'button',
          onclick: async () => { await api('POST', '/api/navidrome/disconnect').catch(report); nd.status = { connected: false }; cache.clear(); toast('Navidrome disconnected'); go({ type: 'albums' }); },
        }, 'Disconnect') : null)),
      h('p', { class: 'bw-note' }, icon('lock', 13), 'Your password is not stored: the server keeps only a salted login token, and music streams through ultraspace so it works on every device you sign in from.'))));
    if (!urlIn.value) urlIn.focus(); else if (!passIn.value) passIn.focus();
  }

  // ---------- playback ----------

  function playList(list, i, src = null) {
    if (!list.length) return;
    source = src;
    restoring = null;
    queue = [...list];
    index = Math.max(0, i);
    load(true);
  }

  function load(autoplay) {
    const t = queue[index];
    if (!t) return;
    scrobbled = false;
    audio.src = t.src;
    // NotAllowedError means the browser wants a click first; the play button then does it.
    if (autoplay) audio.play().catch((err) => { if (err.name !== 'NotAllowedError') report(new Error(`Cannot play ${t.title}: ${err.message}`)); });
    pCover.replaceChildren(cover({ name: t.album, coverUrl: t.coverUrl }, 'sm'));
    pTitle.textContent = t.title;
    pSub.textContent = [t.artist, t.album].filter(Boolean).join(' — ');
    tDur.textContent = t.duration ? fmtClock(t.duration) : '0:00';
    updateHeart();
    if ('mediaSession' in navigator) {
      navigator.mediaSession.metadata = new MediaMetadata({
        title: t.title, artist: t.artist || t.album, album: t.album,
        artwork: t.coverUrl ? [{ src: t.coverUrl }] : [],
      });
    }
    if (t.nd && autoplay) nd.call('scrobble', { id: t.id, submission: false }).catch(() => {}); // "now playing"
    markPlaying();
    broadcast();
  }

  function updateHeart() {
    const t = queue[index];
    pHeart.classList.toggle('hidden', !t?.nd);
    pHeart.classList.toggle('on', !!t?.starred);
  }

  function markPlaying() {
    const current = queue[index]?.key;
    content.querySelectorAll('tr[data-key]').forEach((tr) => tr.classList.toggle('playing', tr.dataset.key === current));
    content.classList.toggle('paused', audio.paused);
  }

  function toggle() {
    if (index < 0) {
      if (tracks.length) playList(tracks, 0, { filter: '' });
      else if (nd.connected) playRandom();
      return;
    }
    audio.paused ? audio.play().catch(report) : audio.pause();
  }

  function next(manual) {
    if (!queue.length) return;
    if (repeat === 'one' && !manual) { audio.currentTime = 0; audio.play(); return; }
    if (shuffle && queue.length > 1) {
      let n;
      do { n = Math.floor(Math.random() * queue.length); } while (n === index);
      index = n;
    } else if (index + 1 < queue.length) {
      index++;
    } else if (repeat === 'all' || manual) {
      index = 0;
    } else {
      audio.pause();
      return;
    }
    load(true);
  }

  function prev() {
    if (!queue.length) return;
    if (audio.currentTime > 3) { audio.currentTime = 0; return; }
    index = (index - 1 + queue.length) % queue.length;
    load(true);
  }

  function updateModes() {
    shuffleBtn.classList.toggle('on', shuffle);
    repeatBtn.classList.toggle('on', repeat !== 'off');
    repeatBtn.dataset.mode = repeat;
    repeatBtn.title = `Repeat: ${repeat}`;
  }

  function updateVol() {
    volBtn.replaceChildren(icon(audio.muted || audio.volume === 0 ? 'volumeX' : 'volume', 16));
    vol.value = String(Math.round(audio.volume * 100));
  }

  function broadcast() {
    emit('music', { track: queue[index] || null, playing: !audio.paused });
  }

  let seeking = false;
  audio.addEventListener('timeupdate', () => {
    const d = audio.duration || queue[index]?.duration;
    if (seeking || !d || !Number.isFinite(d)) return;
    seek.value = String(Math.round((1000 * audio.currentTime) / d));
    seek.style.setProperty('--fill', `${(100 * audio.currentTime) / d}%`);
    tCur.textContent = fmtClock(audio.currentTime);
    // Count a play in Navidrome after half the song (or four minutes), like other players.
    const t = queue[index];
    if (t?.nd && !scrobbled && audio.currentTime >= Math.min(240, d / 2)) {
      scrobbled = true;
      nd.call('scrobble', { id: t.id, submission: true }).catch(() => {});
    }
  });
  audio.addEventListener('loadedmetadata', () => {
    if (Number.isFinite(audio.duration)) tDur.textContent = fmtClock(audio.duration);
    if (pendingSeek != null) { audio.currentTime = Math.min(pendingSeek, audio.duration || pendingSeek); pendingSeek = null; }
  });
  audio.addEventListener('play', () => { playBtn.replaceChildren(icon('pause', 20)); playBtn.setAttribute('aria-label', 'Pause'); broadcast(); markPlaying(); });
  audio.addEventListener('pause', () => { playBtn.replaceChildren(icon('play', 20)); playBtn.setAttribute('aria-label', 'Play'); broadcast(); markPlaying(); });
  audio.addEventListener('ended', () => next(false));
  audio.addEventListener('error', () => { if (audio.src) report(new Error(`Cannot play ${queue[index]?.title || 'this file'} in this browser`)); });
  seek.addEventListener('input', () => {
    seeking = true;
    seek.style.setProperty('--fill', `${seek.value / 10}%`);
    const d = audio.duration || queue[index]?.duration;
    if (d) tCur.textContent = fmtClock((seek.value / 1000) * d);
  });
  seek.addEventListener('change', () => {
    const d = audio.duration || queue[index]?.duration;
    if (d) audio.currentTime = (seek.value / 1000) * d;
    seeking = false;
  });
  vol.addEventListener('input', () => {
    audio.volume = vol.value / 100;
    audio.muted = false;
    vol.style.setProperty('--fill', `${vol.value}%`);
    try { localStorage.setItem('us.volume', String(audio.volume)); } catch { /* ignore */ }
    updateVol();
  });
  vol.style.setProperty('--fill', `${vol.value}%`);
  win.body.addEventListener('keydown', (e) => {
    if (e.target.closest('input, textarea')) return;
    if (e.key === ' ') { e.preventDefault(); toggle(); }
    else if (e.key === 'ArrowRight' && e.shiftKey) next(true);
    else if (e.key === 'ArrowLeft' && e.shiftKey) prev();
  });
  win.body.tabIndex = -1;
  if ('mediaSession' in navigator) {
    navigator.mediaSession.setActionHandler('play', () => audio.play());
    navigator.mediaSession.setActionHandler('pause', () => audio.pause());
    navigator.mediaSession.setActionHandler('nexttrack', () => next(true));
    navigator.mediaSession.setActionHandler('previoustrack', () => prev());
  }

  updateModes();
  updateVol();
  const ready = loadLibrary();
  const playPath = async (p) => {
    await ready;
    const t = tracks.find((x) => x.path === p);
    if (!t) return;
    const album = t.albumObj;
    playList(album.tracks, album.tracks.indexOf(t), { album: album.path });
    go({ type: 'album', album: album.path });
  };
  if (args.play) playPath(args.play);

  // ---------- restore a queue saved on another device; playback waits for a click ----------

  async function sourceList(src) {
    if (src?.nd) {
      if (src.nd === 'album') return ((await nd.call('getAlbum', { id: src.id })).album.song || []).map((s) => nd.track(s));
      if (src.nd === 'playlist') return playlistSongs(src.id);
      if (src.nd === 'starred') return ((await nd.call('getStarred2')).starred2.song || []).map((s) => nd.track(s));
      if (src.nd === 'list' && Array.isArray(src.ids)) {
        const songs = await Promise.all(src.ids.slice(0, 300).map((id) => nd.call('getSong', { id }).then((r) => nd.track(r.song), () => null)));
        return songs.filter(Boolean);
      }
      return [];
    }
    if (src?.album) return albums.find((a) => a.path === src.album)?.tracks || [];
    if (src && typeof src.filter === 'string') {
      const f = src.filter;
      return f ? tracks.filter((t) => `${t.title} ${t.artist} ${t.album}`.toLowerCase().includes(f)) : tracks;
    }
    return [];
  }

  if (restoring) {
    ready.then(async () => {
      const r = restoring;
      if (!r) return; // the user started something else meanwhile
      if (typeof r.view?.type === 'string' && (!r.view.type.startsWith('nd-') || nd.connected)) view = r.view;
      shuffle = !!r.shuffle;
      repeat = ['off', 'all', 'one'].includes(r.repeat) ? r.repeat : 'off';
      updateModes();
      // Older saves stored a bare file path.
      const key = r.track && !/^[fn]:/.test(r.track) ? `f:${r.track}` : r.track;
      try {
        if (key) {
          let list = [];
          if (!(r.source?.nd && !nd.connected)) list = await sourceList(r.source).catch(() => []);
          let t = list.find((x) => x.key === key);
          let src = r.source;
          if (!t && key.startsWith('f:')) {
            t = tracks.find((x) => x.key === key);
            if (t) { list = t.albumObj.tracks; src = { album: t.albumObj.path }; }
          }
          if (t && restoring === r) {
            source = src;
            queue = [...list];
            index = queue.indexOf(t);
            pendingSeek = Number(r.time) || 0;
            load(false);
          }
        }
      } catch { /* the saved queue is gone; start fresh */ }
      if (restoring === r) restoring = null;
      render();
    });
  }

  return {
    onArgs(a) { if (a.play) playPath(a.play); },
    saveState() {
      if (restoring) return restoring;
      const t = queue[index];
      return {
        view, shuffle, repeat,
        track: t?.key || null, source: t ? source : null,
        time: t ? Math.floor(pendingSeek ?? audio.currentTime) : 0,
      };
    },
    onDestroy() {
      audio.pause();
      audio.removeAttribute('src');
      audio.load();
      emit('music', null);
      if ('mediaSession' in navigator) navigator.mediaSession.metadata = null;
    },
  };
}
