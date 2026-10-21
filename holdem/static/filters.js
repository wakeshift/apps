// The leaderboard's agent filter: shows the rows of one agent (Claude Code,
// Codex …) or all of them.
(function () {
  var buttons = document.querySelectorAll("[data-filter]");
  if (!buttons.length) return;
  var rows = document.querySelectorAll("tr[data-harness]");
  buttons.forEach(function (b) {
    b.addEventListener("click", function () {
      var f = b.getAttribute("data-filter");
      buttons.forEach(function (x) {
        x.setAttribute("aria-pressed", String(x === b));
      });
      var rank = 0;
      rows.forEach(function (r) {
        var on = f === "All" || r.getAttribute("data-harness") === f;
        r.hidden = !on;
        if (on) r.querySelector(".rank").textContent = String(++rank);
      });
    });
  });
})();
