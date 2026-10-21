// Checks a finished hand's shuffle in the browser, the same way the engine
// does (apps/holdem/engine: shuffle.ts and verify.ts):
//
// 1. SHA-256 of the seed's bytes must equal the fingerprint published before
//    the deal.
// 2. The deck is a Fisher–Yates shuffle of 2c 2d 2h 2s 3c … As, from the last
//    position down, each swap's index read from HMAC-SHA256(seed,
//    "<hand id>:<block>") as big-endian 32-bit numbers, with rejection
//    sampling.
// 3. The deal is two cards to each player in the start's dealOrder (one each,
//    twice), then burn, flop, burn, turn, burn, river. The board and every
//    hand shown must be those cards.
(function () {
  var box = document.querySelector("[data-verify]");
  if (!box) return;
  var button = box.querySelector("[data-run]");
  if (!button) return;

  function hexBytes(hex) {
    var out = new Uint8Array(hex.length / 2);
    for (var i = 0; i < out.length; i++) out[i] = parseInt(hex.substr(i * 2, 2), 16);
    return out;
  }
  function toHex(buf) {
    return Array.prototype.map.call(new Uint8Array(buf), function (b) {
      return b.toString(16).padStart(2, "0");
    }).join("");
  }

  async function deckFor(seed, handId) {
    var key = await crypto.subtle.importKey("raw", hexBytes(seed), { name: "HMAC", hash: "SHA-256" }, false, ["sign"]);
    var enc = new TextEncoder();
    var block = 0;
    var buf = null;
    var pos = 32;
    async function next32() {
      if (pos + 4 > 32) {
        buf = new DataView(await crypto.subtle.sign("HMAC", key, enc.encode(handId + ":" + block++)));
        pos = 0;
      }
      var v = buf.getUint32(pos);
      pos += 4;
      return v;
    }
    var deck = [];
    "23456789TJQKA".split("").forEach(function (r) {
      "cdhs".split("").forEach(function (s) {
        deck.push(r + s);
      });
    });
    for (var i = deck.length - 1; i > 0; i--) {
      var n = i + 1;
      var limit = Math.floor(0x100000000 / n) * n;
      var x = await next32();
      while (x >= limit) x = await next32();
      var j = x % n;
      var t = deck[i];
      deck[i] = deck[j];
      deck[j] = t;
    }
    return deck;
  }

  async function check() {
    var log = JSON.parse(box.querySelector("[data-log]").textContent);
    var start = log.find(function (e) { return e.type === "start"; });
    var end = log.find(function (e) { return e.type === "end"; });
    if (!start || !end) return { ok: false, why: "The hand isn't over yet: there's no seed to check." };
    var digest = toHex(await crypto.subtle.digest("SHA-256", hexBytes(end.seed)));
    if (digest !== start.commitment) return { ok: false, why: "The seed doesn't match the fingerprint published before the deal." };
    var deck = await deckFor(end.seed, start.hand);
    var n = start.dealOrder.length;
    var places = { flop: [2 * n + 1, 2 * n + 2, 2 * n + 3], turn: [2 * n + 5], river: [2 * n + 7] };
    var checked = 0;
    for (var k = 0; k < log.length; k++) {
      var e = log[k];
      if (e.type === "deal") {
        var want = places[e.street].map(function (i) { return deck[i]; }).join(",");
        if (want !== e.cards.join(",")) return { ok: false, why: "The " + e.street + " isn't what the deck deals." };
        checked += e.cards.length;
      } else if (e.type === "show") {
        var d = start.dealOrder.indexOf(e.seat);
        var hole = d < 0 ? [] : [deck[d], deck[n + d]];
        if (hole.slice().sort().join(",") !== e.cards.slice().sort().join(",")) return { ok: false, why: "Seat " + e.seat + "'s cards aren't what the deck dealt it." };
        checked += e.cards.length;
      }
    }
    return { ok: true, checked: checked };
  }

  button.addEventListener("click", function () {
    var ok = box.querySelector("[data-ok]");
    var bad = box.querySelector("[data-bad]");
    ok.hidden = true;
    bad.hidden = true;
    if (!window.crypto || !crypto.subtle) {
      box.querySelector("[data-why]").textContent = "This browser can't check it here (it needs Web Crypto, on an https page).";
      bad.hidden = false;
      return;
    }
    button.disabled = true;
    check()
      .then(function (r) {
        if (r.ok) ok.hidden = false;
        else {
          box.querySelector("[data-why]").textContent = r.why;
          bad.hidden = false;
        }
      })
      .catch(function (err) {
        box.querySelector("[data-why]").textContent = "The check couldn't run: " + err;
        bad.hidden = false;
      })
      .finally(function () {
        button.disabled = false;
      });
  });
})();
