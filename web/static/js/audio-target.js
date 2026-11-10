// Audio Target Manager: multi-device audio routing (Browser + Server Sinks)
import { h, icon, api, emit, on, toast, report, closeMenu } from './core.js';
import { openApp } from './wm.js';

export function getAudioOutputs() {
  try {
    const raw = localStorage.getItem('us.audio_outputs');
    if (raw) {
      const parsed = JSON.parse(raw);
      if (typeof parsed.browser === 'boolean' && Array.isArray(parsed.serverSinks)) {
        if (!parsed.browser && parsed.serverSinks.length === 0) {
          parsed.browser = true;
        }
        return parsed;
      }
    }
  } catch {}

  return { browser: true, serverSinks: [] };
}

export function saveAudioOutputs(outputs) {
  // Ensure at least one output device is active
  if (!outputs.browser && (!outputs.serverSinks || outputs.serverSinks.length === 0)) {
    outputs.browser = true;
  }
  localStorage.setItem('us.audio_outputs', JSON.stringify(outputs));
  localStorage.setItem('us.audio_target', outputs.browser && outputs.serverSinks.length === 0 ? 'browser' : 'server');
  emit('audio-outputs-changed', outputs);
}

export async function toggleBrowserOutput() {
  const current = getAudioOutputs();
  current.browser = !current.browser;
  if (!current.browser && current.serverSinks.length === 0) {
    toast('At least one audio output must be enabled');
    return current;
  }
  saveAudioOutputs(current);
  return current;
}

export async function toggleServerSink(sinkName) {
  const current = getAudioOutputs();
  const idx = current.serverSinks.indexOf(sinkName);
  if (idx >= 0) {
    current.serverSinks.splice(idx, 1);
  } else {
    current.serverSinks.push(sinkName);
  }

  if (!current.browser && current.serverSinks.length === 0) {
    toast('At least one audio output must be enabled');
    return current;
  }

  try {
    if (current.serverSinks.length > 0) {
      await api('POST', '/api/audio/sinks', { sinks: current.serverSinks });
    }
  } catch (err) {
    report(err);
  }

  saveAudioOutputs(current);
  return current;
}

export async function setAllServerSinks(sinkNames) {
  const current = getAudioOutputs();
  current.serverSinks = [...sinkNames];
  if (!current.browser && current.serverSinks.length === 0) {
    current.browser = true;
  }
  try {
    if (current.serverSinks.length > 0) {
      await api('POST', '/api/audio/sinks', { sinks: current.serverSinks });
    }
  } catch (err) {
    report(err);
  }
  saveAudioOutputs(current);
  return current;
}

// openAudioOutputsPopover opens an interactive checklist popover allowing
// users to toggle multiple devices (Browser, Groove Max, Built-in Speaker, HDMI)
// simultaneously without prematurely closing.
let activeAudioPopover = null;

export function closeAudioOutputsPopover() {
  if (activeAudioPopover) {
    activeAudioPopover.remove();
    activeAudioPopover = null;
  }
}

document.addEventListener('pointerdown', (e) => {
  if (activeAudioPopover && !activeAudioPopover.contains(e.target) && !e.target.closest('.p-target') && !e.target.closest('.mb-sound-btn')) {
    closeAudioOutputsPopover();
  }
}, true);

export async function openAudioOutputsPopover(anchorEl) {
  closeMenu();
  closeAudioOutputsPopover();

  const r = anchorEl.getBoundingClientRect();
  const popover = h('div', { class: 'menu audio-target-popover glass', role: 'dialog', 'aria-label': 'Audio Output Devices', style: { minWidth: '260px', padding: '8px' } });
  activeAudioPopover = popover;

  let st = null;
  try {
    st = await api('GET', '/api/audio/status');
  } catch {}

  function renderItems() {
    const outputs = getAudioOutputs();

    // If server sinks are empty in saved state but server has active sinks, populate from server
    if (outputs.serverSinks.length === 0 && st?.active_sinks?.length) {
      // Don't override if user explicitly unchecked all server sinks
    }

    const items = [];

    // Header
    items.push(h('div', { style: { display: 'flex', alignItems: 'center', justifyContent: 'space-between', padding: '6px 8px 8px', borderBottom: '1px solid var(--border)', marginBottom: '6px' } },
      h('span', { style: { fontSize: '12px', fontWeight: '600', textTransform: 'uppercase', letterSpacing: '0.04em', opacity: '0.8' } }, 'Audio Output Devices'),
      h('button', {
        class: 'icon-btn small', 'aria-label': 'Close', onclick: (e) => { e.stopPropagation(); closeAudioOutputsPopover(); },
      }, icon('x', 14)),
    ));

    // Browser output row
    const isBrowserOn = !!outputs.browser;
    items.push(h('button', {
      class: `menu-item ${isBrowserOn ? 'active' : ''}`,
      style: { display: 'flex', alignItems: 'center', justifyContent: 'space-between', width: '100%' },
      onclick: async (e) => {
        e.stopPropagation();
        await toggleBrowserOutput();
        renderItems();
      },
    },
    h('div', { style: { display: 'flex', alignItems: 'center', gap: '8px' } },
      icon('laptop', 16),
      h('span', { class: 'menu-label' }, 'This Device (Browser)')),
    h('span', { style: { color: isBrowserOn ? 'var(--accent)' : 'var(--muted)', display: 'flex', alignItems: 'center' } },
      icon(isBrowserOn ? 'check' : 'minus', 15))));

    // Server Hardware Sinks
    if (st?.available && st.sinks?.length) {
      items.push(h('div', { class: 'menu-sep', style: { margin: '6px 0' } }));
      items.push(h('div', { style: { fontSize: '11px', fontWeight: '500', opacity: '0.6', padding: '4px 8px' } }, 'Server Speakers & Bluetooth'));

      for (const s of st.sinks) {
        let ic = 'volume';
        if (s.type === 'bluetooth') ic = 'bluetooth';
        else if (s.type === 'hdmi') ic = 'tv';
        else if (s.type === 'usb') ic = 'sliders';

        const isSinkOn = outputs.serverSinks.includes(s.name);
        items.push(h('button', {
          class: `menu-item ${isSinkOn ? 'active' : ''}`,
          style: { display: 'flex', alignItems: 'center', justifyContent: 'space-between', width: '100%' },
          onclick: async (e) => {
            e.stopPropagation();
            await toggleServerSink(s.name);
            renderItems();
          },
        },
        h('div', { style: { display: 'flex', alignItems: 'center', gap: '8px' } },
          icon(ic, 16),
          h('span', { class: 'menu-label' }, s.description || s.name)),
        h('span', { style: { color: isSinkOn ? 'var(--accent)' : 'var(--muted)', display: 'flex', alignItems: 'center' } },
          icon(isSinkOn ? 'check' : 'minus', 15))));
      }
    }

    // Footer link to Settings
    items.push(h('div', { class: 'menu-sep', style: { margin: '6px 0' } }));
    items.push(h('button', {
      class: 'menu-item',
      onclick: (e) => {
        e.stopPropagation();
        closeAudioOutputsPopover();
        openApp('settings', { section: 'sound' });
      },
    }, icon('sliders', 15), h('span', { class: 'menu-label' }, 'Sound & Audio Settings…')));

    popover.replaceChildren(...items);
  }

  renderItems();
  document.body.append(popover);

  const popRect = popover.getBoundingClientRect();
  const left = Math.max(8, Math.min(r.left, window.innerWidth - popRect.width - 8));
  // If anchored from bottom player bar, place above anchor; if top menubar, place below anchor
  if (r.top > window.innerHeight / 2) {
    popover.style.top = `${Math.max(8, r.top - popRect.height - 6)}px`;
  } else {
    popover.style.top = `${Math.min(window.innerHeight - popRect.height - 8, r.bottom + 6)}px`;
  }
  popover.style.left = `${left}px`;
  popover.style.position = 'fixed';
  popover.style.zIndex = '9999';

  return popover;
}

