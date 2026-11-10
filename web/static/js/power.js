// Power and system controls: reboot, shutdown, and service restart with environment detection.
import { h, icon, api, toast, confirmDialog } from './core.js';

let cachedPower = null;

export async function getPowerStatus(force) {
  if (cachedPower && !force) return cachedPower;
  try {
    cachedPower = await api('GET', '/api/power/status');
    return cachedPower;
  } catch {
    return cachedPower;
  }
}

export function showReconnectingOverlay(title, subtitle) {
  const overlay = h('div', {
    class: 'power-overlay glass',
    style: {
      position: 'fixed', inset: '0', zIndex: '99999',
      display: 'flex', flexDirection: 'column', alignItems: 'center', justifyContent: 'center',
      background: 'rgba(0, 0, 0, 0.78)', backdropFilter: 'blur(16px)',
      color: '#fff', textAlign: 'center', padding: '24px',
    },
  },
    h('div', { style: { display: 'flex', flexDirection: 'column', alignItems: 'center', gap: '16px', maxWidth: '400px' } },
      h('span', { class: 'bw-spin big' }),
      h('h2', { style: { margin: 0, fontSize: '20px', fontWeight: '600' } }, title),
      h('p', { class: 'power-sub', style: { margin: 0, fontSize: '14px', opacity: 0.8, lineHeight: '1.5' } }, subtitle)
    )
  );
  document.body.append(overlay);

  let attempts = 0;
  const poll = setInterval(async () => {
    attempts++;
    if (attempts < 3) return; // Wait 6-8s before first check
    try {
      const res = await fetch('/api/power/status', { method: 'GET', credentials: 'same-origin', headers: { 'X-Ultraspace': '1' } });
      if (res.ok) {
        clearInterval(poll);
        const sub = overlay.querySelector('.power-sub');
        if (sub) sub.textContent = 'Server back online! Reloading…';
        setTimeout(() => window.location.reload(), 1200);
      }
    } catch {}
  }, 2500);
}

export function showShutdownOverlay() {
  const overlay = h('div', {
    class: 'power-overlay glass',
    style: {
      position: 'fixed', inset: '0', zIndex: '99999',
      display: 'flex', flexDirection: 'column', alignItems: 'center', justifyContent: 'center',
      background: 'rgba(0, 0, 0, 0.88)', backdropFilter: 'blur(20px)',
      color: '#fff', textAlign: 'center', padding: '24px',
    },
  },
    h('div', { style: { display: 'flex', flexDirection: 'column', alignItems: 'center', gap: '16px', maxWidth: '420px' } },
      h('span', { style: { color: 'var(--muted, #94a3b8)' } }, icon('power', 48)),
      h('h2', { style: { margin: 0, fontSize: '22px', fontWeight: '600' } }, 'Server Powered Off'),
      h('p', { style: { margin: 0, fontSize: '14px', opacity: 0.8, lineHeight: '1.5' } },
        'The host system has powered off. It is safe to unplug or close the machine. To turn it back on, press its physical power button.'
      )
    )
  );
  document.body.append(overlay);
}

export async function promptReboot() {
  const ok = await confirmDialog(
    'Restart Server',
    'Are you sure you want to reboot the system? All active sessions and background transfers will pause until the server restarts.',
    'Restart Server',
    false
  );
  if (!ok) return;

  try {
    await api('POST', '/api/power/reboot');
    showReconnectingOverlay('Rebooting Server…', 'The host system is restarting. Ultraspace will automatically reconnect as soon as it comes back up.');
  } catch (err) {
    toast(`Reboot failed: ${err.message}`, 'error');
  }
}

export async function promptShutdown() {
  const ok = await confirmDialog(
    'Shut Down System',
    'Are you sure you want to shut down? The machine will power off completely and must be turned back on manually with its physical power button.',
    'Shut Down',
    true
  );
  if (!ok) return;

  try {
    await api('POST', '/api/power/shutdown');
    showShutdownOverlay();
  } catch (err) {
    toast(`Shutdown failed: ${err.message}`, 'error');
  }
}

export async function promptRestartService() {
  const ok = await confirmDialog(
    'Restart Ultraspace Service',
    'Restart the Ultraspace daemon process? Active streaming sessions will reload.',
    'Restart Service',
    false
  );
  if (!ok) return;

  try {
    await api('POST', '/api/power/restart-service');
    showReconnectingOverlay('Restarting Ultraspace…', 'The service is restarting. Reconnecting automatically…');
  } catch (err) {
    toast(`Restart service failed: ${err.message}`, 'error');
  }
}
