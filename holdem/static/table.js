// The live table: re-renders the felt, the hand's log and the table talk
// every two seconds from /api/tables/<id> (the server renders the HTML), and
// counts down the clock of whoever is to act.
(function () {
  var main = document.querySelector("[data-table]");
  if (!main) return;
  var id = main.getAttribute("data-table");
  var parts = { felt: "felt", phone: "phone", log: "log", talk: "talk", fingerprint: "fingerprint" };
  var meta = document.querySelector('[data-live="meta"]');

  function tick() {
    var now = Date.now();
    document.querySelectorAll("[data-deadline]").forEach(function (svg) {
      var end = Date.parse(svg.getAttribute("data-deadline"));
      var left = svg.querySelector(".left");
      var total = Number(svg.getAttribute("data-total")) || Math.max(1, end - now);
      if (!svg.getAttribute("data-total")) svg.setAttribute("data-total", String(total));
      if (left) left.setAttribute("width", String(Math.max(0, Math.min(132, (132 * (end - now)) / total))));
    });
    document.querySelectorAll("[data-deadline-secs]").forEach(function (el) {
      var s = Math.max(0, Math.round((Date.parse(el.getAttribute("data-deadline-secs")) - now) / 1000));
      el.textContent = s + "s";
    });
  }

  function load() {
    fetch("/api/tables/" + id, { headers: { Accept: "application/json" } })
      .then(function (r) {
        return r.ok ? r.json() : null;
      })
      .then(function (d) {
        if (!d) return;
        Object.keys(parts).forEach(function (k) {
          var el = document.querySelector('[data-live="' + parts[k] + '"]');
          if (el && d.html[k] !== undefined && el.innerHTML !== d.html[k]) {
            if (k === "fingerprint") el.textContent = d.html[k];
            else el.innerHTML = d.html[k];
          }
        });
        if (meta && d.hand) meta.textContent = meta.textContent.replace(/hand #[\d,]+|$/, "").replace(/ · $/, "") + " · hand #" + d.hand.number.toLocaleString("en-US");
        tick();
      })
      .catch(function () {});
  }

  tick();
  setInterval(tick, 250);
  setInterval(function () {
    if (!document.hidden) load();
  }, 2000);
})();
