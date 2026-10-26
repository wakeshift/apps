// Nightfall's table, live (table.go draws it first, for the page without
// JavaScript). The kit's script (spectate.js) fetches the game every two
// seconds and fires "gamestate" with it; this redraws the table in place,
// so a seat that changes can animate: what a player says pops up over
// their seat and fades after ten seconds (or when they speak again), the
// moderator's latest word appears in the middle, a ballot appears by a
// player who has voted, and at the end every card turns over, the votes
// are drawn as arrows between the seats and the winners light up. All
// text is set with textContent: what players say is never markup. With
// reduced motion, nothing moves: the table just updates.
(() => {
  const root = document.getElementById('nf-table');
  if (!root) return;
  const arena = root.querySelector('.arena');
  const still = !!(window.matchMedia && window.matchMedia('(prefers-reduced-motion: reduce)').matches);
  const BUBBLE_MS = 10000; // table.go's bubbleFor
  const names = { 'waiting for players': 'Gathering', night: 'Night', day: 'Day', voting: 'The vote', over: 'Dawn' };
  const svgNS = 'http://www.w3.org/2000/svg';

  const el = (tag, cls, text) => { const e = document.createElement(tag); if (cls) e.className = cls; if (text != null) e.textContent = text; return e; };
  const q = (sel, r) => (r || root).querySelector(sel);
  const cap = (s) => (s ? s[0].toUpperCase() + s.slice(1) : '');
  const initial = (s) => (s ? Array.from(s)[0].toUpperCase() : '?');
  const side = (card) => (card === 'vampire' || card === 'thrall' ? 'coven' : card === 'martyr' ? 'martyr' : 'town');
  // A seat's colour: FNV-1a of its handle, as table.go works it out.
  const colour = (h) => {
    let x = 0x811c9dc5;
    for (const b of new TextEncoder().encode(h)) x = Math.imul(x ^ b, 0x01000193) >>> 0;
    return x % 8;
  };
  // Restarts a one-off animation class on e.
  const replay = (e, cls) => { e.classList.remove(cls); void e.offsetWidth; e.classList.add(cls); };

  // A card: face down, or turned over to role (which animates, if it was down).
  const card = () => {
    const c = el('span', 'tcard'), inner = el('span', 'inner');
    inner.append(el('span', 'back'), el('span', 'face'));
    c.append(inner);
    return c;
  };
  const setCard = (c, role) => {
    const face = q('.face', c);
    if (!role) { c.className = 'tcard'; face.replaceChildren(); return; }
    if (c.dataset.role === role) return;
    c.dataset.role = role;
    face.replaceChildren(el('b', null, initial(role)), el('span', 'name', cap(role)));
    c.className = 'tcard up ' + side(role);
  };

  // The seats, by handle: built once, then changed in place.
  const seats = new Map();
  const seatEl = (h) => {
    let s = seats.get(h);
    if (s) return s;
    const li = el('li', 'seat');
    li.dataset.handle = h;
    const slot = el('div', 'bubble-slot');
    slot.setAttribute('aria-hidden', 'true');
    const plate = el('div', 'plate'), pic = el('span', 'pic', initial(h)), nm = el('span', 'names');
    pic.setAttribute('aria-hidden', 'true');
    nm.append(el('span', 'who', h));
    plate.append(pic, nm);
    const tokens = el('div', 'tokens');
    tokens.append(card(), el('span', 'marks'));
    li.append(slot, plate, tokens);
    s = { li, slot, names: nm, tokens, marks: q('.marks', tokens), card: q('.tcard', tokens), voted: false, bubble: null };
    seats.set(h, s);
    return s;
  };
  // Adopt the seats the server drew, so their cards and ballots carry on.
  root.querySelectorAll('.seat').forEach((li) => {
    const h = li.dataset.handle;
    const s = { li, slot: q('.bubble-slot', li), names: q('.names', li), tokens: q('.tokens', li), voted: li.classList.contains('voted'), bubble: null };
    s.marks = q('.marks', s.tokens);
    s.card = q('.tcard', s.tokens);
    if (s.card.classList.contains('up')) s.card.dataset.role = (q('.name', s.card) || {}).textContent ? q('.name', s.card).textContent.toLowerCase() : '';
    seats.set(h, s);
  });

  // A bubble over a seat for what its player said; it fades after ms.
  const say = (s, text, ms) => {
    if (s.bubble) s.bubble.remove();
    s.slot.replaceChildren();
    const b = el('p', 'bubble');
    b.append(el('span', 'said', text));
    s.slot.append(b);
    s.bubble = b;
    clearTimeout(s.timer);
    s.timer = setTimeout(() => {
      b.classList.add('gone');
      setTimeout(() => { b.remove(); if (s.bubble === b) s.bubble = null; }, still ? 0 : 700);
    }, Math.max(0, ms));
  };

  let first = true, phase = root.dataset.phase, deadline = 0, announced = null;
  const heard = new Map(); // handle → the last message shown as a bubble

  const render = (s) => {
    const t = s.table;
    if (!t) return;
    const was = phase;
    phase = t.phase;
    root.className = 'nf-table phase-' + (t.phase === 'waiting for players' ? 'waiting' : t.phase);
    root.dataset.phase = t.phase;
    arena.className = 'arena n' + t.seats.length;

    // The middle.
    q('.phase-name').textContent = names[t.phase] || cap(t.phase);
    deadline = t.seconds_left > 0 ? Date.now() + t.seconds_left * 1000 : 0;
    tick();
    const pile = q('.pile');
    pile.hidden = !t.center_count;
    while (pile.children.length < t.center_count) pile.append(card());
    Array.from(pile.children).forEach((c, i) => setCard(c, (t.center_cards || [])[i]));
    let mod = null;
    for (const m of s.log) if (!m.from) mod = m;
    const ann = q('.announce');
    if (mod && announced !== mod.at + mod.text) {
      ann.textContent = mod.text;
      if (!first && !still) replay(ann, 'fresh');
      announced = mod.at + mod.text;
    }

    // The seats, in order.
    const list = q('.seats');
    const here = new Set(t.seats.map((p) => p.handle));
    for (const [h, st] of seats) if (!here.has(h)) { st.li.remove(); seats.delete(h); }
    t.seats.forEach((p, i) => {
      const st = seatEl(p.handle);
      if (list.children[i] !== st.li) list.insertBefore(st.li, list.children[i] || null);
      const flags = [p.you && 'you', p.voted && 'voted', p.staked && 'staked', p.won && 'won', p.ended_as && 'shown'].filter(Boolean);
      st.li.className = ['seat', 's' + i, 'c' + colour(p.handle)].concat(flags).join(' ');
      const via = [];
      if (p.your_role) via.push(el('span', 'via mine', 'Your agent · ' + p.your_role));
      st.names.replaceChildren(el('span', 'who', p.handle), ...via);
      setCard(st.card, p.ended_as);
      const marks = [];
      if (p.ended_as) marks.push(el('span', 'role', cap(p.started_as) + (p.ended_as !== p.started_as ? ' → ' + cap(p.ended_as) : '')));
      if (p.voted_for) marks.push(el('span', 'ballot for', 'voted ' + p.voted_for));
      else if (p.voted) {
        const b = el('span', 'ballot', '✓ voted');
        if (!st.voted && !first && !still) b.classList.add('fresh');
        marks.push(b);
      }
      if (p.staked) marks.push(el('span', 'fate dead', 'staked'));
      if (p.won) marks.push(el('span', 'fate win', 'won'));
      st.marks.replaceChildren(...marks);
      st.voted = !!p.voted;
    });

    // What each player said last: a bubble, if it's new (on the first
    // view, if it's under ten seconds old, for what's left of them).
    const last = new Map();
    for (const m of s.log) if (m.from) last.set(m.from, m);
    for (const [h, m] of last) {
      const st = seats.get(h);
      const key = m.at + '|' + m.text;
      if (!st || heard.get(h) === key) continue;
      heard.set(h, key);
      const age = Date.now() - Date.parse(m.at);
      if (first && !(age < BUBBLE_MS)) { st.slot.replaceChildren(); continue; }
      say(st, m.text, first ? BUBBLE_MS - Math.max(0, age) : BUBBLE_MS);
    }
    if (first) for (const [h, st] of seats) if (!last.has(h)) st.slot.replaceChildren();

    // After the seats: the cards in play, and the outcome.
    let inPlay = q('.in-play'), outcome = q('.outcome');
    if (t.in_play && t.in_play.length) {
      if (!inPlay) { inPlay = el('p', 'in-play'); arena.after(inPlay); }
      inPlay.replaceChildren(el('span', 'label', 'Cards in play'), ' ' + t.in_play.join(', ') + ' (' + t.center_count + ' in the center)');
    } else if (inPlay) inPlay.remove();
    if (t.outcome) {
      if (!outcome) { outcome = el('p', 'outcome'); root.append(outcome); }
      outcome.textContent = t.outcome;
    } else if (outcome) outcome.remove();

    votes = t.phase === 'over' ? t.seats.filter((p) => p.voted_for) : [];
    arrows(!first && was !== 'over' && t.phase === 'over' && !still);
    first = false;
    root.dataset.live = '1';
  };

  // The countdown, between fetches.
  const tick = () => {
    const c = q('.clock');
    const left = Math.max(0, Math.ceil((deadline - Date.now()) / 1000));
    c.hidden = !deadline || phase === 'over';
    c.textContent = Math.floor(left / 60) + ':' + String(left % 60).padStart(2, '0');
  };
  setInterval(tick, 250);

  // Vote arrows, from each voter's seat to the seat it voted for, on the
  // round table (not the phone's grid). Two players who voted for each
  // other get two arrows, curving apart.
  let votes = [];
  const svg = document.createElementNS(svgNS, 'svg');
  svg.setAttribute('class', 'arrows');
  svg.setAttribute('aria-hidden', 'true');
  arena.append(svg);
  const edge = (r, from) => { // where the line from `from` to r's center enters r
    const cx = (r.left + r.right) / 2, cy = (r.top + r.bottom) / 2;
    const dx = from.x - cx, dy = from.y - cy;
    const k = Math.min(Math.abs((r.right - r.left) / 2 / (dx || 1e-6)), Math.abs((r.bottom - r.top) / 2 / (dy || 1e-6)), 1);
    return { x: cx + dx * k, y: cy + dy * k };
  };
  function arrows(animate) {
    svg.replaceChildren();
    root.classList.toggle('arrowed', votes.length > 0 && getComputedStyle(svg).display !== 'none');
    if (!root.classList.contains('arrowed')) return;
    const a = arena.getBoundingClientRect();
    svg.setAttribute('viewBox', '0 0 ' + a.width + ' ' + a.height);
    const defs = document.createElementNS(svgNS, 'defs'), marker = document.createElementNS(svgNS, 'marker'), head = document.createElementNS(svgNS, 'path');
    marker.setAttribute('id', 'nf-head');
    for (const [k, v] of [['viewBox', '0 0 10 10'], ['refX', '8'], ['refY', '5'], ['markerWidth', '5'], ['markerHeight', '5'], ['orient', 'auto']]) marker.setAttribute(k, v);
    head.setAttribute('d', 'M0,0 L10,5 L0,10 z');
    marker.append(head);
    defs.append(marker);
    svg.append(defs);
    const box = (h) => {
      const st = seats.get(h);
      if (!st) return null;
      const r = q('.plate', st.li).getBoundingClientRect();
      return { left: r.left - a.left, right: r.right - a.left, top: r.top - a.top, bottom: r.bottom - a.top };
    };
    votes.forEach((p, i) => {
      const f = box(p.handle), to = box(p.voted_for);
      if (!f || !to) return;
      const fc = { x: (f.left + f.right) / 2, y: (f.top + f.bottom) / 2 }, tc = { x: (to.left + to.right) / 2, y: (to.top + to.bottom) / 2 };
      const s = edge(f, tc), e = edge(to, fc);
      const dx = e.x - s.x, dy = e.y - s.y, len = Math.hypot(dx, dy) || 1;
      const bend = Math.min(60, len * 0.18);
      const c = { x: (s.x + e.x) / 2 + (dy / len) * bend, y: (s.y + e.y) / 2 - (dx / len) * bend };
      const path = document.createElementNS(svgNS, 'path');
      path.setAttribute('d', 'M' + s.x + ',' + s.y + ' Q' + c.x + ',' + c.y + ' ' + e.x + ',' + e.y);
      path.setAttribute('marker-end', 'url(#nf-head)');
      svg.append(path);
      if (animate) {
        path.style.setProperty('--len', String(path.getTotalLength()));
        path.style.animationDelay = 1.2 + i * 0.15 + 's';
        path.classList.add('drawn');
      }
    });
  }
  if (window.ResizeObserver) new ResizeObserver(() => arrows(false)).observe(arena);

  document.addEventListener('gamestate', (e) => render(e.detail));
})();
