package network

// portalPage is the setup screen shown to a phone that joins Ultraspace-Setup.
// It uses the same dark glass, accent, and type as the desktop, and has no
// dependency on the signed-in app.
var portalPage = []byte(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>ultraspace setup</title>
<style>
  :root { color-scheme: dark; --accent: #7c6cf2; --text: #f1f1f5; --text-2: #b4b4c2; --muted: #8b8b9c; --field: rgba(255,255,255,.06); --border: rgba(255,255,255,.08); --hover: rgba(255,255,255,.06); }
  * { box-sizing: border-box; }
  body { margin: 0; min-height: 100%; font: 14px/1.4 Inter, system-ui, -apple-system, "Segoe UI", sans-serif; color: var(--text); background: #0b1026; }
  .wrap { min-height: 100vh; display: grid; place-items: center; padding: 24px 16px; }
  .card { width: min(420px, 100%); padding: 22px; border-radius: 16px; background: rgba(24,24,32,.92); border: 1px solid rgba(255,255,255,.12); box-shadow: 0 30px 80px rgba(0,0,0,.55); }
  .brand { display: flex; align-items: center; gap: 12px; margin-bottom: 14px; }
  .mark { width: 36px; height: 36px; border-radius: 12px; position: relative; background: linear-gradient(135deg, #a78bfa, var(--accent) 55%, #22d3ee); box-shadow: inset 0 0 0 1px rgba(255,255,255,.25); }
  .mark::before { content: ""; position: absolute; inset: 25%; border-radius: 50%; border: 3px solid rgba(255,255,255,.95); }
  .mark::after { content: ""; position: absolute; width: 22%; height: 22%; right: 14%; top: 14%; border-radius: 50%; background: #fff; }
  h1 { margin: 0; font-size: 16px; font-weight: 650; }
  .sub { color: var(--muted); font-size: 12px; }
  .lead { margin: 0 0 14px; color: var(--text-2); font-size: 13px; }
  .nets { display: flex; flex-direction: column; gap: 8px; margin: 0 0 12px; }
  .net { display: flex; align-items: center; gap: 12px; width: 100%; text-align: left; padding: 12px 14px; border-radius: 12px; background: #1d1d25; border: 1px solid var(--border); color: inherit; font: inherit; cursor: pointer; }
  .net.on { border-color: color-mix(in srgb, var(--accent) 45%, transparent); }
  .net b { display: block; font-weight: 600; }
  .net small { color: var(--muted); }
  label { display: block; margin: 10px 0 4px; font-size: 12px; font-weight: 550; color: var(--text-2); }
  input { width: 100%; height: 32px; padding: 0 11px; border-radius: 8px; background: var(--field); border: 1px solid var(--border); color: inherit; font: inherit; }
  input:focus { outline: none; border-color: var(--accent); box-shadow: 0 0 0 3px color-mix(in srgb, var(--accent) 16%, transparent); }
  .row { display: flex; gap: 8px; margin-top: 14px; }
  button.primary, button.ghost, a.ghost { height: 32px; padding: 0 13px; border-radius: 8px; font: inherit; font-weight: 550; cursor: pointer; }
  button.primary { background: var(--accent); color: #fff; border: 0; }
  button.primary:disabled { opacity: .45; cursor: default; }
  button.ghost, a.ghost { display: inline-flex; align-items: center; background: var(--field); color: inherit; border: 1px solid var(--border); text-decoration: none; }
  .msg { min-height: 18px; margin: 10px 0 0; color: var(--text-2); font-size: 13px; }
  .msg.bad { color: #ef4444; }
  details { margin-top: 14px; color: var(--text-2); }
  summary { cursor: pointer; }
  .done h2 { margin: 8px 0 6px; font-size: 18px; }
  .empty { color: var(--muted); padding: 8px 0 12px; }
</style>
</head>
<body>
<div class="wrap">
  <main class="card">
    <div class="brand"><span class="mark"></span><div><h1>ultraspace</h1><div class="sub">Setup</div></div></div>
    <div id="form">
      <p class="lead">This machine is on Ultraspace-Setup. Choose a network, or type its name. The password is the one for that network, not for Ultraspace-Setup. If this page did not open by itself, go to http://10.42.0.1</p>
      <div id="list" class="nets"></div>
      <label for="ssid">Network name</label>
      <input id="ssid" autocomplete="off" placeholder="Type a network name">
      <label for="pw">Password</label>
      <input id="pw" type="password" autocomplete="off" placeholder="Leave empty for an open network">
      <div class="row"><button id="join" class="primary" disabled>Join</button><a id="open" class="ghost" href="http://10.42.0.1:4600/">Open Ultraspace</a></div>
      <p id="msg" class="msg"></p>
    </div>
    <section id="done" class="done" hidden>
      <h2>Connected</h2>
      <p class="lead">You can leave Ultraspace-Setup and use your usual Wi-Fi.</p>
    </section>
  </main>
</div>
<script>
const list = document.getElementById('list');
const ssid = document.getElementById('ssid');
const pw = document.getElementById('pw');
const join = document.getElementById('join');
const msg = document.getElementById('msg');
let nets = [];

function render() {
  const current = ssid.value.trim();
  list.replaceChildren();
  if (!nets.length) {
    const p = document.createElement('div');
    p.className = 'empty';
    p.textContent = 'No networks are listed. Type the name below. That works for a hidden network too.';
    list.append(p);
    return;
  }
  for (const n of nets) {
    const b = document.createElement('button');
    b.type = 'button';
    b.className = 'net' + (current === n.ssid ? ' on' : '');
    const name = document.createElement('b');
    name.textContent = n.ssid;
    const meta = document.createElement('small');
    meta.textContent = (n.security || 'Open') + ' · ' + (n.signal || 0) + '%';
    const text = document.createElement('span');
    text.append(name, meta);
    b.append(text);
    b.onclick = () => { ssid.value = n.ssid; join.disabled = false; msg.textContent = ''; pw.focus(); render(); };
    list.append(b);
  }
}

ssid.addEventListener('input', () => { join.disabled = !ssid.value.trim(); render(); });

join.onclick = async () => {
  const name = ssid.value.trim();
  if (!name) return;
  const known = nets.some((n) => n.ssid === name);
  join.disabled = true;
  msg.className = 'msg';
  msg.textContent = 'Joining ' + name + '. Ultraspace-Setup will drop. If the password is wrong, it comes back and you can try again.';
  try {
    const res = await fetch('/api/connect', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ ssid: name, password: pw.value, hidden: !known })
    });
    const data = await res.json().catch(() => ({}));
    if (!res.ok) {
      msg.className = 'msg bad';
      msg.textContent = data.error || 'Could not join that network.';
      join.disabled = false;
      return;
    }
    document.getElementById('form').hidden = true;
    document.getElementById('done').hidden = false;
  } catch (e) {
    msg.textContent = 'Ultraspace-Setup dropped while the machine tried to join. If it did not connect, that network comes back and you can try again.';
  }
};

fetch('/api/networks').then((r) => r.json()).then((data) => {
  nets = data.networks || [];
  render();
}).catch(() => {
  msg.textContent = 'Could not list networks. You can still type a name.';
  render();
});
</script>
</body>
</html>
`)
