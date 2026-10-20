// Navidrome (Subsonic API) helpers for the Music app. Every call goes through
// ultraspace, which adds the stored credentials; the browser never sees them.
import { api } from '../core.js';

const enc = encodeURIComponent;

export const nd = {
  status: null,

  async load() {
    try { this.status = await api('GET', '/api/navidrome'); } catch { this.status = { connected: false }; }
    return this.status;
  },

  get connected() { return !!this.status?.connected; },

  async call(method, params = {}) {
    const q = new URLSearchParams();
    for (const [k, v] of Object.entries(params)) {
      if (Array.isArray(v)) v.forEach((x) => q.append(k, x));
      else if (v != null) q.set(k, v);
    }
    const res = await api('GET', `/api/navidrome/rest/${method}?${q}`);
    const r = res['subsonic-response'];
    if (!r || r.status !== 'ok') throw new Error(`Navidrome: ${r?.error?.message || 'request failed'}`);
    return r;
  },

  coverUrl(id, size = 300) { return id ? `/api/navidrome/rest/getCoverArt?id=${enc(id)}&size=${size}` : null; },
  streamUrl(id) { return `/api/navidrome/rest/stream?id=${enc(id)}`; },

  track(s) {
    return {
      key: `n:${s.id}`, id: s.id, nd: true,
      title: s.title || 'Untitled', artist: s.artist || s.displayArtist || '', album: s.album || '',
      albumId: s.albumId, artistId: s.artistId, number: s.track ?? null, duration: s.duration || 0,
      format: (s.suffix || '').toUpperCase(), size: s.size || 0, starred: !!s.starred,
      src: this.streamUrl(s.id), coverUrl: this.coverUrl(s.coverArt || s.albumId, 300),
    };
  },

  album(a) {
    return {
      id: a.id, name: a.name || a.album || 'Untitled', artist: a.artist || a.displayArtist || '', artistId: a.artistId,
      coverUrl: this.coverUrl(a.coverArt || a.id, 300), year: a.year, songCount: a.songCount || 0, duration: a.duration || 0,
      starred: !!a.starred,
    };
  },
};
