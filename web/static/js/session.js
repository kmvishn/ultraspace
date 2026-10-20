// Workspace sync: the open windows and what each one shows are saved on the server,
// so signing in on any device picks up where you left off.
import { api, notice, toast, fmtAgo } from './core.js';
import { openApp, appById, allWindows, closeAllWindows } from './wm.js';

const KEY = '/api/state/workspace';
const SAVE_EVERY = 4000;

export const device = { id: deviceId(), name: deviceName(navigator.userAgent) };

let last = '';        // JSON of the workspace as last saved (or loaded)
let known = 0;        // server timestamp of the newest workspace we have seen
let timer = null;
let checking = false;
let lastCheck = 0;
let prompt = null;

function deviceId() {
  try {
    let id = localStorage.getItem('us.device');
    if (!id) { id = crypto.randomUUID(); localStorage.setItem('us.device', id); }
    return id;
  } catch {
    return `tmp-${Math.random().toString(36).slice(2)}`;
  }
}

// "Chrome on Windows", "Safari on iPhone" and so on; good enough to recognise a device.
export function deviceName(ua) {
  const browser = /Edg\//.test(ua) ? 'Edge' : /OPR\//.test(ua) ? 'Opera' : /Firefox\//.test(ua) ? 'Firefox'
    : /Chrome\//.test(ua) ? 'Chrome' : /Safari\//.test(ua) ? 'Safari' : 'Browser';
  const os = /iPhone/.test(ua) ? 'iPhone' : /iPad/.test(ua) ? 'iPad' : /Android/.test(ua) ? 'Android'
    : /Windows/.test(ua) ? 'Windows' : /Mac OS X/.test(ua) ? 'Mac' : /CrOS/.test(ua) ? 'ChromeOS' : /Linux/.test(ua) ? 'Linux' : 'unknown';
  return `${browser} on ${os}`;
}

export function capture() {
  return {
    v: 1,
    windows: allWindows().map((w) => ({
      app: w.app.id,
      state: (w.handle.saveState && w.handle.saveState()) || {},
      geom: w.geom(),
      focused: w.focused,
    })),
  };
}

function restore(data) {
  const list = (data && Array.isArray(data.windows) ? data.windows : []).filter((w) => appById(w.app));
  let focus = null;
  for (const w of list) {
    try {
      const win = openApp(w.app, { restore: w.state || {} }, { geom: w.geom || {} });
      if (w.focused && !w.geom?.min) focus = win;
    } catch (err) {
      console.error('restore window', w.app, err);
    }
  }
  focus?.focus();
  return list.length;
}

async function save({ keepalive = false } = {}) {
  if (prompt) return; // waiting for the user to choose between this device and another
  const json = JSON.stringify(capture());
  if (json === last) return;
  last = json;
  const body = `{"device":${JSON.stringify(device.id)},"device_name":${JSON.stringify(device.name)},"base":${known},"data":${json}}`;
  try {
    const res = await fetch(KEY, {
      method: 'PUT', credentials: 'same-origin', keepalive: keepalive && body.length < 60_000,
      headers: { 'X-Ultraspace': '1', 'Content-Type': 'application/json' }, body,
    });
    if (res.ok) known = (await res.json()).updated || known;
    else {
      last = ''; // try again next tick
      if (res.status === 409) { lastCheck = 0; checkOtherDevice(); }
    }
  } catch {
    last = '';
  }
}

// When this tab comes back into view, offer the session another device saved meanwhile.
async function checkOtherDevice() {
  if (checking || prompt || !timer || Date.now() - lastCheck < 5000) return;
  checking = true;
  lastCheck = Date.now();
  try {
    const res = await api('GET', KEY);
    if (!res.data || res.updated <= known || res.device === device.id) return;
    const theirs = JSON.stringify(res.data);
    known = res.updated;
    if (theirs === last) return;
    prompt = notice(`You continued on ${res.device_name || 'another device'} ${fmtAgo(res.updated)}. Switch to that session?`, [
      { label: 'Keep this one', run: () => { prompt = null; last = ''; save(); } },
      {
        label: 'Switch', primary: true,
        run: () => { prompt = null; closeAllWindows(); restore(res.data); last = JSON.stringify(capture()); },
      },
    ], 'laptop');
  } catch { /* offline or signed out; try again later */ } finally {
    checking = false;
  }
}

const onVisible = () => { if (document.visibilityState === 'visible') checkOtherDevice(); else save({ keepalive: true }); };
const onHide = () => save({ keepalive: true });

// Called once the desktop is on screen.
export async function startSession() {
  stopSession();
  let res = null;
  try { res = await api('GET', KEY); } catch { /* start empty */ }
  known = res?.updated || 0;
  if (res?.data && restore(res.data) && res.device && res.device !== device.id) {
    toast(`Picked up where you left off on ${res.device_name || 'another device'}`);
  }
  // Apps finish restoring asynchronously; their saveState() returns the restored state until then.
  last = JSON.stringify(capture());
  timer = setInterval(() => { if (!document.hidden) save(); }, SAVE_EVERY);
  document.addEventListener('visibilitychange', onVisible);
  addEventListener('pagehide', onHide);
  addEventListener('focus', checkOtherDevice);
}

// Saves now; used before signing out so nothing is lost.
export async function flushSession() {
  if (timer) await save();
}

export function stopSession() {
  clearInterval(timer);
  timer = null;
  prompt?.();
  prompt = null;
  document.removeEventListener('visibilitychange', onVisible);
  removeEventListener('pagehide', onHide);
  removeEventListener('focus', checkOtherDevice);
}
