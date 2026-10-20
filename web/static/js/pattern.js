// A 3x3 pattern pad, as on Android phones. Dots are numbered 0-8, left to right and
// top to bottom; passing over a dot that was skipped joins it too.
import { h } from './core.js';

const NS = 'http://www.w3.org/2000/svg';

export function patternPad({ onDone } = {}) {
  let seq = [];
  let drawing = false;
  let pointer = null;
  let locked = false;

  const svg = document.createElementNS(NS, 'svg');
  svg.setAttribute('viewBox', '0 0 300 300');
  svg.setAttribute('class', 'pp-svg');
  const lines = document.createElementNS(NS, 'polyline');
  lines.setAttribute('class', 'pp-line');
  const live = document.createElementNS(NS, 'line');
  live.setAttribute('class', 'pp-line live');
  live.style.display = 'none';
  svg.append(lines, live);
  const dots = [];
  for (let i = 0; i < 9; i++) {
    const g = document.createElementNS(NS, 'g');
    const cx = 50 + (i % 3) * 100, cy = 50 + Math.floor(i / 3) * 100;
    const halo = document.createElementNS(NS, 'circle');
    halo.setAttribute('cx', cx); halo.setAttribute('cy', cy); halo.setAttribute('r', 26); halo.setAttribute('class', 'pp-halo');
    const dot = document.createElementNS(NS, 'circle');
    dot.setAttribute('cx', cx); dot.setAttribute('cy', cy); dot.setAttribute('r', 9); dot.setAttribute('class', 'pp-dot');
    g.append(halo, dot);
    svg.append(g);
    dots.push({ g, cx, cy });
  }
  const el = h('div', { class: 'pp', role: 'group', 'aria-label': 'Pattern' }, svg);

  const toSvg = (e) => {
    const r = svg.getBoundingClientRect();
    return { x: ((e.clientX - r.left) / r.width) * 300, y: ((e.clientY - r.top) / r.height) * 300 };
  };

  function add(i) {
    if (seq.includes(i)) return;
    const last = seq[seq.length - 1];
    if (last !== undefined) {
      // Crossing a skipped dot in a straight line joins it (0→2 passes 1, 0→8 passes 4…).
      const [r1, c1, r2, c2] = [Math.floor(last / 3), last % 3, Math.floor(i / 3), i % 3];
      if ((r1 + r2) % 2 === 0 && (c1 + c2) % 2 === 0) {
        const mid = ((r1 + r2) / 2) * 3 + (c1 + c2) / 2;
        if (!seq.includes(mid)) seq.push(mid);
      }
    }
    seq.push(i);
    navigator.vibrate?.(8);
  }

  function hit(p) {
    dots.forEach((d, i) => { if (Math.hypot(p.x - d.cx, p.y - d.cy) < 30) add(i); });
  }

  function paint() {
    dots.forEach((d, i) => d.g.classList.toggle('on', seq.includes(i)));
    lines.setAttribute('points', seq.map((i) => `${dots[i].cx},${dots[i].cy}`).join(' '));
    const last = seq[seq.length - 1];
    if (drawing && pointer && last !== undefined) {
      live.setAttribute('x1', dots[last].cx); live.setAttribute('y1', dots[last].cy);
      live.setAttribute('x2', pointer.x); live.setAttribute('y2', pointer.y);
      live.style.display = '';
    } else {
      live.style.display = 'none';
    }
  }

  svg.addEventListener('pointerdown', (e) => {
    if (locked) return;
    e.preventDefault();
    try { svg.setPointerCapture(e.pointerId); } catch { /* synthetic or already released */ }
    el.classList.remove('error', 'ok');
    seq = [];
    drawing = true;
    pointer = toSvg(e);
    hit(pointer);
    paint();
  });
  svg.addEventListener('pointermove', (e) => {
    if (!drawing) return;
    pointer = toSvg(e);
    hit(pointer);
    paint();
  });
  const end = () => {
    if (!drawing) return;
    drawing = false;
    paint();
    if (seq.length) onDone?.([...seq]);
  };
  svg.addEventListener('pointerup', end);
  svg.addEventListener('pointercancel', end);

  return {
    el,
    clear() { seq = []; el.classList.remove('error', 'ok'); locked = false; paint(); },
    error() {
      el.classList.remove('ok');
      el.classList.add('error');
      navigator.vibrate?.([30, 40, 30]);
      locked = true;
      setTimeout(() => { if (el.classList.contains('error')) { seq = []; el.classList.remove('error'); locked = false; paint(); } }, 900);
    },
    ok() { el.classList.add('ok'); locked = true; },
    unlock() { locked = false; },
  };
}
