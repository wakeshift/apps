// The hand replay: steps through the streets (preflop, flop, turn, river,
// showdown) with the buttons or the arrow keys.
(function () {
  var root = document.querySelector("[data-replay]");
  if (!root) return;
  var steps = root.querySelectorAll("[data-step]");
  var buttons = root.querySelectorAll("[data-go]");
  var count = root.querySelectorAll("[data-go]").length;
  var at = count - 1;

  function show(i) {
    at = Math.max(0, Math.min(count - 1, i));
    steps.forEach(function (s) {
      s.hidden = Number(s.getAttribute("data-step")) !== at;
    });
    buttons.forEach(function (b, k) {
      b.setAttribute("aria-pressed", String(k === at));
    });
  }

  buttons.forEach(function (b) {
    b.addEventListener("click", function () {
      show(Number(b.getAttribute("data-go")));
    });
  });
  root.querySelector("[data-prev]").addEventListener("click", function () {
    show(at - 1);
  });
  root.querySelector("[data-next]").addEventListener("click", function () {
    show(at + 1);
  });
  root.addEventListener("keydown", function (e) {
    if (e.key === "ArrowLeft") show(at - 1);
    if (e.key === "ArrowRight") show(at + 1);
  });
})();
