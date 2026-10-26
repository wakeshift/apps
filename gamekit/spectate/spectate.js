// The public pages, live (spectate.go): a game's page fetches its state
// every two seconds, the list of games every ten, and each redraws what
// changed. All text is set with textContent: what players say is never
// markup. Without this script, the pages reload themselves. A game drawn
// as a table (Server.Table) gets each view as a "gamestate" event, and its
// own script draws the table; its follow-along page is drawn here too.
(() => {
  const body = document.body;
  const el = (tag, cls, text) => { const e = document.createElement(tag); if (cls) e.className = cls; if (text != null) e.textContent = text; return e; };
  const avatar = (src) => { const i = el('img', 'avatar'); i.src = src; i.alt = ''; return i; };
  const time = (at) => {
    const t = el('time', 'time', new Date(at).toLocaleTimeString([], { hour: 'numeric', minute: '2-digit', second: '2-digit' }));
    t.dateTime = at;
    return t;
  };
  const byId = (id) => document.getElementById(id);

  // Times as the reader's clock has them.
  document.querySelectorAll('time.time').forEach((t) => t.replaceWith(time(t.dateTime)));

  // A secret line (a codeword, say) on the follow-along page: shown while
  // it's hovered or focused, or for good once tapped.
  let revealed = false;
  const secret = (b) => {
    b.classList.toggle('shown', revealed);
    b.setAttribute('aria-pressed', revealed ? 'true' : 'false');
    b.addEventListener('click', () => {
      revealed = !revealed;
      document.querySelectorAll('button.secret').forEach((x) => { x.classList.toggle('shown', revealed); x.setAttribute('aria-pressed', revealed ? 'true' : 'false'); });
    });
  };
  document.querySelectorAll('button.secret').forEach(secret);

  // The transcript: on a page with a table it scrolls in its own box, which
  // starts at the newest message and follows new ones unless the reader has
  // scrolled up; otherwise the page itself scrolls.
  const boxed = (log) => log && getComputedStyle(log).overflowY !== 'visible' && log.scrollHeight > log.clientHeight;
  const logAtEnd = (log) => boxed(log) ? log.scrollTop + log.clientHeight >= log.scrollHeight - 40 : window.innerHeight + window.scrollY >= document.body.scrollHeight - 80;
  if (boxed(byId('log'))) byId('log').scrollTop = byId('log').scrollHeight;

  const game = body.dataset.state;
  const tabled = body.classList.contains('tabled');
  if (game) {
    let seen = '';
    const render = (s) => {
      const sub = byId('sub');
      sub.replaceChildren(el('span', 'live-dot'), 'Game ' + s.game + ' · ' + s.status);
      sub.firstChild.setAttribute('aria-hidden', 'true');

      const res = byId('result');
      res.hidden = !(s.result && s.result.length);
      res.replaceChildren(...(s.result || []).map((t) => el('div', null, t)));

      const you = byId('you');
      if (you && s.you) {
        const lines = el('div', 'lines');
        for (const l of s.you) {
          const d = el('div', 'line' + (l.tone ? ' ' + l.tone : ''));
          if (l.label) d.append(el('span', 'label', l.label));
          if (l.secret) {
            const b = el('button', 'secret');
            b.type = 'button';
            b.append(el('span', 'value', l.text), el('span', 'hint', 'tap to show'));
            secret(b);
            d.append(b);
          } else d.append(el('span', 'value', l.text));
          lines.append(d);
        }
        you.replaceChildren(lines);
      }

      const about = byId('about');
      if (about) {
        about.hidden = !(s.about && s.about.length);
        const lines = el('div', 'lines');
        for (const l of s.about || []) {
          const d = el('div', 'line' + (l.tone ? ' ' + l.tone : ''));
          if (l.label) d.append(el('span', 'label', l.label));
          d.append(el('span', 'value', l.text));
          lines.append(d);
        }
        about.replaceChildren(el('div', 'h', 'The game'), lines);
      }

      const pics = {};
      const list = el('div', 'list');
      for (const p of s.players) {
        if (p.avatar) pics[p.handle] = p.avatar;
        const d = el('span', 'player');
        if (p.avatar) d.append(avatar(p.avatar));
        d.append(el('span', 'name', p.handle));
        if (p.note) d.append(el('span', 'note', p.note));
        list.append(d);
      }
      if (byId('players')) byId('players').replaceChildren(el('div', 'h', 'Players'), list);
      const me = s.players.find((p) => p.you);

      // The player's own private reasoning: only on its follow-along page.
      const thinking = s.thinking || [];
      const key = s.log.length + '|' + (s.log.length ? s.log[s.log.length - 1].at : '') + '|' + thinking.length + '|' + (thinking.length ? thinking[thinking.length - 1].at : '') + '|' + Object.keys(pics).length;
      if (key !== seen) {
        const log = byId('log');
        const atEnd = logAtEnd(log);
        const first = seen === '';
        seen = key;
        log.replaceChildren(el('div', 'h', 'Transcript'));
        if (!s.log.length && !thinking.length) log.append(el('div', 'gone', 'Nothing said yet.'));
        const items = [...s.log, ...thinking.map((t) => ({ ...t, thought: true }))]
          .map((m, i) => [m, i])
          .sort((a, b) => new Date(a[0].at) - new Date(b[0].at) || a[1] - b[1])
          .map((x) => x[0]);
        for (const m of items) {
          if (m.thought) {
            const d = el('div', 'entry thought');
            d.append(el('span', 'from', 'Thinking' + (m.tool && m.tool !== 'say' ? ' (' + m.tool + ')' : '')), el('span', 'text', m.text), time(m.at));
            log.append(d);
            continue;
          }
          const d = el('div', 'entry' + (m.from ? '' : ' mod') + (me && m.from === me.handle ? ' mine' : ''));
          if (pics[m.from]) d.append(avatar(pics[m.from]));
          d.append(el('span', 'from', m.from || 'Moderator'), el('span', 'text', m.text), time(m.at));
          log.append(d);
        }
        if (boxed(log)) {
          if (first || atEnd) log.scrollTop = log.scrollHeight;
        } else if (!first && atEnd) window.scrollTo(0, document.body.scrollHeight);
      }
      // The game's own script (a table) draws the rest.
      document.dispatchEvent(new CustomEvent('gamestate', { detail: s }));
    };
    const poll = async () => {
      try {
        const r = await fetch(game, { cache: 'no-store' });
        const s = await r.json();
        if (!r.ok) {
          byId('sub').textContent = s.error;
          if (!s.retry) return;
        } else render(s);
      } catch (e) {}
      setTimeout(poll, 2000);
    };
    // A table is drawn live from the first view: at once.
    setTimeout(poll, tabled ? 0 : 2000);
  }

  const listURL = body.dataset.list;
  if (listURL) {
    const row = (g) => {
      const a = el('a', 'row');
      a.href = g.url;
      const top = el('span', 'g-top');
      top.append(el('span', 'g-status', g.status));
      const players = el('span', 'g-players');
      g.players.forEach((p, i) => {
        if (i) players.append(', ');
        const n = el('span', 'g-player', p.handle);
        players.append(n);
      });
      a.append(top, players, el('span', 'g-watch', (g.over ? 'See how it went' : 'Watch') + ' →'));
      const li = el('li');
      li.append(a);
      return li;
    };
    const poll = async () => {
      try {
        const r = await fetch(listURL, { cache: 'no-store' });
        if (r.ok) {
          const l = await r.json();
          byId('live').replaceChildren(...l.live.map(row));
          byId('live').hidden = !l.live.length;
          byId('live-empty').hidden = !!l.live.length;
          byId('finished').replaceChildren(...l.finished.map(row));
          byId('finished-card').hidden = !l.finished.length;
        }
      } catch (e) {}
      setTimeout(poll, 10000);
    };
    setTimeout(poll, 10000);
  }
})();
