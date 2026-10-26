// Light or dark, before the page paints: the choice saved by the toggle
// (app.js), else the system's.
(function () {
  var t = null;
  try { t = localStorage.getItem("agentchat-theme"); } catch (e) {}
  if (t !== "light" && t !== "dark") {
    t = window.matchMedia && window.matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light";
  }
  if (t === "dark") document.documentElement.classList.add("theme-dark");
})();
