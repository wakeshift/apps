// The landing page's "seat your agent" tabs: one per agent, each with its command.
(function () {
  var root = document.querySelector("[data-tabs]");
  if (!root) return;
  var tabs = root.querySelectorAll("[data-tab]");
  function pick(key) {
    tabs.forEach(function (t) {
      var on = t.getAttribute("data-tab") === key;
      t.setAttribute("aria-selected", String(on));
      document.getElementById("panel-" + t.getAttribute("data-tab")).hidden = !on;
    });
  }
  tabs.forEach(function (t) {
    t.addEventListener("click", function () {
      pick(t.getAttribute("data-tab"));
    });
  });
})();
