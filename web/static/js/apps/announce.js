// Intercom: see this machine's camera, speak to its speakers, and listen to its microphone.
import { h, icon, api, report, toast, ApiError } from '../core.js';
import { registerApp } from '../wm.js';

registerApp({
  id: 'announce', name: 'Intercom', icon: 'video', color: ['#fdba74', '#f97316'], width: 960, height: 720,
  mount(win) { return mountAnnounce(win); },
});

const MAX = 400;
const RECORD_MS = 60000;

function mountAnnounce(win) {
  if (win.rect && !win.maximized && win.rect.h < 680) {
    const area = win.area();
    win.rect.w = Math.min(Math.max(win.rect.w, 960), area.w - 16);
    win.rect.h = Math.min(720, area.h - 16);
    win.clamp();
  }
  const picked = new Set();
  let sinks = [];
  let voices = [];
  let voiceId = '';
  let voiceOpen = false;
  let voiceQuery = '';
  let mics = [];
  let micName = '';
  let recorder = null;
  let chunks = [];
  let stream = null;
  let recordTimer = 0;
  let busy = false;
  let listening = false;

  const text = h('textarea', { maxlength: String(MAX), placeholder: 'Type an announcement', 'aria-label': 'Announcement' });
  const kicker = h('p', { class: 'kicker' }, 'Announcement');
  const count = h('span', { class: 'count' }, '0 / ' + MAX);
  const speed = h('input', { type: 'range', class: 'range', min: '80', max: '260', step: '10', value: '160', 'aria-label': 'Speaking speed' });
  const speedLabel = h('span', { class: 'count' }, '160 wpm');
  const speak = h('button', { class: 'btn primary', type: 'button' }, icon('megaphone', 15), 'Speak');
  const record = h('button', { class: 'btn', type: 'button' }, icon('mic', 15), 'Record');
  const speakers = h('div', { class: 'stack' });
  const msg = h('p', { class: 'msg' });
  const voiceName = h('span', { class: 'label' }, 'Default');
  const voiceMeta = h('span', { class: 'set-desc' }, 'Reads your message');
  const voiceBtn = h('button', { type: 'button', class: 'device', 'aria-expanded': 'false', 'aria-haspopup': 'listbox' },
    h('span', { class: 'device-icon' }, icon('megaphone', 18)),
    h('span', { class: 'device-text' }, h('span', { class: 'device-name' }, voiceName), voiceMeta),
    icon('chevronDown', 16, 'voice-caret'));
  const voiceSearch = h('input', { class: 'input', type: 'search', placeholder: 'Search voices', 'aria-label': 'Search voices' });
  const voiceScroll = h('div', { class: 'voice-scroll', role: 'listbox' });
  const voicePop = h('div', { class: 'voice-pop hidden' }, voiceSearch, voiceScroll);
  const voiceAnchor = h('div', { class: 'voice-anchor' }, voiceBtn);
  const micList = h('div', { class: 'stack' });
  const listenBtn = h('button', { class: 'btn', type: 'button' }, icon('mic', 15), 'Listen');
  const talkNote = h('p', { class: 'msg' });
  const player = h('audio');
  const cam = h('img', { class: 'cam', alt: 'Camera' });
  const camCap = h('p', { class: 'cam-cap' });
  const camBox = h('div', { class: 'cam-box hidden' }, cam, camCap);
  const camList = h('div', { class: 'stack hidden' });
  let cameras = [];
  let camId = '';

  speak.onclick = () => sendText();
  record.onclick = () => toggleRecord();
  listenBtn.onclick = () => toggleListen();
  voiceBtn.onclick = () => {
    voiceOpen = !voiceOpen;
    renderVoice();
    if (voiceOpen) voiceSearch.focus();
  };
  voiceSearch.addEventListener('input', () => { voiceQuery = voiceSearch.value; renderVoice(); });
  voiceSearch.addEventListener('keydown', (e) => {
    if (e.key !== 'Escape') return;
    voiceOpen = false;
    renderVoice();
    voiceBtn.focus();
  });
  speed.addEventListener('input', () => {
    const span = Number(speed.max) - Number(speed.min);
    speed.style.setProperty('--fill', ((Number(speed.value) - Number(speed.min)) / span * 100) + '%');
    speedLabel.textContent = speed.value + ' wpm';
  });
  speed.dispatchEvent(new Event('input'));
  text.addEventListener('input', () => {
    count.textContent = text.value.length + ' / ' + MAX;
    syncButtons();
  });
  document.addEventListener('pointerdown', (e) => {
    if (!voiceOpen || voiceAnchor.contains(e.target) || voicePop.contains(e.target)) return;
    voiceOpen = false;
    renderVoice();
  });

  win.body.append(h('div', { class: 'app app-announce' },
    h('aside', { class: 'side' },
      h('div', { class: 'stack' }, h('p', { class: 'kicker' }, 'Speakers'), speakers),
      h('div', { class: 'stack' },
        h('p', { class: 'kicker' }, 'Talk'),
        h('p', { class: 'lead' }, 'Listen here while you announce. Both can run together.'),
        micList, listenBtn, talkNote)),
    h('section', { class: 'compose' },
      camBox,
      camList,
      h('div', { class: 'head' }, kicker, count),
      text,
      voicePop,
      voiceAnchor,
      h('label', { class: 'speed' }, h('span', { class: 'kicker' }, 'Speed'), speed, speedLabel),
      h('div', { class: 'row' }, speak, record),
      msg),
    player));

  cam.onerror = () => {
    if (!camId) return;
    camCap.textContent = 'Could not open the camera.';
  };

  function currentVoice() {
    return voices.find((v) => v.id === voiceId) || null;
  }

  function sinkKind(s) {
    if (s.type === 'bluetooth') return 'Bluetooth';
    if (s.type === 'hdmi') return 'HDMI';
    return 'Speaker';
  }

  function voiceDetail(v) {
    if (!v.gender) return 'Reads your message';
    return v.gender[0].toUpperCase() + v.gender.slice(1);
  }

  function voiceRow(v, label) {
    const on = v.id === voiceId;
    return h('button', {
      type: 'button',
      class: 'voice-row' + (on ? ' current' : ''),
      role: 'option',
      'aria-selected': on ? 'true' : 'false',
      onclick: () => {
        voiceId = v.id;
        voiceOpen = false;
        voiceQuery = '';
        voiceSearch.value = '';
        renderVoice();
      },
    },
    h('span', { class: 'name' }, label),
    on ? icon('check', 15) : null);
  }

  function syncButtons() {
    const ready = picked.size > 0 && !busy && !recorder;
    speak.disabled = !ready || !text.value.trim();
    record.disabled = picked.size === 0 || busy;
    listenBtn.disabled = !micName;
  }

  function renderSpeakers() {
    speakers.replaceChildren();
    if (!sinks.length) {
      speakers.append(h('div', { class: 'empty-state small' }, 'No speakers found.'));
      syncButtons();
      return;
    }
    for (const s of sinks) {
      const on = picked.has(s.name);
      speakers.append(h('button', {
        type: 'button',
        class: 'device' + (on ? ' current' : ''),
        'aria-pressed': on ? 'true' : 'false',
        onclick: () => {
          if (picked.has(s.name)) picked.delete(s.name);
          else picked.add(s.name);
          renderSpeakers();
        },
      },
      h('span', { class: 'device-icon' }, icon(s.type === 'bluetooth' ? 'bluetooth' : s.type === 'hdmi' ? 'tv' : 'volume', 18)),
      h('span', { class: 'device-text' },
        h('span', { class: 'device-name' },
          h('span', { class: 'label' }, s.description || s.name),
          on ? h('span', { class: 'pill active' }, 'On') : null),
        h('span', { class: 'set-desc' }, sinkKind(s)))));
    }
    syncButtons();
  }

  function renderVoice() {
    const current = currentVoice();
    voiceName.textContent = current ? current.name : 'Default';
    voiceMeta.textContent = current ? voiceDetail(current) : 'Reads your message';
    voiceBtn.setAttribute('aria-expanded', voiceOpen ? 'true' : 'false');
    voiceAnchor.classList.toggle('open', voiceOpen);
    voicePop.classList.toggle('hidden', !voiceOpen);
    text.classList.toggle('hidden', voiceOpen);
    count.classList.toggle('hidden', voiceOpen);
    kicker.textContent = voiceOpen ? 'Voice' : 'Announcement';
    if (!voiceOpen) return;
    const q = voiceQuery.trim().toLowerCase();
    const shown = voices.filter((v) => !q || v.name.toLowerCase().includes(q));
    voiceScroll.replaceChildren();
    voiceScroll.classList.toggle('searching', !!q);
    if (!shown.length) {
      voiceScroll.append(h('div', { class: 'voice-empty' }, 'No matching voice'));
      return;
    }
    if (q) {
      voiceScroll.append(h('div', { class: 'voice-grid' }, ...shown.map((v) => voiceRow(v, v.name))));
      return;
    }
    const women = shown.filter((v) => v.gender === 'female');
    const men = shown.filter((v) => v.gender !== 'female');
    voiceScroll.append(
      h('section', { class: 'voice-col' }, h('div', { class: 'voice-head' }, 'Women'), h('div', { class: 'voice-grid' }, ...women.map((v) => voiceRow(v, v.name)))),
      h('section', { class: 'voice-col' }, h('div', { class: 'voice-head' }, 'Men'), h('div', { class: 'voice-grid' }, ...men.map((v) => voiceRow(v, v.name)))),
    );
  }

  function renderMics() {
    micList.replaceChildren();
    if (!mics.length) {
      listenBtn.classList.add('hidden');
      micList.append(h('div', { class: 'empty-state small' }, 'No microphone on this machine.'));
      syncButtons();
      return;
    }
    listenBtn.classList.remove('hidden');
    for (const m of mics) {
      const on = m.name === micName;
      micList.append(h('button', {
        type: 'button',
        class: 'device' + (on ? ' current' : ''),
        'aria-pressed': on ? 'true' : 'false',
        onclick: () => {
          if (listening && micName !== m.name) stopListen();
          micName = m.name;
          renderMics();
        },
      },
      h('span', { class: 'device-icon' }, icon('mic', 18)),
      h('span', { class: 'device-text' },
        h('span', { class: 'device-name' },
          h('span', { class: 'label' }, m.description || m.name),
          on ? h('span', { class: 'pill active' }, 'On') : null),
        h('span', { class: 'set-desc' }, 'Microphone'))));
    }
    syncButtons();
  }

  async function load() {
    try {
      const [st, voiceList, micRes, camRes] = await Promise.all([
        api('GET', '/api/audio/status'),
        api('GET', '/api/audio/announce/voices').catch(() => ({ voices: [] })),
        api('GET', '/api/audio/announce/mics').catch(() => ({ mics: [] })),
        api('GET', '/api/audio/announce/cameras').catch(() => ({ cameras: [] })),
      ]);
      sinks = (st && st.sinks) || [];
      voices = voiceList.voices || [];
      mics = micRes.mics || [];
      cameras = camRes.cameras || [];
      if (!voiceId && voices.length) {
        const preferred = voices.find((v) => v.name === 'Adam') || voices[0];
        voiceId = preferred.id;
      }
      if (!picked.size) {
        for (const s of sinks) if (s.active) picked.add(s.name);
        if (!picked.size && sinks[0]) picked.add(sinks[0].name);
      }
      if (!micName && mics[0]) micName = mics[0].name;
      renderSpeakers();
      renderVoice();
      renderMics();
      renderCameras();
    } catch (err) {
      report(err);
    }
  }

  async function sendText() {
    const message = text.value.trim();
    if (!message || !picked.size || busy) return;
    busy = true;
    syncButtons();
    msg.textContent = 'Speaking…';
    try {
      await api('POST', '/api/audio/announce/text', { text: message, voice: voiceId, sinks: [...picked], speed: Number(speed.value) });
      msg.textContent = 'Played.';
      toast('Announcement played');
    } catch (err) {
      msg.textContent = err.message || 'Could not speak.';
      if (!(err instanceof ApiError)) report(err);
    } finally {
      busy = false;
      syncButtons();
    }
  }

  async function toggleRecord() {
    if (recorder) {
      recorder.stop();
      return;
    }
    if (!picked.size || busy) return;
    if (!navigator.mediaDevices || !navigator.mediaDevices.getUserMedia) {
      msg.textContent = 'Open this page on localhost or HTTPS to use this browser’s microphone.';
      toast(msg.textContent, 'error');
      return;
    }
    let media;
    try {
      media = await navigator.mediaDevices.getUserMedia({ audio: true });
    } catch (err) {
      msg.textContent = 'This browser’s microphone is not available.';
      toast(err.message || 'Microphone unavailable', 'error');
      return;
    }
    stream = media;
    chunks = [];
    const mime = MediaRecorder.isTypeSupported('audio/webm') ? 'audio/webm' : '';
    recorder = mime ? new MediaRecorder(media, { mimeType: mime }) : new MediaRecorder(media);
    recorder.ondataavailable = (e) => { if (e.data && e.data.size) chunks.push(e.data); };
    recorder.onstop = () => finishRecording();
    recorder.start();
    record.replaceChildren(icon('square', 15), 'Stop and send');
    msg.textContent = 'Recording…';
    syncButtons();
    recordTimer = setTimeout(() => { if (recorder) recorder.stop(); }, RECORD_MS);
  }

  async function finishRecording() {
    clearTimeout(recordTimer);
    const rec = recorder;
    const blob = new Blob(chunks, { type: (rec && rec.mimeType) || 'audio/webm' });
    recorder = null;
    chunks = [];
    if (stream) {
      stream.getTracks().forEach((t) => t.stop());
      stream = null;
    }
    record.replaceChildren(icon('mic', 15), 'Record');
    if (!blob.size) {
      msg.textContent = 'The recording was empty.';
      syncButtons();
      return;
    }
    busy = true;
    syncButtons();
    msg.textContent = 'Playing the recording…';
    try {
      const body = new FormData();
      body.append('audio', blob, 'announcement.webm');
      for (const name of picked) body.append('sink', name);
      const res = await fetch('/api/audio/announce/voice', {
        method: 'POST',
        headers: { 'X-Ultraspace': '1' },
        credentials: 'same-origin',
        body,
      });
      const data = (res.headers.get('Content-Type') || '').includes('application/json') ? await res.json() : null;
      if (!res.ok) throw new ApiError((data && data.error) || 'Could not play the recording', res.status);
      msg.textContent = 'Played.';
      toast('Announcement played');
    } catch (err) {
      msg.textContent = err.message || 'Could not play the recording.';
      if (!(err instanceof ApiError)) report(err);
    } finally {
      busy = false;
      syncButtons();
    }
  }

  function stopListen() {
    listening = false;
    player.pause();
    player.removeAttribute('src');
    player.load();
    listenBtn.replaceChildren(icon('mic', 15), 'Listen');
    renderMics();
  }

  player.onerror = () => {
    if (!listening) return;
    stopListen();
    talkNote.textContent = 'Could not open the microphone.';
  };

  function openCamera(id) {
    if (!id) {
      camId = '';
      cam.removeAttribute('src');
      camBox.classList.add('hidden');
      camCap.textContent = '';
      return;
    }
    if (camId === id && cam.getAttribute('src')) return;
    camId = id;
    const found = cameras.find((c) => c.id === id);
    camBox.classList.remove('hidden');
    camCap.textContent = found ? found.name : '';
    cam.src = '/api/audio/announce/camera?device=' + encodeURIComponent(id);
  }

  function renderCameras() {
    camList.replaceChildren();
    if (!cameras.length) {
      camList.classList.add('hidden');
      openCamera('');
      return;
    }
    if (!cameras.some((c) => c.id === camId)) camId = '';
    const next = camId || cameras[0].id;
    openCamera(next);
    camList.classList.toggle('hidden', cameras.length < 2);
    for (const c of cameras) {
      const on = c.id === camId;
      camList.append(h('button', {
        type: 'button',
        class: 'device' + (on ? ' current' : ''),
        'aria-pressed': on ? 'true' : 'false',
        onclick: () => {
          if (camId === c.id) return;
          cam.removeAttribute('src');
          camId = '';
          openCamera(c.id);
          renderCameras();
        },
      },
      h('span', { class: 'device-icon' }, icon('video', 18)),
      h('span', { class: 'device-text' },
        h('span', { class: 'device-name' }, h('span', { class: 'label' }, c.name)),
        h('span', { class: 'set-desc' }, on ? 'Showing' : 'Camera'))));
    }
  }

  function toggleListen() {
    if (listening) {
      stopListen();
      talkNote.textContent = '';
      return;
    }
    if (!micName) return;
    listening = true;
    listenBtn.replaceChildren(icon('square', 15), 'Stop');
    talkNote.textContent = 'Listening…';
    renderMics();
    player.src = '/api/audio/announce/listen?source=' + encodeURIComponent(micName);
    player.play().catch(() => {
      stopListen();
      talkNote.textContent = 'Could not open the microphone.';
    });
  }

  load();
  syncButtons();
  return {
    onClose() {
      cam.removeAttribute('src');
      if (listening) stopListen();
    },
  };
}
