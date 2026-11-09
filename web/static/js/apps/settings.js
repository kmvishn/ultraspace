import { h, icon, api, toast, report, confirmDialog, dialog, settings, system, on, fmtBytes, fmtDuration, fmtAgo, fmtDate, baseOf, fileUrl, emit } from '../core.js';
import { deviceName } from '../session.js';
import { patternPad } from '../pattern.js';
import { chatbotConfigChanged } from '../chatbot.js';
import { registerApp, openApp } from '../wm.js';
import { WALLPAPERS, ACCENTS } from '../desktop.js';

registerApp({
  id: 'settings', name: 'Settings', icon: 'sliders', color: ['#94a3b8', '#475569'], width: 820, height: 600,
  mount(win, args) { return mountSettings(win, args); },
});

const SECTIONS = [
  { id: 'appearance', label: 'Appearance', icon: 'palette' },
  { id: 'sound', label: 'Sound & Audio', icon: 'volume' },
  { id: 'network', label: 'Network', icon: 'wifi' },
  { id: 'sinkhole', label: 'DNS Sinkhole (AdBlock)', icon: 'shield' },
  { id: 'hardware', label: 'Bluetooth & Hardware', icon: 'bluetooth' },
  { id: 'account', label: 'Account', icon: 'user' },
  { id: 'devices', label: 'Devices', icon: 'laptop' },
  { id: 'chatbot', label: 'Ultra Chat Bot', icon: 'sparkles' },
  { id: 'about', label: 'About', icon: 'info' },
];

function mountSettings(win, args) {
  const nav = h('div', { class: 'side-list' });
  const main = h('div', { class: 'main scroll settings-main' });
  win.body.append(h('div', { class: 'app app-settings' }, h('aside', { class: 'side' }, h('div', { class: 'side-title' }, 'Settings'), nav), main));
  let current = null;
  let off = null;

  const show = (id) => {
    current = id;
    if (off) { off(); off = null; }
    nav.replaceChildren(...SECTIONS.map((s) => h('button', { class: `side-item ${s.id === id ? 'active' : ''}`, onclick: () => show(s.id) }, icon(s.icon, 16), h('span', null, s.label))));
    win.setTitle(`Settings — ${SECTIONS.find((s) => s.id === id).label}`);
    off = { appearance, sound, network, sinkhole, hardware, account, devices, chatbot, about }[id](main) || null;
  };
  show(args.restore?.section || args.section || 'appearance');
  return {
    onArgs(a) { if (a.section && a.section !== current) show(a.section); },
    onDestroy() { if (off) off(); },
    saveState: () => ({ section: current }),
  };

  function group(title, desc, ...children) {
    return h('section', { class: 'set-group' }, h('h3', null, title), desc ? h('p', { class: 'set-desc' }, desc) : null, children);
  }

  function appearance(el) {
    const render = () => {
      const s = settings.data;
      const themeSeg = h('div', { class: 'seg big' }, [['light', 'Light', 'sun'], ['dark', 'Dark', 'moon'], ['auto', 'Auto', 'monitor']].map(([id, label, ic]) =>
        h('button', { class: `seg-btn text ${s.theme === id ? 'active' : ''}`, onclick: () => settings.set({ theme: id }) }, icon(ic, 15), label)));
      const accents = h('div', { class: 'swatches' }, ACCENTS.map((c) => h('button', {
        class: `swatch ${s.accent === c ? 'active' : ''}`, 'aria-label': `Accent ${c}`, style: { background: c },
        onclick: () => settings.set({ accent: c }),
      }, s.accent === c ? icon('check', 14) : null)));
      const custom = String(s.wallpaper).startsWith('file:') ? s.wallpaper.slice(5) : null;
      const walls = h('div', { class: 'wall-grid' },
        WALLPAPERS.map((w) => h('button', { class: `wall ${s.wallpaper === w.id ? 'active' : ''}`, onclick: () => settings.set({ wallpaper: w.id }) },
          h('span', { class: `wall-prev wp-${w.id}` }), h('span', { class: 'wall-name' }, w.name))),
        custom ? h('button', { class: 'wall active' },
          h('span', { class: 'wall-prev wp-image', style: { backgroundImage: `url("${fileUrl(custom, true)}")` } }), h('span', { class: 'wall-name' }, baseOf(custom))) : null);
      const buttonsSeg = h('div', { class: 'seg big' }, [['right', 'Right'], ['left', 'Left']].map(([id, label]) =>
        h('button', { class: `seg-btn text ${(s.buttons || 'right') === id ? 'active' : ''}`, onclick: () => settings.set({ buttons: id }) }, label)));
      const glass = h('label', { class: 'switch-row' },
        h('span', null, h('b', null, 'Transparency'), h('span', { class: 'set-desc' }, 'Frosted-glass menu bar, dock and widgets. Turn off on slow devices.')),
        h('input', { type: 'checkbox', class: 'switch', checked: !!s.transparency, onchange: (e) => settings.set({ transparency: e.target.checked }) }));
      el.replaceChildren(h('div', { class: 'settings-page' },
        group('Theme', null, themeSeg),
        group('Accent colour', null, accents),
        group('Window buttons', 'Minimise, maximise and close: on the right like Windows, or on the left like macOS.', buttonsSeg),
        group('Wallpaper', 'Or right-click any image in Files and choose “Set as wallpaper”.', walls,
          h('button', { class: 'btn small', onclick: () => openApp('files') }, icon('folder', 14), 'Choose from Files…')),
        group('Effects', null, glass)));
    };
    render();
    return on('settings', render);
  }

  function sound(el) {
    const sinkList = h('div', { class: 'device-list' }, h('div', { class: 'empty-state small' }, 'Checking audio devices…'));
    const volSlider = h('input', { type: 'range', min: '0', max: '100', value: '100', class: 'slider', style: { flex: '1', accentColor: 'var(--accent)' } });
    const volNum = h('span', { class: 'card-big', style: { minWidth: '48px', textAlign: 'right' } }, '100%');
    const muteBtn = h('button', { class: 'btn small' }, icon('volume', 14), 'Mute');
    const testBtn = h('button', { class: 'btn small primary' }, icon('play', 14), 'Play Test Sound');
    let timer = null;
    let isUpdating = false;

    async function loadStatus() {
      if (isUpdating) return;
      try {
        const st = await api('GET', '/api/audio/status');
        if (!st.available) {
          sinkList.replaceChildren(h('div', { class: 'device' },
            h('span', { class: 'device-icon' }, icon('volumeX', 20)),
            h('div', { class: 'device-text' },
              h('div', { class: 'device-name' }, 'Audio subsystem not available'),
              h('div', { class: 'set-desc' }, 'PulseAudio or PipeWire (pactl) is not active on this host.'))));
          volSlider.disabled = true;
          muteBtn.disabled = true;
          testBtn.disabled = true;
          return;
        }

        volSlider.disabled = false;
        muteBtn.disabled = false;
        testBtn.disabled = false;

        volSlider.value = st.volume;
        volNum.textContent = `${st.volume}%`;
        muteBtn.replaceChildren(icon(st.mute ? 'volumeX' : 'volume', 14), st.mute ? 'Unmute' : 'Mute');
        if (st.mute) muteBtn.classList.add('warn'); else muteBtn.classList.remove('warn');

        if (!st.sinks || st.sinks.length === 0) {
          sinkList.replaceChildren(h('div', { class: 'empty-state small' }, 'No audio output sinks detected.'));
          return;
        }

        sinkList.replaceChildren(...st.sinks.map((s) => {
          let sIcon = 'volume';
          let sTypeBadge = 'Speaker / Jack';
          if (s.type === 'hdmi') {
            sIcon = 'tv';
            sTypeBadge = 'HDMI / DisplayPort';
          } else if (s.type === 'bluetooth') {
            sIcon = 'music';
            sTypeBadge = 'Bluetooth';
          } else if (s.type === 'usb') {
            sIcon = 'sliders';
            sTypeBadge = 'USB Audio';
          } else if (s.type === 'virtual') {
            sIcon = 'monitor';
            sTypeBadge = 'Virtual Sink';
          }

          const actionBtn = s.active
            ? h('span', { class: 'pill active' }, 'Active Output')
            : h('button', { class: 'btn small', onclick: () => selectSink(s) }, 'Select');

          return h('div', { class: `device ${s.active ? 'current' : ''}` },
            h('span', { class: 'device-icon' }, icon(sIcon, 18)),
            h('div', { class: 'device-text' },
              h('div', { class: 'device-name' }, s.description || s.name, h('span', { class: 'pill' }, sTypeBadge)),
              h('div', { class: 'set-desc' }, `${s.volume}% volume · ${s.mute ? 'Muted' : 'Unmuted'} · ${s.name}`)),
            actionBtn);
        }));
      } catch (err) {
        report(err);
      }
    }

    async function selectSink(s) {
      try {
        await api('POST', '/api/audio/sink', { sink: s.name });
        toast(`Audio output switched to ${s.description || s.name}`);
        await loadStatus();
      } catch (err) {
        report(err);
      }
    }

    volSlider.oninput = () => {
      volNum.textContent = `${volSlider.value}%`;
    };

    volSlider.onchange = async () => {
      isUpdating = true;
      try {
        await api('POST', '/api/audio/volume', { volume: parseInt(volSlider.value, 10) });
      } catch (err) {
        report(err);
      } finally {
        isUpdating = false;
        await loadStatus();
      }
    };

    muteBtn.onclick = async () => {
      isUpdating = true;
      try {
        const currentMuted = muteBtn.textContent.trim() === 'Unmute';
        await api('POST', '/api/audio/mute', { mute: !currentMuted });
      } catch (err) {
        report(err);
      } finally {
        isUpdating = false;
        await loadStatus();
      }
    };

    testBtn.onclick = async () => {
      testBtn.disabled = true;
      try {
        await api('POST', '/api/audio/test');
        toast('Playing test chime on server speakers…');
      } catch (err) {
        toast('Could not play test sound: ' + err.message);
      } finally {
        setTimeout(() => { testBtn.disabled = false; }, 1500);
      }
    };

    const volRow = h('div', { class: 'switch-row', style: { alignItems: 'center' } },
      h('div', { style: { display: 'flex', alignItems: 'center', gap: '12px', flex: '1', marginRight: '16px' } },
        icon('volume', 18),
        volSlider,
        volNum),
      h('div', { class: 'hero-actions' }, muteBtn, testBtn));

    el.replaceChildren(h('div', { class: 'settings-page' },
      group('Master Volume & Controls', 'Controls physical output volume on the server.', volRow),
      group('Audio Output Devices', 'Choose which physical speaker, headphone jack, HDMI, or Bluetooth sink to output sound through.', sinkList)));

    loadStatus();
    timer = setInterval(loadStatus, 15_000);
    return () => clearInterval(timer);
  }

  function network(el) {
    const statusBox = h('div', { class: 'device-list' }, h('div', { class: 'empty-state small' }, 'Checking network…'));
    const hotspotBox = h('div', { class: 'device-list' });
    const wifiList = h('div', { class: 'device-list' }, h('div', { class: 'empty-state small' }, 'Scanning Wi-Fi…'));
    const ifaceList = h('div', { class: 'device-list' });
    const scanBtn = h('button', { class: 'btn small', onclick: () => scan(true) }, icon('refresh', 14), 'Scan');
    let timer = null;

    async function startHotspot() {
      try {
        await api('POST', '/api/network/hotspot/start');
        toast('Setup hotspot activated (Ultraspace-Setup)');
        await loadStatus();
      } catch (err) {
        report(err);
      }
    }

    async function stopHotspot() {
      try {
        await api('POST', '/api/network/hotspot/stop');
        toast('Setup hotspot stopped');
        await loadStatus();
      } catch (err) {
        report(err);
      }
    }

    async function loadStatus() {
      try {
        const st = await api('GET', '/api/network/status');
        if (!st.available) {
          statusBox.replaceChildren(h('div', { class: 'device' },
            h('span', { class: 'device-icon' }, icon('wifiOff', 20)),
            h('div', { class: 'device-text' },
              h('div', { class: 'device-name' }, 'NetworkManager not available'),
              h('div', { class: 'set-desc' }, 'NetworkManager (nmcli) is not active on this host.'))));
          hotspotBox.replaceChildren();
          wifiList.replaceChildren(h('div', { class: 'empty-state small' }, 'Wi-Fi management requires NetworkManager.'));
          ifaceList.replaceChildren();
          return;
        }

        const iconName = st.connected ? (st.type === 'wifi' ? 'wifi' : 'server') : 'wifiOff';
        const title = st.connected
          ? (st.type === 'wifi' ? `Connected to Wi-Fi (${st.ssid || 'Unknown'})` : `Connected via Ethernet (${st.primary})`)
          : 'Disconnected';
        const sub = st.connected ? `IP: ${st.ip || 'Obtaining…'} · Primary: ${st.primary}` : 'No active internet connection';

        statusBox.replaceChildren(h('div', { class: 'device current' },
          h('span', { class: 'device-icon' }, icon(iconName, 20)),
          h('div', { class: 'device-text' },
            h('div', { class: 'device-name' }, title, h('span', { class: `pill ${st.connected ? 'active' : 'failed'}` }, st.connected ? 'Online' : 'Offline')),
            h('div', { class: 'set-desc' }, sub))));

        if (st.hotspot_active) {
          hotspotBox.replaceChildren(h('div', { class: 'device current' },
            h('span', { class: 'device-icon' }, icon('wifi', 20)),
            h('div', { class: 'device-text' },
              h('div', { class: 'device-name' }, `Setup Hotspot Active (${st.hotspot_ssid || 'Ultraspace-Setup'})`, h('span', { class: 'pill active' }, 'Broadcasting')),
              h('div', { class: 'set-desc' }, 'Connect your phone or device to this Wi-Fi to configure the server.')),
            h('button', { class: 'btn small', onclick: () => stopHotspot() }, 'Stop Hotspot')));
        } else {
          hotspotBox.replaceChildren(h('div', { class: 'device' },
            h('span', { class: 'device-icon' }, icon('wifi', 20)),
            h('div', { class: 'device-text' },
              h('div', { class: 'device-name' }, 'Fallback Setup Hotspot', h('span', { class: 'pill' }, 'Auto-Standby')),
              h('div', { class: 'set-desc' }, 'Broadcasts "Ultraspace-Setup" automatically if no network is reachable after 45s.')),
            h('button', { class: 'btn small', onclick: () => startHotspot() }, 'Start Hotspot Now')));
        }

        if (st.interfaces && st.interfaces.length > 0) {
          ifaceList.replaceChildren(...st.interfaces.map((iface) => {
            const ifIcon = iface.type === 'wifi' ? 'wifi' : 'server';
            const details = [
              iface.ipv4 ? `IP: ${iface.ipv4}` : null,
              iface.gateway ? `Gateway: ${iface.gateway}` : null,
              iface.mac ? `MAC: ${iface.mac}` : null,
            ].filter(Boolean).join(' · ');
            return h('div', { class: 'device' },
              h('span', { class: 'device-icon' }, icon(ifIcon, 18)),
              h('div', { class: 'device-text' },
                h('div', { class: 'device-name' }, iface.name, h('span', { class: `pill ${iface.state === 'connected' ? 'active' : ''}` }, iface.state)),
                h('div', { class: 'set-desc' }, details || 'No IP address assigned')));
          }));
        } else {
          ifaceList.replaceChildren(h('div', { class: 'empty-state small' }, 'No network interfaces detected.'));
        }
      } catch (err) {
        report(err);
      }
    }

    async function scan(rescan = false) {
      scanBtn.disabled = true;
      try {
        const { networks } = await api('GET', `/api/network/wifi/scan${rescan ? '?rescan=true' : ''}`);
        if (!networks || networks.length === 0) {
          wifiList.replaceChildren(h('div', { class: 'empty-state small' }, 'No Wi-Fi networks found.'));
          return;
        }

        wifiList.replaceChildren(...networks.map((ap) => {
          const actionBtn = ap.in_use
            ? h('button', { class: 'btn small', onclick: () => disconnectWiFi(ap) }, 'Disconnect')
            : h('button', { class: 'btn small primary', onclick: () => promptConnect(ap) }, 'Connect');

          return h('div', { class: `device ${ap.in_use ? 'current' : ''}` },
            h('span', { class: 'device-icon' }, icon('wifi', 18)),
            h('div', { class: 'device-text' },
              h('div', { class: 'device-name' }, ap.ssid, ap.in_use ? h('span', { class: 'pill active' }, 'Connected') : null),
              h('div', { class: 'set-desc' }, `${ap.signal}% signal · ${ap.freq} · ${ap.security}`)),
            actionBtn);
        }));
      } catch (err) {
        wifiList.replaceChildren(h('div', { class: 'empty-state small' }, 'Wi-Fi scan failed: ' + err.message));
      } finally {
        scanBtn.disabled = false;
      }
    }

    function promptConnect(ap) {
      const pwd = h('input', { class: 'input', type: 'password', placeholder: 'Wi-Fi password', autocomplete: 'off' });
      const errorMsg = h('div', { class: 'form-error' });
      const connectBtn = h('button', { class: 'btn primary' }, 'Connect');

      const doConnect = async () => {
        errorMsg.textContent = '';
        connectBtn.disabled = true;
        connectBtn.textContent = 'Connecting…';
        try {
          await api('POST', '/api/network/wifi/connect', {
            ssid: ap.ssid,
            password: pwd.value,
          });
          close();
          toast(`Connected to ${ap.ssid}`);
          await loadStatus();
          await scan();
        } catch (err) {
          errorMsg.textContent = err.message || 'Failed to connect';
          connectBtn.disabled = false;
          connectBtn.textContent = 'Connect';
        }
      };

      connectBtn.onclick = doConnect;
      pwd.addEventListener('keydown', (e) => { if (e.key === 'Enter') doConnect(); });

      const close = dialog({
        title: `Connect to ${ap.ssid}`,
        width: '380px',
        body: h('div', { class: 'set-form' },
          ap.security === 'Open' ? h('p', { class: 'dialog-text' }, 'This is an open network without a password.') : null,
          ap.security !== 'Open' ? [h('label', { class: 'field-label' }, 'Password'), pwd] : null,
          errorMsg),
        actions: [h('button', { class: 'btn', onclick: () => close() }, 'Cancel'), connectBtn],
      });
      requestAnimationFrame(() => pwd.focus());
    }

    async function disconnectWiFi(ap) {
      if (!(await confirmDialog(`Disconnect from ${ap.ssid}?`, 'The server will lose this Wi-Fi connection.', 'Disconnect'))) return;
      try {
        await api('POST', '/api/network/wifi/disconnect', { device: 'wlan0' });
        toast(`Disconnected from ${ap.ssid}`);
        await loadStatus();
        await scan();
      } catch (err) {
        report(err);
      }
    }

    const joinOtherBtn = h('button', { class: 'btn small', onclick: () => promptJoinOther() }, icon('plus', 14), 'Join Other Network…');

    function promptJoinOther() {
      const ssidInput = h('input', { class: 'input', placeholder: 'Network Name (SSID)', autocomplete: 'off' });
      const pwdInput = h('input', { class: 'input', type: 'password', placeholder: 'Password', autocomplete: 'off' });
      const hiddenCheck = h('input', { type: 'checkbox', class: 'switch' });
      const errorMsg = h('div', { class: 'form-error' });
      const connectBtn = h('button', { class: 'btn primary' }, 'Connect');

      const doJoin = async () => {
        if (!ssidInput.value.trim()) {
          errorMsg.textContent = 'Please enter an SSID';
          return;
        }
        errorMsg.textContent = '';
        connectBtn.disabled = true;
        connectBtn.textContent = 'Connecting…';
        try {
          await api('POST', '/api/network/wifi/connect', {
            ssid: ssidInput.value.trim(),
            password: pwdInput.value,
            hidden: hiddenCheck.checked,
          });
          close();
          toast(`Connected to ${ssidInput.value.trim()}`);
          await loadStatus();
          await scan();
        } catch (err) {
          errorMsg.textContent = err.message || 'Failed to connect';
          connectBtn.disabled = false;
          connectBtn.textContent = 'Connect';
        }
      };

      connectBtn.onclick = doJoin;
      pwdInput.addEventListener('keydown', (e) => { if (e.key === 'Enter') doJoin(); });

      const close = dialog({
        title: 'Join Other Network',
        width: '380px',
        body: h('div', { class: 'set-form' },
          h('label', { class: 'field-label' }, 'Network Name (SSID)'), ssidInput,
          h('label', { class: 'field-label' }, 'Password'), pwdInput,
          h('label', { class: 'switch-row' },
            h('span', null, h('b', null, 'Hidden network'), h('span', { class: 'set-desc' }, 'This network does not broadcast its SSID.')),
            hiddenCheck),
          errorMsg),
        actions: [h('button', { class: 'btn', onclick: () => close() }, 'Cancel'), connectBtn],
      });
      requestAnimationFrame(() => ssidInput.focus());
    }

    el.replaceChildren(h('div', { class: 'settings-page' },
      group('Network Status', 'Current connection and IP configuration of this server.', statusBox),
      group('Fallback Setup Hotspot', 'Provides emergency Wi-Fi access if your laptop server loses network.', hotspotBox),
      group('Wi-Fi Networks', 'Available wireless networks detected by this server.', wifiList,
        h('div', { class: 'hero-actions', style: { marginTop: '12px' } }, scanBtn, joinOtherBtn)),
      group('Network Interfaces', 'Physical and virtual network adapters installed on the machine.', ifaceList)));

    loadStatus();
    scan();
    timer = setInterval(() => { loadStatus(); }, 15_000);
    return () => clearInterval(timer);
  }

  function sinkhole(el) {
    const toggleBox = h('div', { class: 'device-list' }, h('div', { class: 'empty-state small' }, 'Checking DNS Sinkhole…'));
    const statsGrid = h('div', { class: 'stat-grid' });
    const rulesBox = h('div', { class: 'set-form' });
    const queriesBox = h('div', { class: 'device-list' }, h('div', { class: 'empty-state small' }, 'No queries recorded yet.'));
    let timer = null;
    let localIP = 'this server’s IP';

    async function loadStatus() {
      try {
        const [st, netSt, qRes] = await Promise.all([
          api('GET', '/api/sinkhole/status'),
          api('GET', '/api/network/status').catch(() => null),
          api('GET', '/api/sinkhole/queries').catch(() => ({ queries: [] })),
        ]);

        if (netSt && netSt.interfaces) {
          const iface = netSt.interfaces.find((i) => i.ip && !i.ip.startsWith('127.'));
          if (iface) localIP = iface.ip.split('/')[0];
        }

        // 1. Toggle switch & status
        const toggleSwitch = h('input', {
          type: 'checkbox',
          checked: !!st.enabled,
          onchange: async (e) => {
            const enabled = e.target.checked;
            try {
              const res = await api('POST', '/api/sinkhole/toggle', { enabled });
              if (res.ok) {
                toast(enabled ? 'DNS Sinkhole AdBlocker activated' : 'DNS Sinkhole AdBlocker deactivated');
              } else if (res.error) {
                toast(res.error, 'error');
              }
            } catch (err) {
              report(err);
            }
            await loadStatus();
          },
        });

        toggleBox.replaceChildren(
          h('div', { class: 'device' },
            h('span', { class: 'device-icon' }, icon(st.enabled ? 'shield' : 'shieldOff', 22)),
            h('div', { class: 'device-text' },
              h('div', { class: 'device-name' },
                'DNS Sinkhole AdBlocker',
                st.enabled
                  ? h('span', { class: 'pill active' }, st.listening ? `Listening on port ${st.port}` : 'Active')
                  : h('span', { class: 'pill' }, 'Disabled')
              ),
              h('div', { class: 'set-desc' },
                st.enabled
                  ? `To block ads across your home network: set your router or phone's Primary DNS server to ${localIP} (port ${st.port}).`
                  : 'Turn on to block advertising, trackers, and telemetry network-wide on all connected devices.'
              )
            ),
            h('label', { class: 'toggle' }, toggleSwitch, h('span', { class: 'slider' }))
          )
        );

        // 2. Stats cards
        const stats = st.stats || {};
        const statCard = (label, val, sub) => h('div', {
          class: 'stat-card',
          style: { padding: '14px', background: 'var(--card-bg, rgba(255,255,255,0.04))', borderRadius: 'var(--r-md)', border: '1px solid var(--border)' },
        },
          h('div', { class: 'set-desc', style: { marginBottom: '4px' } }, label),
          h('div', { style: { fontSize: '24px', fontWeight: '700', color: 'var(--text)' } }, String(val)),
          sub ? h('div', { class: 'set-desc', style: { marginTop: '4px', fontSize: '11px' } }, sub) : null
        );

        statsGrid.style.display = 'grid';
        statsGrid.style.gridTemplateColumns = 'repeat(auto-fit, minmax(130px, 1fr))';
        statsGrid.style.gap = '10px';

        statsGrid.replaceChildren(
          statCard('Total Queries', stats.total_queries || 0),
          statCard('Ads Blocked', stats.blocked_queries || 0),
          statCard('Block Ratio', `${(st.percent_blocked || 0).toFixed(1)}%`),
          statCard('Active Rules', (st.rule_count || 0) + (st.blacklist_count || 0), `${st.rule_count || 0} built-in`)
        );

        // 3. Custom rules
        const addDomain = async (action, domainInput) => {
          const d = domainInput.value.trim();
          if (!d) return;
          try {
            await api('POST', '/api/sinkhole/rules', { action, domain: d });
            toast(`Rule added for ${d}`);
            domainInput.value = '';
            await loadStatus();
          } catch (err) {
            report(err);
          }
        };

        const removeDomain = async (action, domain) => {
          try {
            await api('POST', '/api/sinkhole/rules', { action, domain });
            toast(`Rule removed for ${domain}`);
            await loadStatus();
          } catch (err) {
            report(err);
          }
        };

        const wlInput = h('input', { class: 'input', placeholder: 'e.g. googleadservices.com' });
        const blInput = h('input', { class: 'input', placeholder: 'e.g. tracking.site.com' });

        const wlList = (st.whitelist || []).map((d) => h('div', { class: 'side-item', style: { justifyContent: 'space-between', height: '28px' } },
          h('span', null, d),
          h('button', { class: 'icon-btn', style: { padding: '2px' }, onclick: () => removeDomain('remove_whitelist', d) }, icon('x', 12))
        ));

        const blList = (st.blacklist || []).map((d) => h('div', { class: 'side-item', style: { justifyContent: 'space-between', height: '28px' } },
          h('span', null, d),
          h('button', { class: 'icon-btn', style: { padding: '2px' }, onclick: () => removeDomain('remove_blacklist', d) }, icon('x', 12))
        ));

        const updateBtn = h('button', {
          class: 'btn small',
          onclick: async () => {
            updateBtn.disabled = true;
            try {
              await api('POST', '/api/sinkhole/update');
              toast('Updating blocklists in background…');
            } catch (err) {
              report(err);
            } finally {
              updateBtn.disabled = false;
            }
          },
        }, icon('download', 14), 'Update StevenBlack Blocklist');

        rulesBox.replaceChildren(
          h('div', { style: { display: 'grid', gridTemplateColumns: '1fr 1fr', gap: '16px' } },
            h('div', null,
              h('div', { class: 'side-title', style: { paddingLeft: '0' } }, 'Custom Whitelist (Allowed)'),
              h('div', { style: { display: 'flex', gap: '6px', marginBottom: '8px' } },
                wlInput,
                h('button', { class: 'btn small', onclick: () => addDomain('add_whitelist', wlInput) }, 'Add')
              ),
              h('div', { class: 'side-list' }, wlList.length ? wlList : h('div', { class: 'set-desc' }, 'No whitelisted domains.'))
            ),
            h('div', null,
              h('div', { class: 'side-title', style: { paddingLeft: '0' } }, 'Custom Blacklist (Blocked)'),
              h('div', { style: { display: 'flex', gap: '6px', marginBottom: '8px' } },
                blInput,
                h('button', { class: 'btn small primary', onclick: () => addDomain('add_blacklist', blInput) }, 'Add')
              ),
              h('div', { class: 'side-list' }, blList.length ? blList : h('div', { class: 'set-desc' }, 'No custom blacklisted domains.'))
            )
          ),
          h('div', { style: { marginTop: '16px' } }, updateBtn)
        );

        // 4. Queries log
        const queries = qRes.queries || [];
        if (queries.length === 0) {
          queriesBox.replaceChildren(h('div', { class: 'empty-state small' }, 'No DNS queries recorded yet. Send queries to this device to see live logs.'));
        } else {
          queriesBox.replaceChildren(...queries.slice(0, 30).map((q) => {
            const timeStr = q.time ? new Date(q.time).toLocaleTimeString() : '';
            return h('div', { class: 'device' },
              h('span', { class: 'device-icon' }, icon(q.blocked ? 'shieldOff' : 'check', 16)),
              h('div', { class: 'device-text' },
                h('div', { class: 'device-name' },
                  q.domain,
                  q.blocked
                    ? h('span', { class: 'pill danger', style: { background: 'rgba(239, 68, 68, 0.15)', color: '#ef4444' } }, 'Blocked')
                    : h('span', { class: 'pill' }, 'Allowed')
                ),
                h('div', { class: 'set-desc' }, `${q.type || 'A'} query from ${q.client} · ${q.duration_ms}ms · ${timeStr}`)
              )
            );
          }));
        }
      } catch (err) {
        report(err);
      }
    }

    el.replaceChildren(h('div', { class: 'settings-page' },
      group('DNS Sinkhole & AdBlocker', 'Block ads, tracking, and telemetry network-wide on all connected devices.', toggleBox),
      group('Activity & Statistics', 'Real-time metrics for network DNS queries.', statsGrid),
      group('Rules & Filtering', 'Manage custom whitelisted and blacklisted domain names.', rulesBox),
      group('Recent DNS Queries', 'Live log of domain lookup requests processed by Ultraspace.', queriesBox)
    ));

    loadStatus();
    timer = setInterval(loadStatus, 5_000);
    return () => clearInterval(timer);
  }

  function hardware(el) {
    const btBox = h('div', { class: 'device-list' }, h('div', { class: 'empty-state small' }, 'Checking Bluetooth…'));
    const btDevList = h('div', { class: 'device-list' });
    const usbList = h('div', { class: 'device-list' }, h('div', { class: 'empty-state small' }, 'Scanning USB devices…'));
    const pciList = h('div', { class: 'device-list' });
    const scanBtn = h('button', { class: 'btn small', onclick: () => scanBT() }, icon('refresh', 14), 'Scan for Bluetooth Devices');
    let timer = null;

    async function loadStatus() {
      try {
        const st = await api('GET', '/api/hardware/status');

        if (!st.bluetooth || !st.bluetooth.available) {
          btBox.replaceChildren(h('div', { class: 'device' },
            h('span', { class: 'device-icon' }, icon('bluetooth', 20)),
            h('div', { class: 'device-text' },
              h('div', { class: 'device-name' }, 'Bluetooth adapter not detected'),
              h('div', { class: 'set-desc' }, 'No BlueZ Bluetooth controller was found on this server.'))));
          btDevList.replaceChildren();
          scanBtn.disabled = true;
        } else {
          scanBtn.disabled = !st.bluetooth.powered;
          const powerSwitch = h('input', {
            type: 'checkbox', class: 'switch', checked: st.bluetooth.powered,
            onchange: async (e) => {
              try {
                await api('POST', '/api/hardware/bluetooth/power', { powered: e.target.checked });
                toast(`Bluetooth turned ${e.target.checked ? 'on' : 'off'}`);
                await loadStatus();
              } catch (err) { report(err); }
            },
          });

          btBox.replaceChildren(h('div', { class: 'device current' },
            h('span', { class: 'device-icon' }, icon('bluetooth', 20)),
            h('div', { class: 'device-text' },
              h('div', { class: 'device-name' }, st.bluetooth.name || 'Bluetooth Adapter', h('span', { class: `pill ${st.bluetooth.powered ? 'active' : ''}` }, st.bluetooth.powered ? 'Powered On' : 'Powered Off')),
              h('div', { class: 'set-desc' }, `MAC: ${st.bluetooth.mac || 'Unknown'} · ${st.bluetooth.pairable ? 'Pairable' : 'Not pairable'}`)),
            h('label', { class: 'switch-row', style: { margin: 0, padding: 0 } }, powerSwitch)));

          if (st.bluetooth_devices && st.bluetooth_devices.length > 0) {
            btDevList.replaceChildren(...st.bluetooth_devices.map((d) => {
              const connectBtn = d.connected
                ? h('button', { class: 'btn small', onclick: () => disconnectBT(d) }, 'Disconnect')
                : h('button', { class: 'btn small primary', onclick: () => connectBT(d) }, 'Connect');
              const removeBtn = h('button', { class: 'btn small', onclick: () => removeBT(d) }, icon('trash', 14));

              return h('div', { class: `device ${d.connected ? 'current' : ''}` },
                h('span', { class: 'device-icon' }, icon('bluetooth', 18)),
                h('div', { class: 'device-text' },
                  h('div', { class: 'device-name' }, d.name || d.mac, d.connected ? h('span', { class: 'pill active' }, 'Connected') : d.paired ? h('span', { class: 'pill' }, 'Paired') : null),
                  h('div', { class: 'set-desc' }, `${d.mac} ${d.trusted ? '· Trusted' : ''}`)),
                h('div', { class: 'hero-actions' }, connectBtn, removeBtn));
            }));
          } else {
            btDevList.replaceChildren(h('div', { class: 'empty-state small' }, st.bluetooth.powered ? 'No paired devices. Click “Scan” to discover nearby devices.' : 'Bluetooth is powered off.'));
          }
        }

        if (st.usb_devices && st.usb_devices.length > 0) {
          usbList.replaceChildren(...st.usb_devices.map((u) => {
            let uIcon = 'usb';
            if (u.category === 'Storage') uIcon = 'hardDrive';
            else if (u.category === 'Audio') uIcon = 'volume';
            else if (u.category === 'Input') uIcon = 'sliders';

            return h('div', { class: 'device' },
              h('span', { class: 'device-icon' }, icon(uIcon, 18)),
              h('div', { class: 'device-text' },
                h('div', { class: 'device-name' }, u.description, h('span', { class: 'pill' }, u.category)),
                h('div', { class: 'set-desc' }, `ID: ${u.id} · Bus ${u.bus} Device ${u.device}`)));
          }));
        } else {
          usbList.replaceChildren(h('div', { class: 'empty-state small' }, 'No external USB devices detected.'));
        }

        if (st.pci_devices && st.pci_devices.length > 0) {
          pciList.replaceChildren(...st.pci_devices.map((p) => {
            return h('div', { class: 'device' },
              h('span', { class: 'device-icon' }, icon('cpu', 18)),
              h('div', { class: 'device-text' },
                h('div', { class: 'device-name' }, p.description, h('span', { class: 'pill' }, p.class)),
                h('div', { class: 'set-desc' }, `Slot: ${p.slot}`)));
          }));
        } else {
          pciList.replaceChildren(h('div', { class: 'empty-state small' }, 'No PCI controllers enumerated.'));
        }
      } catch (err) {
        report(err);
      }
    }

    async function scanBT() {
      scanBtn.disabled = true;
      scanBtn.textContent = 'Scanning (5s)…';
      try {
        const { devices } = await api('POST', '/api/hardware/bluetooth/scan');
        toast(`Found ${devices ? devices.length : 0} Bluetooth devices`);
        await loadStatus();
      } catch (err) {
        report(err);
      } finally {
        scanBtn.disabled = false;
        scanBtn.replaceChildren(icon('refresh', 14), 'Scan for Bluetooth Devices');
      }
    }

    async function connectBT(d) {
      try {
        await api('POST', '/api/hardware/bluetooth/connect', { mac: d.mac });
        toast(`Connected to ${d.name || d.mac}`);
        await loadStatus();
      } catch (err) {
        report(err);
      }
    }

    async function disconnectBT(d) {
      try {
        await api('POST', '/api/hardware/bluetooth/disconnect', { mac: d.mac });
        toast(`Disconnected from ${d.name || d.mac}`);
        await loadStatus();
      } catch (err) {
        report(err);
      }
    }

    async function removeBT(d) {
      if (!(await confirmDialog(`Unpair ${d.name || d.mac}?`, 'This Bluetooth device will be removed from known devices.', 'Unpair'))) return;
      try {
        await api('POST', '/api/hardware/bluetooth/remove', { mac: d.mac });
        toast(`Unpaired ${d.name || d.mac}`);
        await loadStatus();
      } catch (err) {
        report(err);
      }
    }

    el.replaceChildren(h('div', { class: 'settings-page' },
      group('Bluetooth Controller', 'Manage laptop Bluetooth radio and settings.', btBox),
      group('Bluetooth Devices', 'Paired wireless headphones, speakers, keyboards, and accessories.', btDevList,
        h('div', { class: 'hero-actions', style: { marginTop: '12px' } }, scanBtn)),
      group('Connected USB Devices', 'Plugged in flash drives, external hard disks, USB soundcards, and peripherals.', usbList),
      group('Internal PCI & GPU Controllers', 'Onboard graphics chipsets, network controllers, and motherboard buses.', pciList)));

    loadStatus();
    timer = setInterval(loadStatus, 20_000);
    return () => clearInterval(timer);
  }

  function account(el) {
    const cur = h('input', { class: 'input', type: 'password', autocomplete: 'current-password' });
    const next = h('input', { class: 'input', type: 'password', autocomplete: 'new-password' });
    const again = h('input', { class: 'input', type: 'password', autocomplete: 'new-password' });
    const error = h('div', { class: 'form-error' });
    const submit = h('button', { class: 'btn primary', type: 'submit' }, 'Change password');
    const form = h('form', {
      class: 'set-form',
      onsubmit: async (e) => {
        e.preventDefault();
        error.textContent = '';
        if (next.value !== again.value) { error.textContent = 'The new passwords do not match.'; return; }
        submit.disabled = true;
        try {
          await api('POST', '/api/account/password', { current: cur.value, new: next.value });
          cur.value = next.value = again.value = '';
          toast('Password changed. Your other devices were signed out.');
        } catch (err) { error.textContent = err.message; }
        submit.disabled = false;
      },
    },
      h('label', { class: 'field-label' }, 'Current password'), cur,
      h('label', { class: 'field-label' }, 'New password (at least 10 characters)'), next,
      h('label', { class: 'field-label' }, 'Confirm new password'), again,
      error, submit);
    el.replaceChildren(h('div', { class: 'settings-page' },
      group('Signed in as', null, h('div', { class: 'account-card' },
        h('span', { class: 'avatar' }, (window.ultraspaceUser?.username || '?').slice(0, 1).toUpperCase()),
        h('div', null, h('div', { class: 'account-name' }, window.ultraspaceUser?.username || ''), h('div', { class: 'set-desc' }, 'Administrator')),
        h('div', { class: 'tb-spacer' }),
        h('button', { class: 'btn', onclick: () => emit('signout') }, icon('logOut', 15), 'Sign out'))),
      group('Password', 'Changing your password signs out every other device.', form),
      patternGroup()));
    return null;
  }

  // ---------- pattern lock ----------

  function patternGroup() {
    const on = !!window.ultraspaceUser?.pattern;
    const box = h('div', { class: 'switch-row pattern-row' },
      h('span', null, h('b', null, on ? 'Pattern lock is on' : 'Pattern lock is off'),
        h('span', { class: 'set-desc' }, on
          ? 'After your password, sign-in asks for your pattern. Forgot it? On the server run: sudo -u ultraspace ultraspace reset-pattern -user ' + (window.ultraspaceUser?.username || 'NAME')
          : 'Ask for a 3×3 pattern after the password when signing in. A stolen password alone is then not enough.')),
      h('div', { class: 'hero-actions' },
        h('button', { class: `btn ${on ? '' : 'primary'}`, onclick: () => setupPattern() }, on ? 'Change' : 'Set up'),
        on ? h('button', { class: 'btn', onclick: () => removePattern() }, 'Turn off') : null));
    return group('Pattern lock', null, box);
  }

  function setupPattern() {
    let first = null;
    const msg = h('p', { class: 'dialog-text pattern-msg' }, 'Draw a pattern joining at least 4 dots.');
    const pwd = h('input', { class: 'input hidden', type: 'password', placeholder: 'Your current password', autocomplete: 'current-password' });
    const save = h('button', { class: 'btn primary', disabled: true }, 'Save');
    const pad = patternPad({
      onDone: (seq) => {
        if (seq.length < 4) { msg.textContent = 'Join at least 4 dots. Try again.'; pad.error(); return; }
        if (!first) { first = seq.join(','); msg.textContent = 'Draw it again to confirm.'; setTimeout(() => pad.clear(), 350); return; }
        if (seq.join(',') !== first) { first = null; msg.textContent = 'The patterns did not match. Start again.'; pad.error(); return; }
        pad.ok();
        msg.textContent = 'Pattern confirmed. Enter your password to save it.';
        pwd.classList.remove('hidden');
        save.disabled = false;
        pwd.focus();
      },
    });
    save.addEventListener('click', async () => {
      save.disabled = true;
      try {
        await api('POST', '/api/account/pattern', { password: pwd.value, pattern: first });
        window.ultraspaceUser.pattern = true;
        close();
        toast('Pattern lock is on');
        show('account');
      } catch (err) { msg.textContent = err.message; save.disabled = false; }
    });
    pwd.addEventListener('keydown', (e) => { if (e.key === 'Enter' && !save.disabled) save.click(); });
    const close = dialog({
      title: window.ultraspaceUser?.pattern ? 'Change pattern' : 'Set up pattern lock', width: '360px',
      body: h('div', { class: 'pattern-setup' }, msg, pad.el, pwd),
      actions: [h('button', { class: 'btn', onclick: () => close() }, 'Cancel'), save],
    });
  }

  function removePattern() {
    const pwd = h('input', { class: 'input', type: 'password', placeholder: 'Your current password', autocomplete: 'current-password' });
    const msg = h('div', { class: 'form-error' });
    const ok = h('button', {
      class: 'btn danger-solid',
      onclick: async () => {
        try {
          await api('POST', '/api/account/pattern', { password: pwd.value, pattern: '' });
          window.ultraspaceUser.pattern = false;
          close();
          toast('Pattern lock is off');
          show('account');
        } catch (err) { msg.textContent = err.message; }
      },
    }, 'Turn off');
    pwd.addEventListener('keydown', (e) => { if (e.key === 'Enter') ok.click(); });
    const close = dialog({
      title: 'Turn off pattern lock?', width: '360px',
      body: h('div', null, h('p', { class: 'dialog-text' }, 'Sign-in will ask only for your password.'), pwd, msg),
      actions: [h('button', { class: 'btn', onclick: () => close() }, 'Cancel'), ok],
    });
    requestAnimationFrame(() => pwd.focus());
  }

  function devices(el) {
    const list = h('div', { class: 'device-list' }, h('div', { class: 'empty-state small' }, 'Loading…'));
    const othersBtn = h('button', { class: 'btn', onclick: () => signOutOthers() }, icon('logOut', 15), 'Sign out all other devices');
    let timer = null;

    async function load() {
      try {
        const { sessions } = await api('GET', '/api/account/sessions');
        othersBtn.disabled = sessions.length < 2;
        list.replaceChildren(...sessions.map((s) => {
          const mobile = /Mobile|Android|iPhone|iPad/.test(s.user_agent);
          return h('div', { class: `device ${s.current ? 'current' : ''}` },
            h('span', { class: 'device-icon' }, icon(mobile ? 'smartphone' : 'laptop', 20)),
            h('div', { class: 'device-text' },
              h('div', { class: 'device-name' }, s.user_agent ? deviceName(s.user_agent) : 'Unknown device', s.current ? h('span', { class: 'pill active' }, 'This device') : null),
              h('div', { class: 'set-desc' }, [s.ip, s.current ? 'active now' : s.last_seen ? `active ${fmtAgo(s.last_seen)}` : '', `signed in ${fmtDate(s.created)}`].filter(Boolean).join(' · '))),
            s.current ? null : h('button', { class: 'btn small', onclick: () => revoke(s) }, 'Sign out'));
        }));
      } catch (err) { report(err); }
    }

    async function revoke(s) {
      try {
        await api('POST', '/api/account/sessions/revoke', { id: s.id });
        toast(`Signed out ${deviceName(s.user_agent)}`);
      } catch (err) { report(err); }
      load();
    }

    async function signOutOthers() {
      if (!(await confirmDialog('Sign out other devices?', 'Every other browser signed in to ultraspace will need your password again.', 'Sign out'))) return;
      try {
        await api('POST', '/api/account/sessions/revoke', { others: true });
        toast('Signed out all other devices');
      } catch (err) { report(err); }
      load();
    }

    el.replaceChildren(h('div', { class: 'settings-page' },
      group('Your session follows you',
        'Open windows, the song and position in Music, and unsaved text in the editor are saved to this server every few seconds. The Browser, Terminal sessions and VS Code run on the server itself, so their tabs, logins, running programs and open files are simply there on every device. A device stays signed in until it has been unused for 30 days.'),
      group('Signed-in devices', 'Sign out any device you no longer use, for example a shared computer.', list, othersBtn)));
    load();
    timer = setInterval(load, 30_000);
    return () => clearInterval(timer);
  }

  // ---------- Ultra Chat Bot ----------

  function chatbot(el) {
    const KEYS = {
      gemini: { label: 'Gemini API key', where: 'aistudio.google.com/apikey' },
      mistral: { label: 'Mistral API key', where: 'console.mistral.ai → API keys' },
      groq: { label: 'Groq API key', where: 'console.groq.com/keys (free tier, very fast)' },
      openrouter: { label: 'OpenRouter API key', where: 'openrouter.ai/keys (only its free models are listed)' },
      brave: { label: 'Brave Search API key (optional, for web search)', where: 'api-dashboard.search.brave.com' },
    };
    const render = async () => {
      let c;
      try { c = await api('GET', '/api/ai/config'); } catch (err) { report(err); return; }
      chatbotConfigChanged(c);
      const all = [...c.providers, c.search];
      const toggle = h('label', { class: 'switch-row' },
        h('span', null, h('b', null, 'Show the chat bubble'), h('span', { class: 'set-desc' }, 'A floating Ultra Chat Bot button at the bottom-right. Ctrl+J opens it.')),
        h('input', { type: 'checkbox', class: 'switch', checked: c.enabled, onchange: async (e) => { await api('PUT', '/api/ai/config', { enabled: e.target.checked }).catch(report); render(); } }));
      const rows = all.map((p) => {
        const meta = KEYS[p.id] || { label: `${p.name} key`, where: '' };
        const input = h('input', { class: 'input', type: 'password', placeholder: p.configured ? `Saved (${p.hint}) — paste a new key to replace` : 'Paste the key', autocomplete: 'off', spellcheck: 'false' });
        const save = async () => {
          if (!input.value.trim()) return;
          try { await api('PUT', '/api/ai/config', { keys: { [p.id]: input.value.trim() } }); toast(`${p.name} key saved`); render(); } catch (err) { report(err); }
        };
        input.addEventListener('keydown', (e) => { if (e.key === 'Enter') save(); });
        return h('div', { class: 'ai-key' },
          h('label', { class: 'field-label' }, meta.label, p.configured ? h('span', { class: 'pill active' }, `saved ${p.hint}`) : null),
          h('div', { class: 'ai-key-row' }, input,
            h('button', { class: 'btn primary small', onclick: save }, 'Save'),
            p.configured ? h('button', {
              class: 'btn small', onclick: async () => {
                if (!(await confirmDialog(`Remove the ${p.name} key?`, 'It is deleted from the server.', 'Remove'))) return;
                await api('PUT', '/api/ai/config', { keys: { [p.id]: '' } }).catch(report);
                render();
              },
            }, 'Remove') : null),
          meta.where ? h('div', { class: 'set-desc' }, `Get one at ${meta.where}`) : null);
      });
      el.replaceChildren(h('div', { class: 'settings-page' },
        group('Ultra Chat Bot', 'Chat with Gemini, Mistral, Groq or OpenRouter models from anywhere on the desktop. Keys are stored on your server and never sent to the browser; the server talks to the provider for you.', toggle),
        group('API keys', null, h('div', { class: 'ai-keys' }, rows)),
        group('Privacy', 'What you type, and any window you choose to include, is sent to the provider of the model you pick (Google, Mistral, Groq or OpenRouter, which passes it on to the model’s host). With web search on, your question also goes to Brave.')));
    };
    render();
    return null;
  }

  function about(el) {
    const kv = h('dl', { class: 'kv' });
    const render = (s) => {
      if (!s) return;
      kv.replaceChildren(...[
        ['Version', `ultraspace ${s.app.version}`],
        ['Memory used by ultraspace', fmtBytes(s.app.rss)],
        ['Server', s.hostname],
        ['Operating system', s.os],
        ['Kernel', s.kernel],
        ['Uptime', fmtDuration(s.uptime)],
        ['CPU cores', s.cpu.cores],
        ['Memory', fmtBytes(s.memory.total)],
      ].flatMap(([k, v]) => [h('dt', null, k), h('dd', null, v)]));
    };
    render(system.latest);
    el.replaceChildren(h('div', { class: 'settings-page' },
      h('div', { class: 'about-hero' }, h('span', { class: 'logo-mark big' }),
        h('div', null, h('div', { class: 'about-name' }, 'ultraspace'), h('div', { class: 'set-desc' }, 'Your personal server, in one small Go binary.'))),
      group('This server', null, kv)));
    return on('system', render);
  }
}
