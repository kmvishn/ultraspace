// A small Markdown renderer for Notes. Everything is HTML-escaped first, links are
// limited to http(s) and mailto, and task-list boxes carry their source line number
// so the preview can tick them.

const esc = (s) => s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');

function safeUrl(u) {
  const t = u.trim();
  return /^(https?:|mailto:)/i.test(t) ? t : null;
}

// inline formats one line of already-split text.
function inline(src) {
  const codes = [];
  // Pull code spans out before escaping, so their text is escaped exactly once.
  let s = esc(src.replace(/`([^`]+)`/g, (_, c) => { codes.push(c); return `\u0000${codes.length - 1}\u0000`; }));
  s = s
    .replace(/\[([^\]]+)\]\(([^)\s]+)\)/g, (m, text, url) => {
      const u = safeUrl(url.replace(/&amp;/g, '&'));
      return u ? `<a href="${esc(u)}" target="_blank" rel="noopener noreferrer">${text}</a>` : m;
    })
    .replace(/(^|[\s(])(https?:\/\/[^\s<)]+)/g, (_, pre, url) => `${pre}<a href="${url}" target="_blank" rel="noopener noreferrer">${url}</a>`)
    .replace(/\*\*([^*]+)\*\*/g, '<strong>$1</strong>')
    .replace(/__([^_]+)__/g, '<strong>$1</strong>')
    .replace(/(^|[^*])\*([^*\s][^*]*)\*/g, '$1<em>$2</em>')
    .replace(/(^|[^\w])_([^_\s][^_]*)_(?!\w)/g, '$1<em>$2</em>')
    .replace(/~~([^~]+)~~/g, '<del>$1</del>')
    .replace(/==([^=]+)==/g, '<mark>$1</mark>');
  return s.replace(/\u0000(\d+)\u0000/g, (_, i) => `<code>${esc(codes[Number(i)])}</code>`);
}

export function renderMarkdown(text) {
  const lines = text.replace(/\r\n?/g, '\n').split('\n');
  const out = [];
  let i = 0;
  let para = [];
  const flush = () => { if (para.length) { out.push(`<p>${para.map(inline).join('<br>')}</p>`); para = []; } };

  while (i < lines.length) {
    const line = lines[i];
    let m;
    if ((m = line.match(/^(\s*)(```|~~~)\s*([\w+-]*)\s*$/))) {
      flush();
      const fence = m[2];
      const body = [];
      i++;
      while (i < lines.length && !lines[i].trim().startsWith(fence)) body.push(lines[i++]);
      i++;
      out.push(`<pre><code${m[3] ? ` data-lang="${esc(m[3])}"` : ''}>${esc(body.join('\n'))}</code></pre>`);
      continue;
    }
    if (!line.trim()) { flush(); i++; continue; }
    if ((m = line.match(/^(#{1,6})\s+(.*)$/))) { flush(); out.push(`<h${m[1].length}>${inline(m[2])}</h${m[1].length}>`); i++; continue; }
    if (/^\s*([-*_])(\s*\1){2,}\s*$/.test(line)) { flush(); out.push('<hr>'); i++; continue; }
    if (/^\s*>/.test(line)) {
      flush();
      const body = [];
      while (i < lines.length && /^\s*>/.test(lines[i])) body.push(lines[i++].replace(/^\s*>\s?/, ''));
      out.push(`<blockquote>${renderMarkdown(body.join('\n'))}</blockquote>`);
      continue;
    }
    if (/^\s*([-*+]|\d+[.)])\s+/.test(line)) {
      flush();
      const ordered = /^\s*\d+[.)]/.test(line);
      const items = [];
      while (i < lines.length && /^\s*([-*+]|\d+[.)])\s+/.test(lines[i])) {
        const raw = lines[i];
        const indent = raw.match(/^\s*/)[0].length;
        const content = raw.replace(/^\s*([-*+]|\d+[.)])\s+/, '');
        const task = content.match(/^\[([ xX])\]\s+(.*)$/);
        const nested = indent >= 2 ? ' class="nested"' : '';
        items.push(task
          ? `<li class="task${indent >= 2 ? ' nested' : ''}"><input type="checkbox" data-line="${i}"${task[1] !== ' ' ? ' checked' : ''}><span>${inline(task[2])}</span></li>`
          : `<li${nested}>${inline(content)}</li>`);
        i++;
      }
      out.push(ordered ? `<ol>${items.join('')}</ol>` : `<ul>${items.join('')}</ul>`);
      continue;
    }
    if (/^\|.*\|\s*$/.test(line) && i + 1 < lines.length && /^\|?\s*:?-{2,}/.test(lines[i + 1])) {
      flush();
      const cells = (l) => l.trim().replace(/^\||\|$/g, '').split('|').map((c) => inline(c.trim()));
      const head = cells(line);
      i += 2;
      const rows = [];
      while (i < lines.length && /^\|.*\|\s*$/.test(lines[i])) rows.push(cells(lines[i++]));
      out.push(`<table><thead><tr>${head.map((c) => `<th>${c}</th>`).join('')}</tr></thead><tbody>${rows.map((r) => `<tr>${r.map((c) => `<td>${c}</td>`).join('')}</tr>`).join('')}</tbody></table>`);
      continue;
    }
    para.push(line);
    i++;
  }
  flush();
  return out.join('\n');
}

// toggleTask flips the "[ ]" / "[x]" on source line n.
export function toggleTask(text, n, on) {
  const lines = text.split('\n');
  if (lines[n] !== undefined) lines[n] = lines[n].replace(/\[([ xX])\]/, on ? '[x]' : '[ ]');
  return lines.join('\n');
}

// titleOf is a note's first heading or first line.
export function titleOf(text) {
  const first = text.split('\n').find((l) => l.trim()) || '';
  return first.replace(/^#+\s*/, '').replace(/[*_`~]/g, '').trim().slice(0, 120);
}
