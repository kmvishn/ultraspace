// Unattended Lock Screen with idle watchdog, video/audio playback inhibitor,
// PIN keypad + password fallback, and customizable lockscreen widgets.
import {
  h, icon, api, on, emit, settings, system, fmtBytes, fmtDuration,
} from './core.js';
import { applyAppearance } from './desktop.js';

let activeLockEl = null;
let watchdogTimer = null;
let lastActivity = Date.now();
let currentUser = null;
let onSignOutHandler = null;
let currentMusic = null;

// Track music state broadcast by the music player
on('music', (m) => {
  currentMusic = m;
});

// Check whether any media is currently playing to inhibit auto-lock
export function isMediaInhibitingLock() {
  // Check any video elements in the document
  const videos = document.querySelectorAll('video');
  for (const v of videos) {
    if (!v.paused && !v.ended && v.currentTime > 0 && v.readyState >= 2) {
      return true;
    }
  }
  // Check any audio elements in the document
  const audios = document.querySelectorAll('audio');
  for (const a of audios) {
    if (!a.paused && !a.ended && a.currentTime > 0) {
      return true;
    }
  }
  // Check music app state
  if (currentMusic && currentMusic.playing) {
    return true;
  }
  return false;
}

// Reset idle timer on user activity
function recordActivity() {
  lastActivity = Date.now();
}

export function initLockScreen(user, onSignOut) {
  currentUser = user;
  onSignOutHandler = onSignOut;
  lastActivity = Date.now();

  // Listen to user interaction events to track idle state
  const events = ['pointerdown', 'pointermove', 'keydown', 'touchstart', 'wheel'];
  let throttle = 0;
  const onActivity = () => {
    const now = Date.now();
    if (now - throttle > 1000) {
      throttle = now;
      recordActivity();
    }
  };

  events.forEach((ev) => window.addEventListener(ev, onActivity, { passive: true }));

  // Key combination to immediately lock the screen: Ctrl+Alt+L or Meta+Alt+L
  const onGlobalKey = (e) => {
    if ((e.ctrlKey || e.metaKey) && e.altKey && e.key.toLowerCase() === 'l') {
      e.preventDefault();
      lockDesktop();
    }
  };
  window.addEventListener('keydown', onGlobalKey);

  // Watchdog interval running every 2 seconds
  if (watchdogTimer) clearInterval(watchdogTimer);
  watchdogTimer = setInterval(() => {
    if (activeLockEl) return; // already locked
    const timeoutSec = Number(settings.data.lockTimeout ?? 300);
    if (!timeoutSec || timeoutSec <= 0) return; // auto-lock disabled

    const idleSeconds = (Date.now() - lastActivity) / 1000;
    if (idleSeconds >= timeoutSec) {
      if (isMediaInhibitingLock()) {
        // Media is actively playing, postpone locking
        lastActivity = Date.now();
        return;
      }
      lockDesktop();
    }
  }, 2000);

  // Check if session was locked prior to page reload
  if (sessionStorage.getItem('us.locked') === '1') {
    lockDesktop();
  }

  return () => {
    if (watchdogTimer) clearInterval(watchdogTimer);
    events.forEach((ev) => window.removeEventListener(ev, onActivity));
    window.removeEventListener('keydown', onGlobalKey);
  };
}

export function lockDesktop() {
  if (activeLockEl) return;
  sessionStorage.setItem('us.locked', '1');

  const s = settings.data;
  const lockEl = h('div', { class: 'desktop-lock' });
  applyAppearance(lockEl, s);
  lockEl.className = `${lockEl.className} desktop-lock`;

  let lockCleanup = [];

  // Top Status Bar
  const hostVal = h('span', { class: 'lock-bar-host' }, system.latest?.hostname || 'ultraspace');
  const batVal = h('span', { class: 'lock-bar-val hidden' });
  const timeVal = h('span', { class: 'lock-bar-val' });

  const themeToggle = h('button', {
    class: 'icon-btn lock-bar-btn',
    title: 'Toggle theme',
    'aria-label': 'Toggle theme',
    onclick: () => {
      const cur = document.documentElement.dataset.theme || 'dark';
      settings.set({ theme: cur === 'dark' ? 'light' : 'dark' });
    },
  }, icon(document.documentElement.dataset.theme === 'light' ? 'moon' : 'sun', 15));

  const signOutBtn = h('button', {
    class: 'icon-btn lock-bar-btn',
    title: 'Sign out',
    'aria-label': 'Sign out',
    onclick: () => {
      unlock();
      if (onSignOutHandler) onSignOutHandler();
    },
  }, icon('logOut', 15));

  const bar = h('header', { class: 'lock-bar' },
    h('div', { class: 'lock-bar-left' }, h('span', { class: 'logo-mark small' }), hostVal),
    h('div', { class: 'lock-bar-right' }, batVal, timeVal, themeToggle, signOutBtn));

  // Top bar power/battery status
  const updateBarPower = async () => {
    try {
      const p = await api('GET', '/api/power/status');
      if (p.has_battery) {
        batVal.classList.remove('hidden');
        batVal.replaceChildren(icon('battery', 14), h('span', null, `${p.percent}%`));
      } else {
        batVal.classList.add('hidden');
      }
    } catch (_) {}
  };
  updateBarPower();

  // Clock & Date Widget
  const clockStyle = s.lockClockStyle || 'modern';
  const timeEl = h('div', { class: 'lock-time' });
  const dateEl = h('div', { class: 'lock-date' });
  const clockWidget = h('div', { class: `lock-clock style-${clockStyle}` }, timeEl, dateEl);

  const tFmt = new Intl.DateTimeFormat(undefined, { hour: '2-digit', minute: '2-digit' });
  const dFmt = new Intl.DateTimeFormat(undefined, { weekday: 'long', day: 'numeric', month: 'long' });
  const tickClock = () => {
    const now = new Date();
    timeEl.textContent = tFmt.format(now);
    dateEl.textContent = dFmt.format(now);
    timeVal.textContent = tFmt.format(now);
  };
  tickClock();
  const clockTimer = setInterval(tickClock, 5000);
  lockCleanup.push(() => clearInterval(clockTimer));

  // Custom Message Widget
  let msgWidget = null;
  if (s.lockCustomMessage && s.lockCustomMessage.trim()) {
    msgWidget = h('div', { class: 'lock-custom-msg' },
      icon('info', 14), h('span', null, s.lockCustomMessage.trim()));
  }

  // System Glance Widget
  let sysWidget = null;
  if (s.lockShowSystemStats !== false) {
    const cpuPill = h('div', { class: 'lock-stat-pill' }, icon('cpu', 13), h('span', { class: 'val' }, '–'));
    const memPill = h('div', { class: 'lock-stat-pill' }, icon('memory', 13), h('span', { class: 'val' }, '–'));
    const uptimePill = h('div', { class: 'lock-stat-pill' }, icon('activity', 13), h('span', { class: 'val' }, '–'));

    sysWidget = h('div', { class: 'lock-widget-sys' }, cpuPill, memPill, uptimePill);

    const updateSys = (st) => {
      if (!st) return;
      cpuPill.querySelector('.val').textContent = `${st.cpu.percent.toFixed(0)}%`;
      const memPct = (100 * st.memory.used) / st.memory.total;
      memPill.querySelector('.val').textContent = `${memPct.toFixed(0)}%`;
      uptimePill.querySelector('.val').textContent = `up ${fmtDuration(st.uptime)}`;
    };
    if (system.latest) updateSys(system.latest);
    lockCleanup.push(on('system', updateSys));
  }

  // Now Playing Widget
  let nowPlayingWidget = null;
  if (s.lockShowNowPlaying !== false) {
    const npCover = h('div', { class: 'lock-np-cover' }, icon('music', 18));
    const npTitle = h('div', { class: 'lock-np-title' }, 'Nothing playing');
    const npArtist = h('div', { class: 'lock-np-artist' }, '');
    const npPlayBtn = h('button', { class: 'icon-btn lock-np-btn primary', 'aria-label': 'Play/Pause' }, icon('play', 16));
    const npPrevBtn = h('button', { class: 'icon-btn lock-np-btn', 'aria-label': 'Previous' }, icon('skipBack', 14));
    const npNextBtn = h('button', { class: 'icon-btn lock-np-btn', 'aria-label': 'Next' }, icon('skipForward', 14));

    nowPlayingWidget = h('div', { class: 'lock-widget-nowplaying hidden' },
      npCover,
      h('div', { class: 'lock-np-info' }, npTitle, npArtist),
      h('div', { class: 'lock-np-controls' }, npPrevBtn, npPlayBtn, npNextBtn));

    const findActiveVideo = () => {
      for (const v of document.querySelectorAll('video')) {
        if (!v.ended && v.currentTime > 0) return v;
      }
      return null;
    };

    const updateMediaUI = () => {
      const vid = findActiveVideo();
      if (vid) {
        nowPlayingWidget.classList.remove('hidden');
        npCover.replaceChildren(icon('film', 18));
        npTitle.textContent = 'Video Playback';
        npArtist.textContent = vid.paused ? 'Paused' : 'Playing';
        npPlayBtn.replaceChildren(icon(vid.paused ? 'play' : 'pause', 16));
        npPlayBtn.onclick = () => { vid.paused ? vid.play() : vid.pause(); updateMediaUI(); };
        npPrevBtn.onclick = () => { vid.currentTime = Math.max(0, vid.currentTime - 10); };
        npNextBtn.onclick = () => { vid.currentTime = Math.min(vid.duration || 0, vid.currentTime + 10); };
        return;
      }

      if (currentMusic && currentMusic.track) {
        nowPlayingWidget.classList.remove('hidden');
        if (currentMusic.track.cover) {
          npCover.style.backgroundImage = `url("${currentMusic.track.cover}")`;
          npCover.replaceChildren();
        } else {
          npCover.style.backgroundImage = '';
          npCover.replaceChildren(icon('music', 18));
        }
        npTitle.textContent = currentMusic.track.title || 'Unknown title';
        npArtist.textContent = currentMusic.track.artist || currentMusic.track.album || '';
        npPlayBtn.replaceChildren(icon(currentMusic.playing ? 'pause' : 'play', 16));
        npPlayBtn.onclick = () => {
          const audio = document.querySelector('audio');
          if (audio) {
            audio.paused ? audio.play() : audio.pause();
          }
        };
        npPrevBtn.onclick = () => {
          const prev = document.querySelector('.player button[aria-label="Previous"]');
          if (prev) prev.click();
        };
        npNextBtn.onclick = () => {
          const next = document.querySelector('.player button[aria-label="Next"]');
          if (next) next.click();
        };
        return;
      }

      nowPlayingWidget.classList.add('hidden');
    };

    updateMediaUI();
    lockCleanup.push(on('music', updateMediaUI));
    const mediaCheckInterval = setInterval(updateMediaUI, 1000);
    lockCleanup.push(() => clearInterval(mediaCheckInterval));
  }

  // Unlock Card (PIN Pad & Password fallback)
  const username = currentUser?.username || 'User';
  const hasPIN = !!currentUser?.pin;
  let usePIN = hasPIN;

  const card = h('div', { class: 'lock-card glass' });
  const errorEl = h('div', { class: 'form-error' });

  // PIN entry state
  let enteredPIN = '';
  const maxDigits = 8;
  const pinDotsWrap = h('div', { class: 'lock-pin-dots', 'aria-label': 'PIN code digits' });

  let pinSubmitBtn = null;

  const updatePINDots = () => {
    const dots = [];
    const count = Math.max(4, Math.min(12, Math.max(enteredPIN.length, 4)));
    for (let i = 0; i < count; i++) {
      dots.push(h('span', { class: `pin-dot ${i < enteredPIN.length ? 'filled' : ''}` }));
    }
    pinDotsWrap.replaceChildren(...dots);
    if (pinSubmitBtn) pinSubmitBtn.disabled = enteredPIN.length < 4;
  };

  const handleDigit = async (d) => {
    if (enteredPIN.length >= 12) return;
    enteredPIN += d;
    updatePINDots();
    errorEl.textContent = '';
  };

  const handleBackspace = () => {
    if (enteredPIN.length > 0) {
      enteredPIN = enteredPIN.slice(0, -1);
      updatePINDots();
      errorEl.textContent = '';
    }
  };

  const handleClear = () => {
    enteredPIN = '';
    updatePINDots();
    errorEl.textContent = '';
  };

  const tryUnlockPIN = async () => {
    if (enteredPIN.length < 4) return;
    if (pinSubmitBtn) pinSubmitBtn.disabled = true;
    try {
      await api('POST', '/api/account/unlock', { pin: enteredPIN });
      unlock();
    } catch (err) {
      // Wrong PIN
      errorEl.textContent = err.message || 'Incorrect PIN';
      card.classList.remove('shake');
      void card.offsetWidth;
      card.classList.add('shake');
      enteredPIN = '';
      updatePINDots();
      if (pinSubmitBtn) pinSubmitBtn.disabled = true;
    }
  };

  // Keyboard handler for lock screen
  const onLockKeyDown = async (e) => {
    if (!activeLockEl) return;
    if (usePIN) {
      if (e.key >= '0' && e.key <= '9') {
        e.preventDefault();
        await handleDigit(e.key);
      } else if (e.key === 'Backspace') {
        e.preventDefault();
        handleBackspace();
      } else if (e.key === 'Escape') {
        e.preventDefault();
        handleClear();
      } else if (e.key === 'Enter') {
        e.preventDefault();
        await tryUnlockPIN();
      }
    }
  };
  window.addEventListener('keydown', onLockKeyDown);
  lockCleanup.push(() => window.removeEventListener('keydown', onLockKeyDown));

  const buildPINPad = () => {
    const keys = ['1', '2', '3', '4', '5', '6', '7', '8', '9', 'C', '0', '⌫'];
    const pad = h('div', { class: 'pin-keypad' });
    keys.forEach((k) => {
      const btn = h('button', {
        class: `pin-key ${k === 'C' || k === '⌫' ? 'pin-action' : ''}`,
        type: 'button',
        'aria-label': k === 'C' ? 'Clear' : k === '⌫' ? 'Backspace' : `Digit ${k}`,
        onclick: async () => {
          if (k === 'C') handleClear();
          else if (k === '⌫') handleBackspace();
          else await handleDigit(k);
        },
      }, k === '⌫' ? icon('chevronLeft', 18) : k);
      pad.append(btn);
    });
    return pad;
  };

  const renderCard = () => {
    errorEl.textContent = '';
    enteredPIN = '';

    const avatar = h('div', { class: 'lock-avatar' },
      h('span', { class: 'avatar big' }, username.slice(0, 1).toUpperCase()));
    const title = h('div', { class: 'lock-title' }, username);

    if (usePIN) {
      pinSubmitBtn = h('button', {
        class: 'btn primary lock-pin-btn',
        type: 'button',
        disabled: true,
        onclick: () => tryUnlockPIN(),
      }, icon('unlock', 15), 'Unlock');

      updatePINDots();
      const numpad = buildPINPad();
      const passSwitch = h('button', {
        class: 'link-btn lock-back',
        type: 'button',
        onclick: () => { usePIN = false; renderCard(); },
      }, 'Unlock with password');

      card.replaceChildren(
        avatar,
        title,
        h('div', { class: 'lock-sub' }, 'Enter PIN'),
        pinDotsWrap,
        numpad,
        pinSubmitBtn,
        errorEl,
        passSwitch,
      );
    } else {
      const passInput = h('input', {
        class: 'input lock-input',
        type: 'password',
        placeholder: 'Password',
        'aria-label': 'Password',
        autocomplete: 'current-password',
      });
      const submitBtn = h('button', {
        class: 'lock-submit',
        type: 'submit',
        'aria-label': 'Unlock',
      }, icon('chevronRight', 20));

      const form = h('form', {
        class: 'lock-form',
        onsubmit: async (e) => {
          e.preventDefault();
          errorEl.textContent = '';
          submitBtn.disabled = true;
          try {
            await api('POST', '/api/account/unlock', { password: passInput.value });
            unlock();
          } catch (err) {
            errorEl.textContent = err.message || 'Incorrect password';
            card.classList.remove('shake');
            void card.offsetWidth;
            card.classList.add('shake');
            passInput.value = '';
            passInput.focus();
            submitBtn.disabled = false;
          }
        },
      },
      avatar,
      title,
      h('div', { class: 'lock-sub' }, 'Enter your master password'),
      h('div', { class: 'lock-row' }, passInput, submitBtn),
      errorEl,
      hasPIN ? h('button', {
        class: 'link-btn lock-back',
        type: 'button',
        onclick: () => { usePIN = true; renderCard(); },
      }, 'Unlock with PIN') : null);

      card.replaceChildren(form);
      setTimeout(() => passInput.focus(), 50);
    }
  };

  renderCard();

  // Unlock transition
  const unlock = () => {
    sessionStorage.removeItem('us.locked');
    lockEl.classList.add('unlocking');
    lockCleanup.forEach((fn) => { try { fn(); } catch (_) {} });
    lockCleanup = [];
    setTimeout(() => {
      lockEl.remove();
      activeLockEl = null;
      recordActivity();
    }, 280);
  };

  const centerCol = h('div', { class: 'lock-center-col' },
    clockWidget,
    msgWidget,
    nowPlayingWidget,
    sysWidget,
    card);

  lockEl.append(bar, centerCol);
  document.body.append(lockEl);
  activeLockEl = lockEl;
}
