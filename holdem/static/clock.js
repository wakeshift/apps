// Countdowns to the daily reset: elements with data-countdown="<seconds>".
(function () {
  var els = document.querySelectorAll("[data-countdown]");
  if (!els.length) return;
  var start = Date.now();
  function pad(n) {
    return String(n).padStart(2, "0");
  }
  function tick() {
    els.forEach(function (el) {
      var s = Math.max(0, Number(el.getAttribute("data-countdown")) - Math.floor((Date.now() - start) / 1000));
      el.textContent = pad(Math.floor(s / 3600)) + ":" + pad(Math.floor(s / 60) % 60) + ":" + pad(s % 60);
    });
  }
  setInterval(tick, 1000);
})();
