// Photos: every image and video in Files as a timeline, grouped by the month it was
// taken (from the photo's EXIF date, else the file date). Thumbnails come from the server.
import { h, icon, api, report, toast, menu, confirmDialog, settings, fileUrl, thumbUrl, downloadFile, dirOf, baseOf, debounce } from '../core.js';
import { registerApp, openApp } from '../wm.js';
import { uploadFiles } from './files.js';

registerApp({
  id: 'photos', name: 'Photos', icon: 'image', color: ['#f472b6', '#c026d3'], width: 1080, height: 720, minWidth: 420,
  mount(win, args) { return mountPhotos(win, args); },
});

const SIZES = { s: 110, m: 160, l: 240 };
const PAGE = 240; // tiles added per scroll step
const monthFmt = new Intl.DateTimeFormat(undefined, { month: 'long', year: 'numeric' });
const dayFmt = new Intl.DateTimeFormat(undefined, { dateStyle: 'medium', timeStyle: 'short' });

function mountPhotos(win, args) {
  let all = [];
  let view = args.restore?.view || { type: 'all' };
  let query = '';
  let size = 'm';
  try { size = localStorage.getItem('us.photos.size') || 'm'; } catch { /* default */ }
  let shown = [];
  let rendered = 0;
  let observer = null;

  const nav = h('div', { class: 'side-list' });
  const folders = h('div', { class: 'side-list' });
  const content = h('div', { class: 'photos-content scroll' });
  const search = h('input', { class: 'input search-input', type: 'search', placeholder: 'Search by name', 'aria-label': 'Search photos' });
  const count = h('span', { class: 'muted small' });
  const sizeSeg = h('div', { class: 'seg' });
  const input = h('input', { type: 'file', multiple: true, accept: 'image/*,video/*', class: 'hidden', onchange: () => { upload([...input.files]); input.value = ''; } });

  win.body.append(h('div', { class: 'app app-photos' },
    h('aside', { class: 'side' }, h('div', { class: 'side-title' }, 'Library'), nav, h('div', { class: 'side-title' }, 'Folders'), folders),
    h('div', { class: 'main' },
      h('div', { class: 'toolbar' }, search, count, h('div', { class: 'tb-spacer' }), sizeSeg,
        h('button', { class: 'icon-btn', title: 'Refresh', 'aria-label': 'Refresh', onclick: () => load() }, icon('refresh', 16)),
        h('button', { class: 'btn primary small', onclick: () => input.click() }, icon('upload', 15), 'Upload'), input),
      content)));

  async function load() {
    content.replaceChildren(h('div', { class: 'empty-state' }, h('span', { class: 'bw-spin big' }), h('div', { class: 'empty-sub' }, 'Finding your photos…')));
    try {
      all = (await api('GET', '/api/files/photos')).photos.map((p) => ({ ...p, when: new Date(p.taken) }));
    } catch (err) { report(err); all = []; }
    render();
  }

  function filtered() {
    let list = all;
    if (view.type === 'photos') list = list.filter((p) => !p.video);
    else if (view.type === 'videos') list = list.filter((p) => p.video);
    else if (view.type === 'folder') list = list.filter((p) => dirOf(p.path) === view.dir);
    if (query) list = list.filter((p) => p.name.toLowerCase().includes(query));
    return list;
  }

  function render() {
    const item = (label, ic, v, n) => h('button', {
      class: `side-item ${view.type === v.type && view.dir === v.dir ? 'active' : ''}`, onclick: () => { view = v; render(); content.scrollTo(0, 0); },
    }, icon(ic, 16), h('span', null, label), h('span', { class: 'side-count' }, n));
    nav.replaceChildren(
      item('All', 'layers', { type: 'all' }, all.length),
      item('Photos', 'image', { type: 'photos' }, all.filter((p) => !p.video).length),
      item('Videos', 'film', { type: 'videos' }, all.filter((p) => p.video).length));
    const dirs = new Map();
    for (const p of all) { const d = dirOf(p.path); dirs.set(d, (dirs.get(d) || 0) + 1); }
    folders.replaceChildren(...[...dirs].sort((a, b) => a[0].localeCompare(b[0])).map(([d, n]) =>
      item(d === '/' ? 'Home' : baseOf(d), 'folder', { type: 'folder', dir: d }, n)));
    sizeSeg.replaceChildren(...Object.keys(SIZES).map((k) => h('button', {
      class: `seg-btn ${size === k ? 'active' : ''}`, title: { s: 'Small', m: 'Medium', l: 'Large' }[k],
      onclick: () => { size = k; try { localStorage.setItem('us.photos.size', k); } catch { /* ignore */ } render(); },
    }, icon('grid', { s: 12, m: 15, l: 18 }[k]))));
    content.style.setProperty('--tile', `${SIZES[size]}px`);

    shown = filtered();
    count.textContent = shown.length ? `${shown.length.toLocaleString()} item${shown.length === 1 ? '' : 's'}` : '';
    win.setTitle(view.type === 'folder' ? `Photos — ${baseOf(view.dir) || 'Home'}` : 'Photos');
    observer?.disconnect();
    rendered = 0;
    if (!all.length) {
      content.replaceChildren(h('div', { class: 'empty-state' }, icon('image', 44), h('div', { class: 'empty-title' }, 'No photos yet'),
        h('div', { class: 'empty-sub' }, 'Upload photos here, or put images anywhere in Files. Drop files onto this window to upload them to Photos.'),
        h('button', { class: 'btn primary', onclick: () => input.click() }, icon('upload', 15), 'Upload photos')));
      return;
    }
    if (!shown.length) { content.replaceChildren(h('div', { class: 'empty-state small' }, 'Nothing matches')); return; }
    content.replaceChildren();
    more();
  }

  // Tiles are added in pages as you scroll, so big libraries stay fast.
  function more() {
    const sentinel = content.querySelector('.photos-sentinel');
    sentinel?.remove();
    const end = Math.min(shown.length, rendered + PAGE);
    let section = content.lastElementChild?.classList.contains('photos-month') ? content.lastElementChild : null;
    for (let i = rendered; i < end; i++) {
      const p = shown[i];
      const key = `${p.when.getFullYear()}-${p.when.getMonth()}`;
      if (!section || section.dataset.key !== key) {
        const n = shown.filter((x) => `${x.when.getFullYear()}-${x.when.getMonth()}` === key).length;
        section = h('section', { class: 'photos-month', dataset: { key } },
          h('h3', { class: 'photos-month-head' }, monthFmt.format(p.when), h('span', { class: 'muted' }, ` · ${n}`)),
          h('div', { class: 'photos-grid' }));
        content.append(section);
      }
      section.lastElementChild.append(tile(p, i));
    }
    rendered = end;
    if (rendered < shown.length) {
      const s = h('div', { class: 'photos-sentinel' });
      content.append(s);
      observer?.disconnect();
      observer = new IntersectionObserver((entries) => { if (entries.some((e) => e.isIntersecting)) more(); }, { root: content, rootMargin: '600px' });
      observer.observe(s);
    }
  }

  function tile(p, i) {
    const big = SIZES[size] > 180;
    const body = p.video
      ? h('img', {
        src: thumbUrl(p.path, big ? 512 : 256), loading: 'lazy', decoding: 'async', alt: p.name, draggable: 'false',
        onerror: (e) => e.target.replaceWith(h('span', { class: 'photo-video' }, icon('film', 26), h('span', { class: 'photo-video-name' }, p.name))),
      })
      : h('img', {
        src: thumbUrl(p.path, big ? 512 : 256), loading: 'lazy', decoding: 'async', alt: p.name, draggable: 'false',
        onerror: (e) => e.target.replaceWith(h('span', { class: 'photo-video' }, icon('image', 24))),
      });
    return h('button', {
      class: `photo-tile ${p.video ? 'is-video' : ''}`, title: `${p.name}\n${dayFmt.format(p.when)}`,
      onclick: () => openViewer(i),
      oncontextmenu: (e) => { e.preventDefault(); tileMenu(e, p, i); },
    }, body, p.video ? h('span', { class: 'photo-badge' }, icon('play', 10)) : null);
  }

  function openViewer(i) {
    if (shown[i].video) {
      openApp('video', { path: shown[i].path, siblings: shown.filter((p) => p.video).slice(0, 500).map((p) => p.path) });
      return;
    }
    // The viewer steps through the current view; keep the list it gets reasonable.
    const from = Math.max(0, i - 1000);
    const siblings = shown.slice(from, from + 2000).map((p) => p.path);
    openApp('viewer', { path: shown[i].path, siblings });
  }

  function tileMenu(e, p, i) {
    menu(e.clientX, e.clientY, [
      { label: 'Open', icon: 'image', action: () => openViewer(i) },
      { label: 'Show in Files', icon: 'folder', action: () => openApp('files', { path: dirOf(p.path) }) },
      p.video ? null : { label: 'Set as wallpaper', icon: 'wallpaper', action: () => { settings.set({ wallpaper: `file:${p.path}` }); toast('Wallpaper updated'); } },
      { label: 'Download', icon: 'download', action: () => downloadFile(p.path) },
      '-',
      { label: 'Delete', icon: 'trash', danger: true, action: () => remove(p) },
    ]);
  }

  async function remove(p) {
    if (!(await confirmDialog('Delete this photo?', `"${p.name}" will be deleted from Files.`, 'Delete'))) return;
    try {
      await api('POST', '/api/files/delete', { path: p.path });
      all = all.filter((x) => x.path !== p.path);
      render();
    } catch (err) { report(err); }
  }

  async function upload(files) {
    const media = files.filter((f) => /^(image|video)\//.test(f.type));
    if (!media.length) return;
    try {
      await api('POST', '/api/files/mkdir', { path: '/Photos' }).catch((err) => { if (err.status !== 409) throw err; });
      await uploadFiles('/Photos', media);
      toast(`Uploaded ${media.length} to Photos`);
      load();
    } catch (err) { report(err); }
  }

  win.body.addEventListener('dragover', (e) => { if (e.dataTransfer?.types.includes('Files')) { e.preventDefault(); content.classList.add('dragover'); } });
  win.body.addEventListener('dragleave', (e) => { if (!win.body.contains(e.relatedTarget)) content.classList.remove('dragover'); });
  win.body.addEventListener('drop', (e) => {
    if (!e.dataTransfer?.files.length) return;
    e.preventDefault();
    content.classList.remove('dragover');
    upload([...e.dataTransfer.files]);
  });
  search.addEventListener('input', debounce(() => { query = search.value.trim().toLowerCase(); render(); }, 150));

  load();
  return {
    saveState: () => ({ view }),
    onDestroy() { observer?.disconnect(); },
  };
}
