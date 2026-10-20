// Shared by both Browser engines: address parsing, search engines, site icons, and the
// bookmarks/history document saved on the server.
import { h, api, report, emit } from '../core.js';
import { device } from '../session.js';

const enc = encodeURIComponent;

export const ENGINES = {
  google: { name: 'Google', search: (q, embed) => `https://www.google.com/search?${embed ? 'igu=1&' : ''}q=${enc(q)}` },
  wikipedia: { name: 'Wikipedia', search: (q) => `https://en.wikipedia.org/w/index.php?search=${enc(q)}` },
  duckduckgo: { name: 'DuckDuckGo', search: (q) => `https://duckduckgo.com/?q=${enc(q)}` },
  bing: { name: 'Bing', search: (q) => `https://www.bing.com/search?q=${enc(q)}` },
};


// What typing `input` into the address bar means: { url } to open, { search } or { error }.
// embed rewrites addresses to forms that work inside a frame (light mode only).
export function webTarget(input, { embed = false } = {}) {
  const s = String(input || '').trim();
  if (!s) return null;
  if (/^https?:\/\//i.test(s)) return parseUrl(s, embed);
  const hostLike = /^(localhost|\d{1,3}(\.\d{1,3}){3}|[^\s/:?#]+\.[a-z][a-z0-9-]+)(:\d+)?([/?#]|$)/i;
  if (!/\s/.test(s) && hostLike.test(s)) return parseUrl(`https://${s}`, embed);
  if (/^[a-z][a-z0-9+.-]*:\S/i.test(s) && !/\s/.test(s)) return { error: 'Only web addresses (http:// and https://) can be opened here.' };
  return { search: s };
}

function parseUrl(s, embed) {
  try {
    const u = new URL(s);
    if (u.protocol !== 'http:' && u.protocol !== 'https:') return { error: 'Only web addresses can be opened here.' };
    return { url: embed ? rewrite(u) : u.href };
  } catch {
    return { error: 'That is not a valid web address.' };
  }
}

// Some sites have an embeddable form of the same page.
function rewrite(u) {
  const host = u.hostname.replace(/^(www|m)\./, '');
  const v = u.searchParams.get('v');
  if (host === 'youtube.com' && u.pathname === '/watch' && v) return `https://www.youtube.com/embed/${enc(v)}`;
  if (host === 'youtu.be' && u.pathname.length > 1) return `https://www.youtube.com/embed/${enc(u.pathname.slice(1))}`;
  if (host === 'google.com' && (u.pathname === '/' || u.pathname === '/search' || u.pathname === '/webhp') && !u.searchParams.has('igu')) {
    u.searchParams.set('igu', '1');
  }
  return u.href;
}

export const hostOf = (url) => { try { return new URL(url).hostname.replace(/^www\./, ''); } catch { return url; } };

function hue(str) {
  let x = 0;
  for (const c of str) x = (x * 31 + c.charCodeAt(0)) >>> 0;
  return x % 360;
}

export function siteIcon(url, size = 'sm') {
  const host = hostOf(url || '');
  // Letter of the site's name: "wikipedia" for en.wikipedia.org, "bbc" for www.bbc.co.uk.
  const labels = host.split('.');
  let name = labels.length > 1 ? labels[labels.length - 2] : labels[0];
  if (labels.length > 2 && name.length <= 3 && labels[labels.length - 1].length === 2) name = labels[labels.length - 3];
  const hu = hue(name);
  return h('span', { class: `site-icon ${size}`, style: { background: `linear-gradient(135deg, hsl(${hu} 65% 55%), hsl(${(hu + 40) % 360} 65% 40%))` } },
    (name.replace(/^[^a-z0-9]+/i, '')[0] || '?').toUpperCase());
}

// ---------- bookmarks and history, shared by all browser windows ----------

export const data = {
  value: { bookmarks: [], history: [], engine: 'google', mode: 'auto', sharp: false },
  fetched: 0,
  chain: Promise.resolve(),
  async refresh(force = false) {
    if (!force && Date.now() - this.fetched < 30_000) return;
    try {
      const res = await api('GET', '/api/state/browser');
      if (res.data) this.value = { ...this.value, ...res.data };
      this.fetched = Date.now();
      emit('browser-data');
    } catch { /* keep what we have */ }
  },
  // Read-modify-write against the latest copy, so two devices do not overwrite each other's bookmarks.
  change(fn) {
    this.chain = this.chain.then(async () => {
      await this.refresh(true);
      fn(this.value);
      emit('browser-data');
      await api('PUT', '/api/state/browser', { device: device.id, device_name: device.name, data: this.value }).catch(report);
    });
    return this.chain;
  },
  isBookmarked(url) { return this.value.bookmarks.some((b) => b.url === url); },
};

export function addHistory(url, title) {
  data.change((d) => {
    d.history = [{ url, title, t: Date.now() }, ...d.history.filter((x) => x.url !== url)].slice(0, 300);
  });
}

