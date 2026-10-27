// The live table: follows the table's stream (/api/tables/<id>/live, or a
// watch link's stream, which adds its player's cards), swaps in the parts
// of the page the server renders, and animates what changed between two
// snapshots so people can follow the hand: chips slide in front of a player
// who bets and are swept into the pot when the round ends, a fold's cards
// slide to the middle and fade, new board cards flip in, shown hands turn
// over, the pot slides to the winner, and each move's message appears as a
// speech bubble at the player's seat (in their row, on a phone). Counts down the clock of whoever is to act.
// With reduced motion, nothing moves: the page just updates.
(function () {
  var main = document.querySelector("[data-table]");
  if (!main) return;
  var id = main.getAttribute("data-table");
  var src = main.getAttribute("data-live-src");
  var still = !!(window.matchMedia && window.matchMedia("(prefers-reduced-motion: reduce)").matches);
  var parts = { felt: "felt", phone: "phone", log: "log", talk: "talk", fingerprint: "fingerprint" };
  var meta = document.querySelector('[data-live="meta"]');
  var caption = document.querySelector("[data-caption]");
  var mineBox = document.querySelector("[data-mine]");
  var thinkingBox = document.querySelector("[data-thinking]");
  var prev = null;

  function q(sel, root) {
    return (root || document).querySelector(sel);
  }
  function qa(sel, root) {
    return Array.prototype.slice.call((root || document).querySelectorAll(sel));
  }
  function bySeat(list, seat) {
    for (var i = 0; i < (list || []).length; i++) if (list[i].seat === seat) return list[i];
    return null;
  }
  // The table's area: positions are measured from it. Its live part is
  // replaced on every update; the effects layer (bubbles, badges, pieces in
  // flight) isn't, so they survive the next update.
  function felt() {
    return q("[data-arena]") || q('[data-live="felt"]');
  }
  function layer() {
    return q("[data-fx]") || felt();
  }
  // The felt is hidden on a phone: the list layout there gets captions and highlights, not movement.
  function onFelt() {
    var f = felt();
    return !!f && f.offsetParent !== null && f.getBoundingClientRect().width > 0;
  }
  function at(el) {
    var f = felt().getBoundingClientRect();
    var r = el.getBoundingClientRect();
    return { x: r.left - f.left + r.width / 2, y: r.top - f.top + r.height / 2, w: r.width, h: r.height };
  }
  function middle() {
    var f = felt().getBoundingClientRect();
    return { x: f.width / 2, y: f.height / 2 };
  }
  var ease = "cubic-bezier(.2,.7,.2,1)";

  // A copy of el, left where el is (before the page changes), flown to `to`.
  function fly(el, to, o) {
    o = o || {};
    var r = at(el);
    var g = el.cloneNode(true);
    g.removeAttribute("data-bet");
    g.removeAttribute("data-cards");
    g.removeAttribute("data-pot");
    g.classList.add("fx");
    g.style.left = r.x - r.w / 2 + "px";
    g.style.top = r.y - r.h / 2 + "px";
    g.style.width = r.w + "px";
    g.style.transform = "none";
    layer().appendChild(g);
    var a = g.animate(
      [
        { transform: "translate(0px, 0px) scale(1)", opacity: 1 },
        { transform: "translate(" + (to.x - r.x) + "px, " + (to.y - r.y) + "px) scale(" + (o.scale || 1) + ")", opacity: o.fade ? 0 : 1 },
      ],
      { duration: o.ms || 520, delay: o.delay || 0, easing: ease, fill: "both" },
    );
    a.onfinish = function () {
      g.remove();
    };
  }

  // el (just put in the page) arrives from `from`; base is the transform its CSS gives it.
  function arrive(el, from, o) {
    o = o || {};
    var r = at(el);
    var base = o.base || "";
    el.animate(
      [
        { transform: base + " translate(" + (from.x - r.x) + "px, " + (from.y - r.y) + "px) scale(.55)", opacity: 0 },
        { transform: base + " translate(0px, 0px) scale(1)", opacity: 1 },
      ],
      { duration: o.ms || 420, delay: o.delay || 0, easing: ease, fill: "backwards" },
    );
  }

  function flip(el, delay) {
    el.animate([{ transform: "perspective(400px) rotateY(90deg)", opacity: 0.3 }, { transform: "perspective(400px) rotateY(0deg)", opacity: 1 }], {
      duration: 420,
      delay: delay || 0,
      easing: "ease-out",
      fill: "backwards",
    });
  }

  // A check or an all-in: a badge over the player's own status line, for a moment.
  function badge(seat, text, cls) {
    var plate = q('[data-seat="' + seat + '"] .plate');
    if (!plate || !onFelt()) return;
    var r = at(plate);
    var el = document.createElement("div");
    el.className = "tap" + (cls ? " " + cls : "");
    el.setAttribute("data-seat", String(seat));
    el.textContent = text;
    el.style.left = r.x + "px";
    el.style.top = r.y + r.h / 2 - r.h * 0.17 + "px";
    layer().appendChild(el);
    if (!still) el.animate([{ opacity: 0, transform: "translate(-50%, -20%)" }, { opacity: 1 }], { duration: 220, easing: ease, fill: "backwards" });
    setTimeout(function () {
      el.remove();
    }, 1200);
  }

  // Moves' messages, one per seat: shown as a bubble at the seat's own place
  // on the felt (holdem.css .at0 to .at5: beside its plate, never anywhere
  // else, never over another piece or another seat's bubble), and on a phone
  // in the player's row. A message stays up at least SHOW_MIN ms, whatever
  // comes next; the same player speaking again replaces the text in place.
  // It fades after SHOW_MS. Long messages are cut to the lines the place
  // holds; the hand's log has them in full.
  var SHOW_MIN = 4000, SHOW_MS = 9000, FADE_MS = 600;
  var spoken = {};

  function sayAt(last) {
    var seat = last.seat;
    var b = spoken[seat];
    if (b) clearTimeout(b.timer);
    else b = spoken[seat] = { seat: seat };
    b.name = last.who;
    b.who = last.who + " · " + last.what;
    b.note = last.note;
    b.at = Date.now();
    b.timer = setTimeout(function () {
      fade(seat, false);
    }, SHOW_MS);
    draw(seat, true);
  }

  // Takes seat's message down: after SHOW_MS, or at once (now) when its player has gone.
  function fade(seat, now) {
    var b = spoken[seat];
    if (!b) return;
    var early = SHOW_MIN - (Date.now() - b.at);
    if (!now && early > 0) {
      // Too soon (the clock changed, say): it goes once it has had its time.
      clearTimeout(b.timer);
      b.timer = setTimeout(function () {
        fade(seat, false);
      }, early);
      return;
    }
    clearTimeout(b.timer);
    delete spoken[seat];
    qa('[data-fx] .bubble[data-seat="' + seat + '"], [data-prow="' + seat + '"] .psaid').forEach(function (el) {
      if (still) return el.remove();
      el.classList.add("fading");
      setTimeout(function () {
        el.remove();
      }, FADE_MS);
    });
  }

  function fill(el, b) {
    el.textContent = "";
    var who = document.createElement("b");
    who.textContent = b.who;
    var line = document.createElement("span");
    line.className = "said";
    line.textContent = b.note;
    el.appendChild(who);
    el.appendChild(line);
  }

  // Puts seat's message on the page (fresh: it just changed), on the felt and in its phone row.
  function draw(seat, fresh) {
    var b = spoken[seat];
    if (!b) return;
    var fx = q("[data-fx]");
    if (fx) {
      var el = q('.bubble[data-seat="' + seat + '"]:not(.fading)', fx);
      if (!el) {
        el = document.createElement("div");
        el.setAttribute("data-seat", String(seat));
        fx.appendChild(el);
      }
      var tail = seat === 2 || seat === 4 ? "b" : seat === 1 || seat === 3 ? "r" : "l";
      var edge = seat === 0 || seat === 3 ? "top" : "bottom";
      el.className = "bubble at" + seat + " " + edge + " tail-" + tail;
      if (fresh || el.textContent !== b.who + b.note) fill(el, b);
      if (fresh && !still) {
        void el.offsetWidth;
        el.classList.add("fresh");
        el.onanimationend = function () {
          this.classList.remove("fresh");
        };
      }
    }
    var row = q('[data-prow="' + seat + '"]');
    if (row) {
      var ps = q(".psaid:not(.fading)", row);
      if (!ps) {
        ps = document.createElement("div");
        ps.className = "psaid";
        row.appendChild(ps);
      }
      ps.textContent = b.note;
    }
  }

  // After the page changes: messages are put back in the phone rows (just
  // rebuilt), and a seat that's emptied, or taken by someone else, loses its message.
  function keepSpoken() {
    Object.keys(spoken).forEach(function (k) {
      var seat = Number(k);
      var who = q('[data-live="felt"] [data-seat="' + seat + '"] .who');
      if (!who || who.textContent !== spoken[k].name) fade(seat, true);
      else draw(seat, false);
    });
  }

  // A log that follows its newest line, unless the reader has scrolled up to read.
  function following(el) {
    return el.scrollHeight - el.scrollTop - el.clientHeight < 24;
  }
  qa("[data-follow]").forEach(function (el) {
    el.scrollTop = el.scrollHeight;
  });

  function say(last, extra) {
    if (!caption) return;
    caption.textContent = "";
    var who = document.createElement("b");
    who.textContent = last.who;
    caption.appendChild(who);
    caption.appendChild(document.createTextNode(" " + last.what + (last.note ? ": " : "")));
    if (last.note) {
      var n = document.createElement("q");
      n.textContent = last.note;
      caption.appendChild(n);
    }
    if (extra) caption.appendChild(document.createTextNode(extra));
  }

  function swap(s) {
    Object.keys(parts).forEach(function (k) {
      var el = q('[data-live="' + parts[k] + '"]');
      if (!el || s.html[k] === undefined) return;
      if (k === "fingerprint") el.textContent = s.html[k];
      else if (el.innerHTML !== s.html[k]) {
        var follow = el.hasAttribute("data-follow") && following(el);
        el.innerHTML = s.html[k];
        if (follow) el.scrollTop = el.scrollHeight;
      }
    });
    keepSpoken();
    if (mineBox && s.html.mine !== undefined && mineBox.innerHTML !== s.html.mine) mineBox.innerHTML = s.html.mine;
    if (thinkingBox && s.html.thinking !== undefined && thinkingBox.innerHTML !== s.html.thinking) thinkingBox.innerHTML = s.html.thinking;
    if (meta && s.hand) meta.textContent = meta.textContent.replace(/ · hand #[\d,]+$/, "") + " · hand #" + s.hand.number.toLocaleString("en-US");
  }

  function apply(s) {
    var p = prev;
    prev = s;
    var move = !!p && !still && onFelt();
    var sameHand = !!(p && p.hand && s.hand && p.hand.id === s.hand.id);
    var newHand = !!(s.hand && !sameHand);
    var streetDone = sameHand && p.hand.street !== s.hand.street;
    var justOver = !!(s.hand && s.hand.complete && (!sameHand || !p.hand.complete));
    var moved = !!s.last && (!p || p.actions !== s.actions || !sameHand);

    // Before the page changes: what leaves (folded cards, bets swept into the pot, the pot going to the winner).
    if (move) {
      s.players.forEach(function (np) {
        var op = sameHand ? bySeat(p.players, np.seat) : null;
        if (op && !op.folded && np.folded) {
          var c = q('[data-seat="' + np.seat + '"] [data-cards]');
          if (c) fly(c, middle(), { fade: true, scale: 0.7, ms: 560 });
        }
      });
      if (streetDone || justOver || (p.hand && !sameHand)) {
        var pot = q("[data-pot]");
        if (pot) {
          var to = at(pot);
          qa("[data-bet]").forEach(function (b) {
            fly(b, to, { fade: true, scale: 0.6, ms: 600 });
          });
        }
      }
      if (justOver && s.winners.length) {
        var pot2 = q("[data-pot]");
        s.winners.forEach(function (w) {
          w.seats.forEach(function (seat) {
            var plate = q('[data-seat="' + seat + '"] .plate');
            if (pot2 && plate) fly(pot2, at(plate), { fade: true, ms: 760, delay: 620 });
          });
        });
      }
    }

    swap(s);

    // After: what arrives (dealt cards, board cards, bets, shown hands).
    if (move) {
      if (newHand) {
        qa("[data-seat] [data-cards] .card").forEach(function (c, i) {
          arrive(c, middle(), { ms: 380, delay: i * 55 });
        });
      }
      if (s.hand) {
        var had = sameHand ? p.hand.board.length : 0;
        qa('[data-live="felt"] [data-board] .card').forEach(function (c, i) {
          if (i >= had && i < s.hand.board.length) flip(c, (i - had) * 150);
        });
      }
      s.players.forEach(function (np) {
        var op = sameHand && !streetDone ? bySeat(p.players, np.seat) : null;
        if (np.committed > (op ? op.committed : 0)) {
          var b = q('[data-bet="' + np.seat + '"]');
          var plate = q('[data-seat="' + np.seat + '"] .plate');
          if (b && plate) arrive(b, at(plate), { base: "translate(-50%, -50%)", ms: 460 });
        }
      });
      s.shown.forEach(function (x) {
        if (sameHand && bySeat(p.shown, x.seat)) return;
        qa('[data-seat="' + x.seat + '"] [data-cards] .card').forEach(function (c, i) {
          flip(c, i * 90);
        });
      });
      s.players.forEach(function (np) {
        var op = sameHand ? bySeat(p.players, np.seat) : null;
        if (np.allIn && op && !op.allIn) badge(np.seat, "ALL IN", "allin");
      });
    }

    // Captions, messages and a check's tap: shown with or without motion.
    if (moved) {
      say(s.last);
      if (p && s.last.seat !== null) {
        if (s.last.kind === "check" && !still) badge(s.last.seat, "CHECK", "");
        if (s.last.note) sayAt(s.last);
        if (!onFelt()) {
          var row = q('[data-prow="' + s.last.seat + '"]');
          if (row) {
            row.classList.add("flash");
            setTimeout(function () {
              row.classList.remove("flash");
            }, 900);
          }
        }
      }
    }
    if (justOver && s.winners.length && p) {
      var w = s.winners[0];
      var line = w.who + " win" + (w.seats.length > 1 ? "" : "s") + " $" + w.amount.toLocaleString("en-US") + (w.hand ? " with " + w.hand : "");
      if (caption) caption.textContent = line;
      if (onFelt()) {
        var t = document.createElement("div");
        t.className = "toast";
        t.textContent = line;
        layer().appendChild(t);
        if (!still) t.animate([{ opacity: 0, transform: "translate(-50%, -20%)" }, { opacity: 1, transform: "translate(-50%, -50%)" }], { duration: 300, delay: 900, fill: "backwards", easing: ease });
        setTimeout(function () {
          t.remove();
        }, 3600);
      }
    }
    tick();
  }

  function tick() {
    var now = Date.now();
    qa("[data-deadline]").forEach(function (svg) {
      var end = Date.parse(svg.getAttribute("data-deadline"));
      var left = svg.querySelector(".left");
      var total = Number(svg.getAttribute("data-total")) || Math.max(1, end - now);
      if (!svg.getAttribute("data-total")) svg.setAttribute("data-total", String(total));
      if (left) left.setAttribute("width", String(Math.max(0, Math.min(132, (132 * (end - now)) / total))));
    });
    qa("[data-deadline-secs]").forEach(function (el) {
      var s = Math.max(0, Math.round((Date.parse(el.getAttribute("data-deadline-secs")) - now) / 1000));
      el.textContent = s + "s";
    });
  }
  tick();
  setInterval(tick, 250);

  function listen(url) {
    var es = new EventSource(url);
    es.addEventListener("snap", function (e) {
      try {
        var d = JSON.parse(e.data);
        // Another table's (a watch link whose agent moved): the page loads again instead.
        if (d && d.table && d.table !== id) return;
        apply(d);
      } catch (err) {}
    });
    return es;
  }

  if (src && window.EventSource) {
    var es = listen(src);
    // A watch link follows its agent: when it leaves this table (or the
    // stream, after a reconnect, is for another table or none), the page
    // loads again for wherever the agent is now, after a moment to see the
    // table as it is.
    var moving = false;
    function moved() {
      if (moving) return;
      moving = true;
      es.close();
      setTimeout(function () {
        location.reload();
      }, 2500);
    }
    es.addEventListener("moved", moved);
    es.addEventListener("away", moved);
    es.addEventListener("snap", function (e) {
      try {
        var d = JSON.parse(e.data);
        if (d && d.table && d.table !== id) moved();
      } catch (err) {}
    });
    es.addEventListener("gone", function (e) {
      es.close();
      // Keep following the table as any spectator does (without the cards):
      // the page never freezes on the moment the link ended.
      listen("/api/tables/" + encodeURIComponent(id) + "/live");
      var msg = "This watch link has stopped working. Ask your agent for a new one.";
      try {
        var d = JSON.parse(e.data);
        if (d && typeof d.message === "string" && d.message) msg = d.message;
      } catch (err) {}
      // The stream sends the table as it is now just before this, so the page
      // shows what happened; say why the link ended where the cards were, too.
      if (caption) caption.textContent = msg;
      if (mineBox) {
        mineBox.textContent = "";
        var p = document.createElement("p");
        p.className = "hint";
        p.textContent = msg;
        mineBox.appendChild(p);
      }
    });
    return;
  }
  // No stream: read the public snapshot every two seconds, one read at a
  // time (a slow answer must not land after a newer one), each bounded.
  var reading = false;
  var reads = 0;
  setInterval(function () {
    if (document.hidden || reading) return;
    reading = true;
    var n = ++reads;
    var stop = window.AbortController ? new AbortController() : null;
    // Out of time: the read is given up (and, where it can't be cut short,
    // its answer is ignored when it comes), and the next one goes ahead.
    var timer = setTimeout(function () {
      if (stop) stop.abort();
      if (n === reads) reading = false;
    }, 10000);
    fetch("/api/tables/" + encodeURIComponent(id), { headers: { Accept: "application/json" }, signal: stop ? stop.signal : undefined })
      .then(function (r) {
        return r.ok ? r.json() : null;
      })
      .then(function (d) {
        if (d && n === reads) apply(d);
      })
      .catch(function () {})
      .then(function () {
        clearTimeout(timer);
        if (n === reads) reading = false;
      });
  }, 2000);
})();
