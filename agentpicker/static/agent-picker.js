// The agent picker (apps/agentpicker): shows one agent's steps at a time and
// remembers the choice. Without it, every agent's steps show.
(function () {
  "use strict";
  var KEY = "wakeshift-agent";
  function load() {
    try { return window.localStorage.getItem(KEY); } catch (e) { return null; }
  }
  function save(id) {
    try { window.localStorage.setItem(KEY, id); } catch (e) { /* private mode */ }
  }
  function setup(root) {
    var pills = Array.prototype.slice.call(root.querySelectorAll(".ap-pill[data-agent]"));
    var panels = Array.prototype.slice.call(root.querySelectorAll(".ap-panel[data-agent]"));
    var more = root.querySelector(".ap-pill.ap-more");
    var moreList = root.querySelector(".ap-more-list");
    if (!pills.length || !panels.length) return;
    function openMore(open) {
      if (!more || !moreList) return;
      more.setAttribute("aria-expanded", open ? "true" : "false");
      moreList.classList.toggle("ap-open", open);
    }
    function select(id, remember) {
      var found = false;
      pills.forEach(function (p) {
        var on = p.getAttribute("data-agent") === id;
        p.setAttribute("aria-pressed", on ? "true" : "false");
        if (on) {
          found = true;
          if (moreList && moreList.contains(p)) openMore(true);
        }
      });
      if (!found) return false;
      panels.forEach(function (p) { p.classList.toggle("ap-on", p.getAttribute("data-agent") === id); });
      if (remember) save(id);
      return true;
    }
    root.classList.add("ap-js");
    pills.forEach(function (p) {
      p.addEventListener("click", function () { select(p.getAttribute("data-agent"), true); });
    });
    if (more) {
      more.addEventListener("click", function () { openMore(more.getAttribute("aria-expanded") !== "true"); });
    }
    if (!select(load() || "", false)) select(root.getAttribute("data-default") || pills[0].getAttribute("data-agent"), false);
    // Copy buttons on commands and config.
    if (navigator.clipboard) {
      root.querySelectorAll(".ap-code").forEach(function (pre) {
        var b = document.createElement("button");
        b.type = "button";
        b.className = "ap-copy";
        b.textContent = "Copy";
        b.addEventListener("click", function () {
          navigator.clipboard.writeText(pre.textContent || "").then(function () {
            b.textContent = "Copied";
            setTimeout(function () { b.textContent = "Copy"; }, 1500);
          }, function () { /* clipboard refused: leave the text selectable */ });
        });
        pre.classList.add("ap-has-copy");
        pre.parentNode.insertBefore(b, pre.nextSibling);
      });
    }
  }
  function init() {
    Array.prototype.forEach.call(document.querySelectorAll("[data-ap]"), setup);
  }
  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", init);
  else init();
})();
