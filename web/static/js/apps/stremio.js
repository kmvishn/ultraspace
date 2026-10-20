// Stremio: films and series from Stremio addons (https://www.stremio.com/addons). Catalogs
// and details come from Cinemeta; streams come from whatever addons you install. Picking a
// stream plays it in Videos: web streams through the server, torrents while they download.
// Signed in to a Stremio account, addons, the library and watch progress stay in sync with
// the official apps; otherwise they are kept on this server and follow you to every device.
import { h, icon, api, report, toast, notice, menu, dialog, confirmDialog, debounce, fmtClock } from '../core.js';
import { registerApp, openApp } from '../wm.js';
import { device } from '../session.js';

registerApp({
  id: 'stremio', name: 'Stremio', icon: 'tv', color: ['#8b5cf6', '#4c1d95'], width: 1120, height: 720, minWidth: 420, minHeight: 320,
  mount(win, args) { return mountStremio(win, args); },
});

const img = (u) => (u ? `/api/stremio/img?u=${encodeURIComponent(u)}` : '');
const TYPE_LABEL = { movie: 'Films', series: 'Series', channel: 'Channels', tv: 'TV', anime: 'Anime', other: 'Other' };
const typeLabel = (t) => TYPE_LABEL[t] || t[0].toUpperCase() + t.slice(1);

// Subtitle languages to offer first, from the browser's languages (OpenSubtitles uses ISO 639-2).
const LANG3 = { en: 'eng', es: 'spa', fr: 'fre', de: 'ger', it: 'ita', pt: 'por', ru: 'rus', hi: 'hin', ml: 'mal', ta: 'tam', te: 'tel', kn: 'kan', bn: 'ben', ar: 'ara', ja: 'jpn', ko: 'kor', zh: 'chi', nl: 'dut', pl: 'pol', tr: 'tur', sv: 'swe', id: 'ind' };
const wantLangs = [...new Set([...(navigator.languages || [navigator.language]).map((l) => LANG3[l.slice(0, 2)]).filter(Boolean), 'eng'])];

// ---------- addons ----------

const store = {
  data: null,
  account: { signedIn: false },
  remote: [], // the account library (saved and watched titles)
  async load() {
    if (this.data) return this.data;
    const [res, acc] = await Promise.all([
      api('GET', '/api/state/stremio').catch(() => ({ data: null })),
      api('GET', '/api/stremio/account').catch(() => ({ signedIn: false })),
    ]);
    this.data = res.data || null;
    this.account = acc;
    if (!this.data?.addons) {
      // First run: install the default addons (catalogs and subtitles).
      const { addons } = await api('GET', '/api/stremio/defaults');
      const got = await Promise.all(addons.map((u) => api('POST', '/api/stremio/manifest', { url: u }).catch(() => null)));
      this.data = { addons: got.filter(Boolean), library: this.data?.library || [] };
      this.save();
    }
    this.data.library ||= [];
    if (acc.signedIn) await this.pull();
    return this.data;
  },
  // pull takes addons and the library from the account (the official apps may have changed them).
  async pull() {
    try {
      const [{ addons }, { items }] = await Promise.all([api('GET', '/api/stremio/account/addons'), api('GET', '/api/stremio/account/library')]);
      if (addons.length) this.data.addons = addons;
      this.remote = items;
      this.save();
    } catch (err) {
      if (err.status === 401) this.account = { signedIn: false };
      else report(err);
    }
  },
  save() {
    api('PUT', '/api/state/stremio', { device: device.id, device_name: device.name, data: this.data }).catch(report);
  },
  // saveAddons stores the addon list here and, when signed in, in the account.
  saveAddons() {
    this.save();
    if (this.account.signedIn) api('PUT', '/api/stremio/account/addons', { addons: this.data.addons }).catch(report);
  },
  inLibrary(id) {
    return this.account.signedIn ? this.remote.some((x) => x.id === id && !x.removed) : this.data.library.some((x) => x.id === id);
  },
  async setLibrary(meta, type, saved) {
    if (this.account.signedIn) {
      await api('POST', '/api/stremio/account/library', { id: meta.id, type, name: meta.name, poster: meta.poster, saved });
      const i = this.remote.findIndex((x) => x.id === meta.id);
      const item = { ...(this.remote[i] || {}), id: meta.id, type, name: meta.name, poster: meta.poster, removed: !saved, temp: false, mtime: new Date().toISOString() };
      if (i >= 0) this.remote[i] = item; else this.remote.push(item);
      return;
    }
    if (saved) this.data.library.push({ id: meta.id, type, name: meta.name, poster: meta.poster, releaseInfo: meta.releaseInfo, added: Date.now() });
    else this.data.library = this.data.library.filter((x) => x.id !== meta.id);
    this.save();
  },
};

// supports reports whether an addon serves a resource for this type and id.
function supports(m, resource, type, id) {
  for (const r of m.resources || []) {
    const name = typeof r === 'string' ? r : r.name;
    if (name !== resource) continue;
    const types = (typeof r === 'object' && r.types) || m.types || [];
    const prefixes = (typeof r === 'object' && r.idPrefixes) || m.idPrefixes;
    if (type && !types.includes(type)) continue;
    if (id && prefixes?.length && !prefixes.some((p) => id.startsWith(p))) continue;
    return true;
  }
  return false;
}

const extrasOf = (c) => (c.extra || []).map((e) => (typeof e === 'string' ? { name: e } : e)).concat((c.extraSupported || []).map((name) => ({ name })));
const requiredOf = (c) => [...(c.extra || []).filter((e) => e.isRequired).map((e) => e.name), ...(c.extraRequired || [])];

// Some addons send HTML entities in plain text ("O&apos;Dea"); decode them everywhere.
const ENT = { amp: '&', apos: "'", quot: '"', lt: '<', gt: '>', nbsp: '\u00a0' };
const unent = (s) => s.replace(/&(#x[0-9a-f]+|#\d+|amp|apos|quot|lt|gt|nbsp);/gi, (m, e) => {
  if (e[0] !== '#') return ENT[e.toLowerCase()];
  const n = e[1] === 'x' || e[1] === 'X' ? parseInt(e.slice(2), 16) : +e.slice(1);
  return n > 0 && n < 0x110000 ? String.fromCodePoint(n) : m;
});
const decode = (v) => (typeof v === 'string' ? unent(v) : Array.isArray(v) ? v.map(decode)
  : v && typeof v === 'object' ? Object.fromEntries(Object.entries(v).map(([k, x]) => [k, decode(x)])) : v);

async function call(addon, resource, type, id, extra) {
  const q = new URLSearchParams({ addon: addon.url, resource, type, id });
  if (extra) q.set('extra', new URLSearchParams(extra).toString());
  return decode(await api('GET', `/api/stremio/addon?${q}`));
}

function mountStremio(win, args) {
  let view = args.restore?.view || { name: 'home' };
  const history = [];
  let data = null;
  let token = 0; // ignores answers for views that were left

  const nav = h('div', { class: 'side-list' });
  const typeNav = h('div', { class: 'side-list' });
  const search = h('input', { class: 'input search-input', type: 'search', placeholder: 'Search films and series', 'aria-label': 'Search' });
  const back = h('button', { class: 'icon-btn', title: 'Back', 'aria-label': 'Back', onclick: () => goBack() }, icon('chevronLeft', 18));
  const content = h('div', { class: 'st-content scroll' });
  win.body.append(h('div', { class: 'app app-stremio' },
    h('aside', { class: 'side' }, h('div', { class: 'side-title' }, 'Stremio'), nav, h('div', { class: 'side-title' }, 'Discover'), typeNav),
    h('div', { class: 'main' }, h('div', { class: 'toolbar' }, back, search), content)));

  search.addEventListener('input', debounce(() => {
    const q = search.value.trim();
    if (q.length >= 2) go({ name: 'search', q }, view.name === 'search');
  }, 450));
  search.addEventListener('keydown', (e) => { if (e.key === 'Enter' && search.value.trim()) go({ name: 'search', q: search.value.trim() }, view.name === 'search'); });

  function go(v, replace = false) {
    if (!replace && view) history.push(view);
    view = v;
    render();
    content.scrollTo(0, 0);
  }
  function goBack() { if (history.length) { view = history.pop(); render(); } }

  function renderNav() {
    const item = (label, ic, v) => h('button', {
      class: `side-item ${view.name === v.name && view.type === v.type ? 'active' : ''}`, onclick: () => { history.length = 0; go(v, true); },
    }, icon(ic, 16), h('span', null, label));
    nav.replaceChildren(
      item('Home', 'home', { name: 'home' }),
      item('Library', 'heart', { name: 'library' }),
      item('Addons', 'puzzle', { name: 'addons' }));
    const types = [...new Set(data.addons.flatMap((a) => (a.manifest.catalogs || []).filter((c) => !requiredOf(c).length).map((c) => c.type)))];
    typeNav.replaceChildren(...types.map((t) => item(typeLabel(t), t === 'series' ? 'tv' : t === 'movie' ? 'film' : 'layers', { name: 'discover', type: t })));
    back.classList.toggle('hidden', !history.length);
  }

  async function render() {
    const my = ++token;
    if (!data) {
      content.replaceChildren(spinner('Loading addons…'));
      data = await store.load().catch((err) => { report(err); return { addons: [], library: [] }; });
      if (my !== token) return;
    }
    renderNav();
    win.setTitle('Stremio');
    const views = { home, discover, search: searchView, detail, addons: addonsView, library };
    (views[view.name] || home)(my);
  }

  const spinner = (text) => h('div', { class: 'empty-state' }, h('span', { class: 'bw-spin big' }), h('div', { class: 'empty-sub' }, text));
  const empty = (title, sub, ic = 'tv') => h('div', { class: 'empty-state' }, icon(ic, 40), h('div', { class: 'empty-title' }, title), sub ? h('div', { class: 'empty-sub' }, sub) : null);

  function poster(m) {
    return h('button', { class: `st-card ${m.posterShape === 'landscape' ? 'wide' : m.posterShape === 'square' ? 'square' : ''}`, title: m.name, onclick: () => go({ name: 'detail', type: m.type, id: m.id }) },
      h('span', { class: 'st-poster' }, m.poster ? h('img', { src: img(m.poster), alt: '', loading: 'lazy', onerror: (e) => e.target.remove() }) : null, h('span', { class: 'st-poster-name' }, m.name)),
      h('span', { class: 'st-card-name' }, m.name),
      h('span', { class: 'st-card-sub muted small' }, [m.releaseInfo || m.year, m.imdbRating ? `★ ${m.imdbRating}` : ''].filter(Boolean).join(' · ')));
  }

  function shelf(title, loader, more) {
    const row = h('div', { class: 'st-row' }, h('div', { class: 'st-row-loading' }, h('span', { class: 'bw-spin' })));
    const el = h('section', { class: 'st-shelf' },
      h('div', { class: 'st-shelf-head' }, h('h3', null, title), more ? h('button', { class: 'btn small', onclick: more }, 'See all', icon('chevronRight', 14)) : null), row);
    loader().then((metas) => {
      if (!metas?.length) { el.remove(); return; }
      row.replaceChildren(...metas.slice(0, 30).map(poster));
    }).catch(() => el.remove());
    return el;
  }

  // ---------- views ----------

  async function home() {
    const sections = [];
    const cont = await continueWatching();
    if (cont.length) sections.push(h('section', { class: 'st-shelf' }, h('div', { class: 'st-shelf-head' }, h('h3', null, 'Continue watching')), h('div', { class: 'st-row' }, cont)));
    for (const a of data.addons) {
      for (const c of (a.manifest.catalogs || []).filter((x) => !requiredOf(x).length).slice(0, 6)) {
        sections.push(shelf(`${c.name || c.id} · ${typeLabel(c.type)}`, async () => (await call(a, 'catalog', c.type, c.id)).metas,
          () => go({ name: 'discover', type: c.type, addon: a.url, catalog: c.id })));
      }
    }
    content.replaceChildren(sections.length ? h('div', { class: 'st-home' }, sections)
      : empty('No catalogs', 'Install an addon with catalogs (Cinemeta is the usual one) in Addons.'));
  }

  async function continueWatching() {
    if (store.account.signedIn) await store.pull();
    const res = await api('GET', '/api/state/media').catch(() => null);
    const local = Object.entries(res?.data?.positions || {}).filter(([k, p]) => k.startsWith('stremio:') && p.src)
      .map(([key, p]) => ({ at: p.at, metaId: p.meta?.id, card: card(p.name, p.poster, p.t, p.d, () => openApp('video', { src: p.src, name: p.name, key, poster: p.poster, meta: p.meta })) }));
    // Titles watched in the official apps (or on a device whose stream link is gone): open the title.
    const remote = store.remote.filter((x) => x.offset > 0 && !local.some((l) => l.metaId === x.id))
      .map((x) => ({ at: Date.parse(x.lastWatched) || 0, card: card(x.name, x.poster, x.offset / 1000, x.duration / 1000,
        () => go({ name: 'detail', type: x.type, id: x.id, video: x.video && x.video !== x.id ? x.video : undefined })) }));
    return [...local, ...remote].sort((a, b) => b.at - a.at).slice(0, 14).map((x) => x.card);
  }

  function card(name, poster, t, d, open) {
    return h('button', { class: 'st-card wide', title: name, onclick: open },
      h('span', { class: 'st-poster' }, poster ? h('img', { src: img(poster), alt: '', loading: 'lazy', onerror: (e) => e.target.remove() }) : null,
        d ? h('span', { class: 'st-progress' }, h('i', { style: { width: `${Math.min(100, (100 * t) / d)}%` } })) : null),
      h('span', { class: 'st-card-name' }, name),
      h('span', { class: 'st-card-sub muted small' }, d ? `${fmtClock(t)} of ${fmtClock(d)}` : 'Watched'));
  }

  async function discover(my) {
    const cats = data.addons.flatMap((a) => (a.manifest.catalogs || []).filter((c) => c.type === view.type && !requiredOf(c).length).map((c) => ({ a, c })));
    if (!cats.length) { content.replaceChildren(empty('Nothing to discover', 'No installed addon has a catalog of this type.')); return; }
    const cur = cats.find((x) => x.a.url === view.addon && x.c.id === view.catalog) || cats[0];
    const genres = extrasOf(cur.c).find((e) => e.name === 'genre')?.options || [];
    const canSkip = extrasOf(cur.c).some((e) => e.name === 'skip');
    const grid = h('div', { class: 'st-grid' });
    const moreBtn = h('button', { class: 'btn', onclick: () => page() }, 'Load more');
    const catSel = h('select', { class: 'input small', 'aria-label': 'Catalog', onchange: (e) => { const x = cats[+e.target.value]; go({ ...view, addon: x.a.url, catalog: x.c.id, genre: '' }, true); } },
      cats.map((x, i) => h('option', { value: i, selected: x === cur }, `${x.c.name || x.c.id} (${x.a.manifest.name})`)));
    const genreSel = genres.length ? h('select', { class: 'input small', 'aria-label': 'Genre', onchange: (e) => go({ ...view, addon: cur.a.url, catalog: cur.c.id, genre: e.target.value }, true) },
      h('option', { value: '' }, 'All genres'), genres.map((g) => h('option', { value: g, selected: g === view.genre }, g))) : null;
    content.replaceChildren(h('div', { class: 'st-page' }, h('div', { class: 'st-filters' }, h('h2', null, typeLabel(view.type)), catSel, genreSel), grid, h('div', { class: 'st-more' }, moreBtn)));
    let skip = 0;
    async function page() {
      moreBtn.disabled = true;
      const extra = {};
      if (view.genre) extra.genre = view.genre;
      if (skip) extra.skip = skip;
      try {
        const metas = (await call(cur.a, 'catalog', cur.c.type, cur.c.id, Object.keys(extra).length ? extra : null)).metas || [];
        if (my !== token) return;
        grid.append(...metas.map(poster));
        skip += metas.length;
        moreBtn.classList.toggle('hidden', !canSkip || !metas.length);
        if (!skip) grid.replaceWith(empty('Nothing here', 'Try another catalog or genre.'));
      } catch (err) { report(err); }
      moreBtn.disabled = false;
    }
    page();
  }

  function searchView() {
    search.value = view.q;
    const cats = data.addons.flatMap((a) => (a.manifest.catalogs || []).filter((c) => extrasOf(c).some((e) => e.name === 'search')).map((c) => ({ a, c })));
    if (!cats.length) { content.replaceChildren(empty('Search is not available', 'No installed addon can search.', 'search')); return; }
    content.replaceChildren(h('div', { class: 'st-home' },
      h('h2', { class: 'st-page-title' }, `Results for “${view.q}”`),
      cats.map(({ a, c }) => shelf(`${typeLabel(c.type)} · ${c.name || a.manifest.name}`, async () => (await call(a, 'catalog', c.type, c.id, { search: view.q })).metas))));
  }

  function library() {
    const items = store.account.signedIn
      ? store.remote.filter((x) => !x.removed && !x.temp).sort((a, b) => String(b.mtime).localeCompare(String(a.mtime)))
      : [...data.library].sort((a, b) => b.added - a.added);
    content.replaceChildren(items.length ? h('div', { class: 'st-page' }, h('h2', { class: 'st-page-title' }, 'Library'), h('div', { class: 'st-grid' }, items.map(poster)))
      : empty('Your library is empty', 'Add films and series with the heart on their page.', 'heart'));
  }

  async function detail(my) {
    content.replaceChildren(spinner('Loading…'));
    const sources = data.addons.filter((a) => supports(a.manifest, 'meta', view.type, view.id));
    let meta = null;
    for (const a of sources) {
      try { meta = (await call(a, 'meta', view.type, view.id)).meta; } catch { /* next addon */ }
      if (meta) break;
    }
    if (my !== token) return;
    if (!meta) { content.replaceChildren(empty('Details unavailable', 'No installed addon knows this title.')); return; }
    win.setTitle(meta.name);
    const inLib = () => store.inLibrary(meta.id);
    const heart = h('button', { class: 'btn', onclick: async () => {
      heart.disabled = true;
      try { await store.setLibrary(meta, meta.type || view.type, !inLib()); } catch (err) { report(err); }
      heart.disabled = false;
      heart.replaceChildren(icon('heart', 15), inLib() ? 'In library' : 'Add to library');
      heart.classList.toggle('primary', inLib());
    } }, icon('heart', 15), inLib() ? 'In library' : 'Add to library');
    heart.classList.toggle('primary', inLib());
    const trailer = (meta.trailerStreams || meta.trailers || []).find((t) => t.ytId || t.source);
    const facts = [meta.releaseInfo || meta.year, meta.runtime, meta.imdbRating ? `★ ${meta.imdbRating}` : '', (meta.genres || meta.genre || []).slice(0, 4).join(', ')].filter(Boolean);
    const people = (label, list) => (list?.length ? h('div', { class: 'st-people' }, h('span', { class: 'muted' }, label), ' ', list.slice(0, 6).join(', ')) : null);
    const streams = h('div', { class: 'st-streams' });
    const episodes = h('div', { class: 'st-episodes' });
    const isSeries = (meta.videos || []).length > 0 && (meta.type || view.type) !== 'movie';

    content.replaceChildren(h('div', { class: 'st-detail' },
      meta.background ? h('div', { class: 'st-backdrop', style: { backgroundImage: `url("${img(meta.background)}")` } }) : null,
      h('div', { class: 'st-detail-body' },
        h('div', { class: 'st-detail-top' },
          meta.poster ? h('img', { class: 'st-detail-poster', src: img(meta.poster), alt: '' }) : null,
          h('div', { class: 'st-detail-info' },
            meta.logo ? h('img', { class: 'st-logo', src: img(meta.logo), alt: meta.name, onerror: (e) => e.target.replaceWith(h('h1', null, meta.name)) }) : h('h1', null, meta.name),
            h('div', { class: 'st-facts' }, facts.map((f) => h('span', null, f))),
            meta.description ? h('p', { class: 'st-desc' }, meta.description) : null,
            people('Director', meta.director), people('Cast', meta.cast),
            h('div', { class: 'hero-actions' },
              !isSeries ? h('button', { class: 'btn primary', onclick: () => streams.scrollIntoView({ behavior: 'smooth' }) }, icon('play', 15), 'Watch') : null,
              heart,
              trailer ? h('button', { class: 'btn', onclick: () => openApp('browser', { open: `https://www.youtube.com/watch?v=${trailer.ytId || trailer.source}` }) }, icon('film', 15), 'Trailer') : null))),
        isSeries ? episodes : null,
        streams)));

    const ctx = { meta, type: meta.type || view.type };
    if (isSeries) renderEpisodes(episodes, streams, ctx, my);
    else loadStreams(streams, ctx, { id: meta.id, title: meta.name }, my);
  }

  function renderEpisodes(box, streams, ctx, my) {
    const vids = ctx.meta.videos.filter((v) => v.id);
    const seasons = [...new Set(vids.map((v) => v.season ?? 0))].sort((a, b) => (a || 99) - (b || 99));
    let season = view.season ?? seasons.find((s) => s > 0) ?? seasons[0];
    const draw = () => {
      const list = vids.filter((v) => (v.season ?? 0) === season).sort((a, b) => (a.episode || a.number || 0) - (b.episode || b.number || 0));
      box.replaceChildren(
        h('div', { class: 'st-shelf-head' }, h('h3', null, 'Episodes'),
          h('select', { class: 'input small', 'aria-label': 'Season', onchange: (e) => { season = +e.target.value; view.season = season; draw(); } },
            seasons.map((s) => h('option', { value: s, selected: s === season }, s ? `Season ${s}` : 'Specials')))),
        h('div', { class: 'st-episode-list' }, list.map((v) => {
          const n = v.episode || v.number;
          const aired = v.released && new Date(v.released) > new Date();
          return h('button', { class: `st-episode ${view.video === v.id ? 'active' : ''}`, disabled: aired, onclick: () => {
            view.video = v.id;
            draw();
            loadStreams(streams, ctx, { id: v.id, title: `S${season}E${n}${v.title || v.name ? ` · ${v.title || v.name}` : ''}`, thumbnail: v.thumbnail }, my);
            streams.scrollIntoView({ behavior: 'smooth' });
          } },
          v.thumbnail ? h('img', { src: img(v.thumbnail), alt: '', loading: 'lazy', onerror: (e) => e.target.remove() }) : h('span', { class: 'st-ep-num' }, n ?? '·'),
          h('span', { class: 'st-ep-text' }, h('span', { class: 'st-ep-title' }, `${n ? `${n}. ` : ''}${v.title || v.name || 'Episode'}`),
            h('span', { class: 'muted small' }, aired ? `Airs ${new Date(v.released).toLocaleDateString()}` : v.released ? new Date(v.released).toLocaleDateString() : '')));
        })));
    };
    draw();
    const chosen = vids.find((v) => v.id === view.video);
    if (chosen) loadStreams(streams, ctx, { id: chosen.id, title: `S${chosen.season}E${chosen.episode || chosen.number}${chosen.title ? ` · ${chosen.title}` : ''}` }, my);
    else streams.replaceChildren(h('div', { class: 'muted st-hint' }, 'Choose an episode to see where to watch it.'));
  }

  async function loadStreams(box, ctx, video, my) {
    const addons = data.addons.filter((a) => supports(a.manifest, 'stream', ctx.type, video.id));
    if (!addons.length) {
      box.replaceChildren(h('div', { class: 'st-no-streams' }, icon('puzzle', 20),
        h('div', null, h('strong', null, 'No stream addons installed'),
          h('div', { class: 'muted small' }, 'Cinemeta only describes titles. Install an addon that provides streams (for example one for public-domain films, or your own media server) in Addons.')),
        h('button', { class: 'btn small', onclick: () => go({ name: 'addons' }) }, 'Addons')));
      return;
    }
    box.replaceChildren(h('div', { class: 'st-shelf-head' }, h('h3', null, `Streams${video.title && video.id !== ctx.meta.id ? ` · ${video.title}` : ''}`)));
    for (const a of addons) {
      const group = h('div', { class: 'st-stream-group' }, h('div', { class: 'st-stream-addon' }, a.manifest.logo ? h('img', { src: img(a.manifest.logo), alt: '' }) : icon('puzzle', 14), a.manifest.name),
        h('div', { class: 'st-row-loading' }, h('span', { class: 'bw-spin' })));
      box.append(group);
      call(a, 'stream', ctx.type, video.id).then((res) => {
        if (my !== token) return;
        const list = (res.streams || []).filter((s) => s.url || s.infoHash || s.ytId || s.externalUrl);
        group.lastChild.replaceWith(list.length ? h('div', { class: 'st-stream-list' }, list.map((s) => streamRow(s, ctx, video))) : h('div', { class: 'muted small st-hint' }, 'No streams from this addon.'));
      }).catch((err) => group.lastChild.replaceWith(h('div', { class: 'muted small st-hint' }, err.message)));
    }
  }

  function streamRow(s, ctx, video) {
    const lines = (s.title || s.description || '').split('\n').filter(Boolean);
    const kind = s.infoHash ? 'Torrent' : s.url ? 'Web' : s.ytId ? 'YouTube' : 'Link';
    const canSave = s.infoHash || s.url;
    return h('div', { class: 'st-stream', role: 'button', tabindex: '0', onclick: () => playStream(s, ctx, video), onkeydown: (e) => { if (e.key === 'Enter') playStream(s, ctx, video); } },
      h('span', { class: 'st-stream-name' }, (s.name || kind).split('\n').map((l) => h('span', null, l))),
      h('span', { class: 'st-stream-title' }, lines.length ? lines.map((l) => h('span', null, l)) : h('span', { class: 'muted' }, kind)),
      h('span', { class: 'pill' }, kind),
      canSave ? h('button', { class: 'icon-btn', title: 'Download to the server', 'aria-label': 'Download to the server', onclick: (e) => { e.stopPropagation(); saveStream(s, ctx, video); } }, icon('download', 16)) : h('span'),
      icon('play', 16));
  }

  const titleOf = (ctx, video) => (video.id === ctx.meta.id ? ctx.meta.name : `${ctx.meta.name} · ${video.title}`);
  function magnetOf(s, name) {
    const trackers = (s.sources || []).filter((x) => x.startsWith('tracker:')).map((x) => `&tr=${encodeURIComponent(x.slice(8))}`).join('');
    return `magnet:?xt=urn:btih:${s.infoHash}&dn=${encodeURIComponent(s.behaviorHints?.filename || name)}${trackers}`;
  }

  // saveStream downloads a stream onto the server: torrents into Files → Torrents (just the
  // chosen file of a season pack), web streams into Files → Downloads.
  async function saveStream(s, ctx, video) {
    const name = titleOf(ctx, video);
    try {
      if (s.infoHash) {
        await api('POST', '/api/torrents', { magnet: magnetOf(s, name), file: Number.isInteger(s.fileIdx) ? s.fileIdx : -1 });
        notice(`Downloading “${name}” on the server, into Files → Torrents`, [{ label: 'Show', primary: true, run: () => openApp('torrents') }, { label: 'OK' }], 'download');
      } else {
        await api('POST', '/api/downloads', { url: s.url, name: s.behaviorHints?.filename || name });
        notice(`Downloading “${name}” on the server, into Files → Downloads`, [{ label: 'Show', primary: true, run: () => openApp('torrents', { filter: 'web' }) }, { label: 'OK' }], 'download');
      }
    } catch (err) { report(err); }
  }

  async function subtitlesFor(type, id) {
    const addons = data.addons.filter((a) => supports(a.manifest, 'subtitles', type, id));
    const all = (await Promise.all(addons.map((a) => call(a, 'subtitles', type, id).then((r) => r.subtitles || []).catch(() => [])))).flat();
    const rank = (s) => { const i = wantLangs.indexOf(s.lang); return i < 0 ? 99 : i; };
    const seen = new Map();
    for (const s of all.filter((x) => x.url).sort((a, b) => rank(a) - rank(b))) {
      const n = seen.get(s.lang) || [];
      if (n.length < (rank(s) < 99 ? 3 : 1)) { n.push(s); seen.set(s.lang, n); }
    }
    return [...seen.values()].flat().slice(0, 16).map((s) => ({ lang: s.lang, url: s.url }));
  }

  async function playStream(s, ctx, video) {
    const name = titleOf(ctx, video);
    const key = `stremio:${ctx.type}:${video.id}`;
    if (s.ytId) { openApp('browser', { open: `https://www.youtube.com/watch?v=${s.ytId}` }); return; }
    if (s.externalUrl && !s.url && !s.infoHash) { openApp('browser', { open: s.externalUrl }); return; }
    const subtitles = await subtitlesFor(ctx.type, video.id).catch(() => []);
    // meta lets the player report progress to the Stremio account (continue watching everywhere).
    const meta = { id: ctx.meta.id, type: ctx.type, name: ctx.meta.name, poster: ctx.meta.poster, video: video.id };
    const base = { name, key, poster: video.thumbnail || ctx.meta.background || ctx.meta.poster, subtitles, meta };
    if (s.url) { openApp('video', { ...base, src: `url:${s.url}` }); return; }
    try {
      const { hash } = await api('POST', '/api/torrents', { magnet: magnetOf(s, name), stream: true });
      openApp('video', { ...base, src: `torrent:${hash}:${Number.isInteger(s.fileIdx) ? s.fileIdx : -1}` });
    } catch (err) { report(err); }
  }

  // ---------- addons ----------

  function addonsView() {
    const input = h('input', { class: 'input', type: 'url', placeholder: 'https://…/manifest.json or stremio://…', 'aria-label': 'Addon address' });
    const install = async (e) => {
      e.preventDefault();
      if (!input.value.trim()) return;
      try {
        const a = await api('POST', '/api/stremio/manifest', { url: input.value });
        data.addons = data.addons.filter((x) => x.url !== a.url && x.manifest.id !== a.manifest.id).concat(a);
        store.saveAddons();
        toast(`Installed ${a.manifest.name}`);
        input.value = '';
        render();
      } catch (err) { report(err); }
    };
    const remove = async (a) => {
      if (!(await confirmDialog(`Remove ${a.manifest.name}?`, 'Its catalogs and streams will no longer appear.', 'Remove'))) return;
      data.addons = data.addons.filter((x) => x !== a);
      store.saveAddons();
      render();
    };
    const resNames = (m) => (m.resources || []).map((r) => (typeof r === 'string' ? r : r.name));
    // merge adds addons that are not installed yet (by address or by addon id).
    const merge = (list) => {
      let n = 0;
      for (const a of list) {
        if (data.addons.some((x) => x.url === a.url || x.manifest.id === a.manifest?.id)) continue;
        data.addons.push(a);
        n++;
      }
      if (n) store.saveAddons();
      return n;
    };
    const signIn = () => {
      const email = h('input', { class: 'input', type: 'email', autocomplete: 'username', 'aria-label': 'Stremio email' });
      const pass = h('input', { class: 'input', type: 'password', autocomplete: 'current-password', 'aria-label': 'Stremio password' });
      const status = h('div', { class: 'form-error' });
      const submit = async () => {
        status.textContent = '';
        try {
          store.account = await api('POST', '/api/stremio/account/login', { email: email.value.trim(), password: pass.value });
          close();
          // Addons from both sides: the account's, plus any installed here first.
          const mine = data.addons;
          const { addons } = await api('GET', '/api/stremio/account/addons');
          data.addons = addons;
          const n = merge(mine);
          if (!n) store.save();
          await store.pull();
          toast(`Signed in as ${store.account.email}; addons, library and progress now sync`);
          render();
        } catch (err) { status.textContent = err.message; }
      };
      const close = dialog({
        title: 'Sign in to Stremio',
        body: h('form', { onsubmit: (e) => { e.preventDefault(); submit(); } },
          h('p', { class: 'dialog-text' }, 'Keeps your addons, library and continue watching in sync with the Stremio apps. Your password is only used to sign in; the server keeps the session, which you can end here any time.'),
          h('label', { class: 'field-label' }, 'Email'), email, h('label', { class: 'field-label' }, 'Password'), pass, status,
          h('button', { type: 'submit', class: 'hidden' })),
        actions: [h('button', { class: 'btn', onclick: () => close() }, 'Cancel'), h('button', { class: 'btn primary', onclick: submit }, 'Sign in')],
      });
      requestAnimationFrame(() => email.focus());
    };
    const signOut = async () => {
      if (!(await confirmDialog('Sign out of Stremio?', 'Addons stay installed here; changes stop syncing with your account.', 'Sign out', false))) return;
      await api('POST', '/api/stremio/account/logout').catch(report);
      store.account = { signedIn: false };
      store.remote = [];
      render();
    };
    const accountBox = store.account.signedIn
      ? h('div', { class: 'st-account card' }, icon('user', 18),
        h('div', null, h('strong', null, store.account.email), h('div', { class: 'muted small' }, 'Stremio account · addons, library and progress sync')),
        h('button', { class: 'btn small', onclick: async () => { await store.pull(); toast('Synced with Stremio'); render(); } }, icon('refresh', 14), 'Sync now'),
        h('button', { class: 'btn small', onclick: signOut }, 'Sign out'))
      : h('div', { class: 'st-account card' }, icon('user', 18),
        h('div', null, h('strong', null, 'Stremio account'), h('div', { class: 'muted small' }, 'Sign in to sync addons, library and continue watching with the Stremio apps.')),
        h('button', { class: 'btn primary small', onclick: signIn }, 'Sign in'));
    const fromLinks = () => {
      const area = h('textarea', { class: 'input st-links', rows: 6, placeholder: 'One addon address per line (https://…/manifest.json or stremio://…)', 'aria-label': 'Addon addresses' });
      const status = h('div', { class: 'form-error' });
      const go = async () => {
        const urls = area.value.split(/\s+/).map((x) => x.trim()).filter(Boolean);
        if (!urls.length) return;
        const got = await Promise.all(urls.map((u) => api('POST', '/api/stremio/manifest', { url: u }).catch((err) => ({ err: `${u.slice(0, 60)}: ${err.message}` }))));
        const bad = got.filter((x) => x.err);
        const n = merge(got.filter((x) => !x.err));
        if (bad.length) { status.textContent = `${bad.length} could not be added. ${bad[0].err}`; } else close();
        if (n) { toast(`Added ${n} addon${n === 1 ? '' : 's'}`); render(); }
      };
      const close = dialog({
        title: 'Add addons from links',
        body: h('div', null, h('p', { class: 'dialog-text' }, 'In Stremio, open Addons, then use “Copy link” (the share button) on each addon you want here.'), area, status),
        actions: [h('button', { class: 'btn', onclick: () => close() }, 'Cancel'), h('button', { class: 'btn primary', onclick: go }, 'Add')],
      });
      requestAnimationFrame(() => area.focus());
    };
    content.replaceChildren(h('div', { class: 'st-page' },
      h('h2', { class: 'st-page-title' }, 'Addons'),
      accountBox,
      h('form', { class: 'st-install', onsubmit: install }, input, h('button', { class: 'btn primary', type: 'submit' }, icon('plus', 15), 'Install')),
      h('div', { class: 'hero-actions st-import' },
        h('button', { class: 'btn', onclick: fromLinks }, icon('link', 15), 'Paste several links')),
      h('p', { class: 'muted small' }, 'Addons are web services; ultraspace fetches catalogs, streams and subtitles from them through the server. Only install addons you trust, and only use streams you have the right to watch.'),
      h('div', { class: 'st-addons' }, data.addons.map((a) => h('div', { class: 'st-addon card' },
        a.manifest.logo ? h('img', { class: 'st-addon-logo', src: img(a.manifest.logo), alt: '', onerror: (e) => e.target.replaceWith(h('span', { class: 'st-addon-logo' }, icon('puzzle', 22))) }) : h('span', { class: 'st-addon-logo' }, icon('puzzle', 22)),
        h('div', { class: 'st-addon-text' },
          h('div', null, h('strong', null, a.manifest.name), h('span', { class: 'muted small' }, ` v${a.manifest.version || '?'}`)),
          a.manifest.description ? h('div', { class: 'muted small st-addon-desc' }, a.manifest.description) : null,
          h('div', { class: 'st-addon-tags' }, resNames(a.manifest).map((r) => h('span', { class: 'pill' }, r)), (a.manifest.types || []).map((t) => h('span', { class: 'pill' }, typeLabel(t))))),
        h('button', { class: 'icon-btn', title: 'More', 'aria-label': `Options for ${a.manifest.name}`, onclick: (e) => {
          const r = e.currentTarget.getBoundingClientRect();
          menu(r.left - 140, r.bottom + 4, [
            { label: 'Copy address', icon: 'link', action: () => navigator.clipboard.writeText(a.url).then(() => toast('Address copied')) },
            a.protected ? null : { label: 'Remove', icon: 'trash', danger: true, action: () => remove(a) },
          ]);
        } }, icon('moreH', 16)))))));
  }

  render();

  return {
    onArgs(a) { if (a.open) go({ name: 'detail', type: a.open.type, id: a.open.id }); },
    saveState: () => ({ view }),
  };
}
