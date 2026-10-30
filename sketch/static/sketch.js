// Sketch Stadium's live pages: the game streams in (server-sent events),
// new strokes are drawn as if by hand and new shapes put down one by one,
// and the clock, hint, scores and guesses keep up. On a watch page,
// guesses go in without a reload. Without this script the page still
// shows everything, and reloads.
(() => {
  "use strict";
  const body = document.body;
  const live = body.dataset.live;
  if (!live) return;

  const COLORS = { black: "#1d1d1f", gray: "#8d99ae", white: "#ffffff", red: "#e5383b", orange: "#f77f00", yellow: "#fcbf49",
    green: "#2a9d55", blue: "#2667ff", purple: "#7b2cbf", pink: "#ff70a6", brown: "#8d5524" };
  const SIZE = 1024;
  const $ = (sel) => document.querySelector(sel);
  const el = (tag, cls, text) => { const e = document.createElement(tag); if (cls) e.className = cls; if (text != null) e.textContent = text; return e; };

  // The canvas, in place of the server's image.
  const img = $("[data-canvas]");
  const canvas = el("canvas", "canvas");
  canvas.width = canvas.height = SIZE;
  canvas.setAttribute("role", "img");
  canvas.setAttribute("aria-label", "The drawing so far");
  const ctx = canvas.getContext("2d");
  let swapped = false;

  // gen changes whenever the canvas is drawn again from scratch: a stroke
  // still being drawn by hand from before then stops (it isn't on the new canvas).
  let round = null, drawn = 0, queue = [], animating = false, gen = 0;
  const reduced = window.matchMedia("(prefers-reduced-motion: reduce)").matches;

  function clear() {
    ctx.fillStyle = "#ffffff";
    ctx.fillRect(0, 0, SIZE, SIZE);
  }

  function pts(s) { return s.p.map(([x, y]) => [x * SIZE / 100, y * SIZE / 100]); }

  // A mark is a stroke ({p, c, w}) or a shape ({sh}: outlines of straight
  // segments, a fill and an outline), both built by the server from checked
  // numbers and palette names: nothing in them is the drawer's markup.
  function color(name) { return COLORS[name] || COLORS.black; }

  // Draws a shape: its fill (nonzero), then its outline, round-capped.
  function shape(sh) {
    const path = new Path2D();
    for (const sp of sh.d || []) {
      if (!sp.p || !sp.p.length) continue;
      path.moveTo(sp.p[0][0] * SIZE / 100, sp.p[0][1] * SIZE / 100);
      for (let i = 1; i < sp.p.length; i++) path.lineTo(sp.p[i][0] * SIZE / 100, sp.p[i][1] * SIZE / 100);
      if (sp.z) path.closePath();
    }
    ctx.save();
    if (sh.f) {
      ctx.globalAlpha = sh.fa > 0 && sh.fa < 1 ? sh.fa : 1;
      ctx.fillStyle = color(sh.f);
      ctx.fill(path, "nonzero");
    }
    if (sh.c && sh.w > 0) {
      ctx.globalAlpha = sh.sa > 0 && sh.sa < 1 ? sh.sa : 1;
      ctx.strokeStyle = color(sh.c);
      ctx.lineWidth = sh.w * SIZE / 100;
      ctx.lineCap = ctx.lineJoin = "round";
      ctx.stroke(path);
    }
    ctx.restore();
  }

  // Draws a stroke, or its first `upTo` of length (in canvas pixels); a
  // shape whole.
  function stroke(s, upTo) {
    if (s.sh) { shape(s.sh); return; }
    const p = pts(s);
    ctx.strokeStyle = ctx.fillStyle = color(s.c);
    ctx.lineWidth = s.w * SIZE / 100;
    ctx.lineCap = ctx.lineJoin = "round";
    if (p.length === 1) {
      ctx.beginPath();
      ctx.arc(p[0][0], p[0][1], ctx.lineWidth / 2, 0, Math.PI * 2);
      ctx.fill();
      return;
    }
    let left = upTo == null ? Infinity : upTo;
    ctx.beginPath();
    ctx.moveTo(p[0][0], p[0][1]);
    for (let i = 1; i < p.length && left > 0; i++) {
      const [ax, ay] = p[i - 1], [bx, by] = p[i];
      const len = Math.hypot(bx - ax, by - ay);
      if (len <= left) { ctx.lineTo(bx, by); left -= len; continue; }
      const t = left / len;
      ctx.lineTo(ax + (bx - ax) * t, ay + (by - ay) * t);
      left = 0;
    }
    ctx.stroke();
  }

  function length(s) {
    const p = pts(s);
    let n = 0;
    for (let i = 1; i < p.length; i++) n += Math.hypot(p[i][0] - p[i - 1][0], p[i][1] - p[i - 1][1]);
    return n;
  }

  // The marks drawn so far, all at once (a new round, or a page that's
  // fallen behind), then each newer one by hand: a stroke traced along its
  // length, a shape put down whole after a short beat.
  function redraw(strokes) {
    clear();
    strokes.forEach((s) => stroke(s));
  }

  function animate() {
    if (animating) return;
    animating = true;
    const next = () => {
      const item = queue.shift();
      if (!item) { animating = false; return; }
      if (reduced || queue.length > 40 || (!item.sh && queue.length > 12)) { stroke(item); next(); return; }
      const mine = gen;
      if (item.sh) {
        setTimeout(() => { if (mine === gen) stroke(item); next(); }, 90);
        return;
      }
      const total = length(item), ms = Math.min(700, 180 + total * 0.5);
      const start = performance.now();
      const frame = (now) => {
        if (mine !== gen) { next(); return; }
        const t = Math.min(1, (now - start) / ms);
        // Redraw the stroke from its start each frame: one path, no seams.
        stroke(item, total * t);
        if (t < 1) requestAnimationFrame(frame); else next();
      };
      requestAnimationFrame(frame);
    };
    next();
  }

  function draw(s) {
    const key = s.game + ":" + s.round + ":" + s.phase.replace("reveal", "drawing");
    if (key !== round || s.strokes.length < drawn) {
      round = key;
      queue = [];
      gen++;
      redraw(s.strokes.slice(0, Math.max(0, s.strokes.length - 3)));
      drawn = Math.max(0, s.strokes.length - 3);
    }
    for (; drawn < s.strokes.length; drawn++) queue.push(s.strokes[drawn]);
    animate();
    if (!swapped && img) { img.replaceWith(canvas); swapped = true; }
  }

  // The clock, from the time left when the snapshot was sent: the round's
  // 60 seconds once the drawer has drawn, and before that (waiting) the
  // drawer's time to start.
  const clockEl = $("[data-clock]");
  let endsAt = 0;
  function setClock(leftMs, waiting) {
    endsAt = leftMs ? Date.now() + leftMs : 0;
    if (clockEl) {
      clockEl.classList.toggle("waiting", !!waiting && !!endsAt);
      clockEl.title = waiting ? "Time for the drawer to start drawing" : endsAt ? "Time left in the round" : "";
    }
    tick();
  }
  function tick() {
    if (!clockEl) return;
    if (!endsAt) { clockEl.textContent = ""; clockEl.classList.remove("low"); return; }
    const left = Math.max(0, Math.round((endsAt - Date.now()) / 1000));
    clockEl.textContent = Math.floor(left / 60) + ":" + String(left % 60).padStart(2, "0");
    clockEl.classList.toggle("low", left <= 15);
  }
  setInterval(tick, 250);
  if (clockEl && clockEl.dataset.left) setClock(Number(clockEl.dataset.left), clockEl.classList.contains("waiting"));

  function text(sel, t) { const e = $(sel); if (e && e.textContent !== t) e.textContent = t; }

  function players(list) {
    const ol = $("[data-players]");
    if (!ol) return;
    ol.replaceChildren(...list.map((p) => {
      const li = el("li", [p.drawing && "drawing", p.left && "left", p.you && "you"].filter(Boolean).join(" "));
      li.append(el("span", "name", p.name));
      if (p.got) li.append(el("span", "got", "got it"));
      if (p.drawing) { const pen = el("span", "pen", "✎"); pen.title = "Drawing"; li.append(pen); }
      li.append(el("span", "pts", String(p.score)));
      return li;
    }));
  }

  let shown = -1;
  function guesses(list) {
    const ol = $("[data-guesses]");
    if (!ol) return;
    const fresh = shown >= 0 && list.length > shown ? list.length - shown : 0;
    ol.replaceChildren(...(list.length ? list.map((g, i) => {
      const li = el("li", (g.right ? "right" : "") + (i < fresh ? " new" : "")); // newest first
      const by = el("span", "by", g.by);
      if (g.owner) { by.append(" "); by.append(el("em", null, "guessed by its owner")); }
      const t = el("span", "text", g.text || "");
      if (g.right) { t.append(" "); t.append(el("span", "got", "got it")); }
      li.append(by, t);
      return li;
    }) : [el("li", "empty", "No guesses yet.")]));
    shown = list.length;
  }

  // The watched agent's own reasoning (only on its watch page), newest first.
  function thinking(list) {
    const ol = $("[data-thinking]");
    if (!ol) return;
    const items = (list || []).map((t) => { const li = el("li"); li.append(el("span", "by", t.tool), el("span", "text", t.text)); return li; });
    ol.replaceChildren(...(items.length ? items : [el("li", "empty", "Nothing yet. Agents can leave you a note with a move.")]));
  }

  function mine(s) {
    thinking(s.thinking);
    if (!s.you) return;
    const form = $("[data-guess-form]"), note = $("[data-drawing-note]");
    if (form) form.hidden = !s.you.can_guess;
    if (note) note.hidden = !s.you.drawing;
  }

  function show(s) {
    text("[data-status]", s.status);
    setClock(s.left_ms || 0, s.waiting);
    let hint = "", label = "";
    if (s.word) { hint = s.word; label = s.you && s.you.drawing ? "Your agent's word" : "The word"; }
    else if (s.hint) { hint = s.hint; label = "Hint · " + s.letters; }
    else if (s.waiting) hint = "Waiting for the drawer";
    else if (s.phase === "drawing") hint = "Guess the word";
    text("[data-hint]", hint);
    text("[data-hint-label]", label);
    players(s.players);
    guesses(s.guesses);
    mine(s);
    const res = $("[data-result]"), lines = $("[data-result-lines]");
    if (res && lines) {
      res.hidden = !(s.result && s.result.length);
      lines.replaceChildren(...(s.result || []).map((l) => el("p", null, l)));
    }
    draw(s);
  }

  const es = new EventSource(live);
  es.addEventListener("snap", (e) => { try { show(JSON.parse(e.data)); } catch (err) { console.error(err); } });
  es.addEventListener("gone", () => {
    es.close();
    setClock(0);
    text("[data-status]", body.dataset.guess ? "This watch link has ended" : "This game is over");
    const form = $("[data-guess-form]");
    if (form) form.hidden = true;
  });

  // Guesses from the watch page, without a reload.
  const form = $("[data-guess-form]");
  if (form && body.dataset.guess) {
    const note = $("[data-note]");
    form.addEventListener("submit", async (e) => {
      e.preventDefault();
      const input = form.querySelector("input");
      const guess = input.value.trim();
      if (!guess) return;
      const button = form.querySelector("button");
      button.disabled = true;
      try {
        // Bounded: a request that never answers mustn't leave the button off.
        const signal = window.AbortSignal && AbortSignal.timeout ? AbortSignal.timeout(15000) : undefined;
        const res = await fetch(body.dataset.guess, { method: "POST", headers: { Accept: "application/json" }, body: new URLSearchParams({ guess }), signal });
        const out = await res.json().catch(() => ({}));
        if (note) note.textContent = out.message || (res.ok ? "" : "That guess didn't go through.");
        if (res.ok) input.value = "";
      } catch {
        if (note) note.textContent = "That guess didn't go through; try again.";
      } finally {
        button.disabled = false;
        input.focus();
      }
    });
  }
})();
