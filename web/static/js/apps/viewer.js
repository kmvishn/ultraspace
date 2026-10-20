import { h, icon, toast, settings, kindOf, fileUrl, baseOf, downloadFile } from '../core.js';
import { registerApp } from '../wm.js';

registerApp({
  id: 'viewer', name: 'Viewer', icon: 'image', color: ['#fbbf24', '#d97706'], width: 960, height: 640, dock: false,
  mount(win, args) { return mountViewer(win, args); },
});

function mountViewer(win, args) {
  let list = [];
  let i = 0;
  const stage = h('div', { class: 'viewer-stage' });
  const counter = h('span', { class: 'viewer-count' });
  const prevBtn = h('button', { class: 'viewer-nav prev', 'aria-label': 'Previous', onclick: () => go(-1) }, icon('chevronLeft', 26));
  const nextBtn = h('button', { class: 'viewer-nav next', 'aria-label': 'Next', onclick: () => go(1) }, icon('chevronRight', 26));
  const wpBtn = h('button', { class: 'icon-btn', title: 'Set as wallpaper', 'aria-label': 'Set as wallpaper', onclick: () => { settings.set({ wallpaper: `file:${list[i]}` }); toast('Wallpaper updated'); } }, icon('wallpaper', 16));
  win.actionsEl.append(counter, wpBtn,
    h('button', { class: 'icon-btn', title: 'Download', 'aria-label': 'Download', onclick: () => downloadFile(list[i]) }, icon('download', 16)));
  win.body.append(h('div', { class: 'app app-viewer' }, stage, prevBtn, nextBtn));

  function show() {
    const p = list[i];
    if (!p) return;
    const kind = kindOf({ name: baseOf(p) });
    const media = kind === 'video'
      ? h('video', { src: fileUrl(p, true), controls: true, autoplay: true, playsinline: true })
      : h('img', { src: fileUrl(p, true), alt: baseOf(p), draggable: 'false' });
    stage.replaceChildren(media);
    win.setTitle(baseOf(p));
    counter.textContent = list.length > 1 ? `${i + 1} / ${list.length}` : '';
    prevBtn.classList.toggle('hidden', list.length < 2);
    nextBtn.classList.toggle('hidden', list.length < 2);
    wpBtn.classList.toggle('hidden', kind !== 'image');
    // Warm the cache for the next image.
    const n = list[(i + 1) % list.length];
    if (n && n !== p && kindOf({ name: baseOf(n) }) === 'image') new Image().src = fileUrl(n, true);
  }

  function go(d) {
    if (list.length < 2) return;
    i = (i + d + list.length) % list.length;
    show();
  }

  function setArgs(a) {
    list = a.siblings && a.siblings.includes(a.path) ? a.siblings : [a.path];
    i = Math.max(0, list.indexOf(a.path));
    show();
  }

  win.body.tabIndex = -1;
  win.body.addEventListener('keydown', (e) => {
    if (e.key === 'ArrowLeft') go(-1);
    else if (e.key === 'ArrowRight') go(1);
  });
  setArgs(args.restore?.path ? args.restore : args);
  return {
    onArgs: setArgs,
    onFocus() { win.body.focus({ preventScroll: true }); },
    saveState: () => ({ path: list[i], siblings: list.length <= 400 ? list : [list[i]] }),
  };
}
