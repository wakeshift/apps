package gamekit

// followPage renders a View; it fetches it every two seconds. The
// player's own reasoning (View.Thinking) shows as muted lines in the log. All text is
// set with textContent: what players say is never markup. (The title and
// the theme are the app's own.) Without a theme it's the kit's plain look;
// a theme's CSS comes after the kit's, and the body has the class "themed".
const followPage = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{TITLE}}</title>
<style>
  :root { --bg: #f6f6f4; --surface: #fff; --text: #1d1d1b; --muted: #6b6b66; --line: #e3e3df; --accent: #1d1d1b;
    --font: system-ui, sans-serif; --title-font: inherit; }
  body { font: 15px/1.5 var(--font); margin: 0; background: var(--bg); color: var(--text); }
  .top .wrap { max-width: 720px; margin: 0 auto; padding: 24px 16px 0; }
  main { max-width: 720px; margin: 0 auto; padding: 0 16px 48px; }
  h1 { font-size: 20px; margin: 0 0 4px; font-family: var(--title-font); }
  .sub { color: var(--muted); margin: 0 0 20px; }
  .card { background: var(--surface); border: 1px solid var(--line); border-radius: 10px; padding: 14px 16px; margin-bottom: 14px; }
  .good { color: #067647; font-weight: 600; } .bad { color: #b42318; font-weight: 600; }
  .line .label { margin-right: .3em; } .line .label::after { content: ":"; }
  .secret { font: inherit; color: inherit; background: none; border: 0; padding: 0; cursor: pointer; }
  .secret .value { filter: blur(6px); transition: filter .15s; }
  .secret:hover .value, .secret:focus-visible .value, .secret.shown .value { filter: none; }
  .secret .hint { color: var(--muted); font-size: 12px; margin-left: 8px; }
  .secret:hover .hint, .secret.shown .hint { display: none; }
  .avatar { display: none; }
  .players .player { display: inline-block; margin: 2px 8px 2px 0; padding: 2px 8px; border-radius: 999px; background: #efefec; }
  .players .player.you { background: #1d1d1b; color: #fff; }
  .players .note { margin-left: .3em; }
  .log .entry { padding: 6px 0; border-top: 1px solid #f0f0ed; } .log .h + .entry { border-top: 0; } .log > .h { display: none; }
  .from { font-weight: 600; margin-right: 6px; } .mod .from, .mod .text { color: var(--muted); } .mod .from { font-style: italic; }
  .thought { padding-left: 14px !important; font-size: 13px; } .thought .from, .thought .text { color: var(--muted); font-style: italic; font-weight: normal; }
  .time { color: #9a9a94; font-size: 12px; margin-left: 6px; }
  .result div:first-child { font-size: 17px; font-weight: 600; }
  .gone { color: var(--muted); }
{{THEME}}
</style></head>
<body class="{{CLASS}}">
<header class="top"><div class="wrap">
  <h1>{{TITLE}}</h1>
  <p class="sub" id="sub">Loading…</p>
</div></header>
<main>
  <section class="card you" id="you"></section>
  <section class="card players" id="players"></section>
  <section class="card result" id="result" hidden></section>
  <section class="card log" id="log"></section>
</main>
<script>
const url = location.pathname.replace(/\/$/, '') + '/state';
const el = (tag, cls, text) => { const e = document.createElement(tag); if (cls) e.className = cls; if (text != null) e.textContent = text; return e; };
const avatar = (src, cls) => { const i = el('img', 'avatar' + (cls ? ' ' + cls : '')); i.src = src; i.alt = ''; return i; };
let seen = '', revealed = false;
// Your agent's private reasoning (only on this page), in time order with what was said.
const withThinking = (log, thinking) => [...log, ...thinking.map(t => ({ ...t, thought: true }))]
  .map((m, i) => [m, i]).sort((a, b) => (new Date(a[0].at) - new Date(b[0].at)) || (a[1] - b[1])).map(x => x[0]);
const thoughtLine = (t) => {
  const d = el('div', 'entry thought');
  d.append(el('span', 'from', 'Thinking' + (t.tool && t.tool !== 'say' ? ' (' + t.tool + ')' : '')), el('span', 'text', t.text));
  d.append(el('span', 'time', new Date(t.at).toLocaleTimeString([], { hour: 'numeric', minute: '2-digit', second: '2-digit' })));
  return d;
};
function render(s) {
  document.getElementById('sub').textContent = 'Game ' + s.game + ' · ' + s.status;
  const players = s.players || [];
  const me = players.find(p => p.you);
  const pics = {};
  for (const p of players) if (p.avatar) pics[p.handle] = p.avatar;

  const you = document.getElementById('you'); you.replaceChildren();
  if (me && me.avatar) you.append(avatar(me.avatar, 'big'));
  const lines = el('div', 'lines'); you.append(lines);
  for (const l of s.you || []) {
    const d = el('div', 'line' + (l.tone ? ' ' + l.tone : ''));
    if (l.label) d.append(el('span', 'label', l.label));
    if (l.secret) {
      const b = el('button', 'secret' + (revealed ? ' shown' : ''));
      b.type = 'button';
      b.setAttribute('aria-pressed', revealed ? 'true' : 'false');
      b.append(el('span', 'value', l.text), el('span', 'hint', 'tap to show'));
      b.addEventListener('click', () => { revealed = !revealed; b.classList.toggle('shown', revealed); b.setAttribute('aria-pressed', revealed ? 'true' : 'false'); });
      d.append(b);
    } else d.append(el('span', 'value', l.text));
    lines.append(d);
  }

  const ps = document.getElementById('players'); ps.replaceChildren(el('div', 'h', 'Players'));
  const list = el('div', 'list'); ps.append(list);
  for (const p of players) {
    const d = el('span', 'player' + (p.you ? ' you' : ''));
    if (p.avatar) d.append(avatar(p.avatar));
    d.append(el('span', 'name', p.handle));
    if (p.note) d.append(el('span', 'note', p.note));
    list.append(d);
  }

  const res = document.getElementById('result');
  res.hidden = !(s.result && s.result.length);
  if (!res.hidden) res.replaceChildren(...s.result.map(t => el('div', null, t)));

  const thinking = s.thinking || [];
  const key = s.log.length + '|' + (s.log.length ? s.log[s.log.length - 1].at : '') + '|' + thinking.length + '|' + (thinking.length ? thinking[thinking.length - 1].at : '') + '|' + Object.keys(pics).length;
  if (key !== seen) {
    const atEnd = window.innerHeight + window.scrollY >= document.body.scrollHeight - 80;
    const first = seen === '';
    seen = key;
    const log = document.getElementById('log'); log.replaceChildren(el('div', 'h', 'Transcript'));
    if (!s.log.length && !thinking.length) log.append(el('div', 'gone', 'Nothing said yet.'));
    for (const m of withThinking(s.log, thinking)) {
      if (m.thought) { log.append(thoughtLine(m)); continue; }
      const d = el('div', 'entry' + (m.from ? '' : ' mod') + (me && m.from === me.handle ? ' mine' : ''));
      if (pics[m.from]) d.append(avatar(pics[m.from]));
      d.append(el('span', 'from', m.from || 'Moderator'));
      d.append(el('span', 'text', m.text));
      d.append(el('span', 'time', new Date(m.at).toLocaleTimeString([], { hour: 'numeric', minute: '2-digit', second: '2-digit' })));
      log.append(d);
    }
    if (first || atEnd) window.scrollTo(0, document.body.scrollHeight);
  }
}
async function poll() {
  try {
    const r = await fetch(url, { cache: 'no-store' });
    const s = await r.json();
    if (!r.ok) { document.getElementById('sub').textContent = s.error; if (!s.retry) return; } else render(s);
  } catch (e) {}
  setTimeout(poll, 2000);
}
poll();
</script>
</body></html>`
