// Wakeshift's page analytics (apps/analytics): a page view when the page
// loads, a page leave when it goes, and the clicks and submits that a page
// marks with data-analytics. Events go to this origin's /ingest proxy, which
// trims page addresses and forwards them to PostHog. Nothing is stored in the
// browser: the ID below is new for each page load.
(function () {
  "use strict";
  var ENDPOINT = "/ingest/i/v0/e/";
  function uuid() {
    if (window.crypto && crypto.randomUUID) return crypto.randomUUID();
    var b = new Uint8Array(16);
    crypto.getRandomValues(b);
    b[6] = (b[6] & 15) | 64;
    b[8] = (b[8] & 63) | 128;
    var h = "";
    for (var i = 0; i < 16; i++) {
      h += (b[i] + 256).toString(16).slice(1);
      if (i === 3 || i === 5 || i === 7 || i === 9) h += "-";
    }
    return h;
  }
  var id = uuid();
  function send(event, extra) {
    var props = { $current_url: location.href, $referrer: document.referrer || "$direct" };
    for (var k in extra) if (Object.prototype.hasOwnProperty.call(extra, k)) props[k] = extra[k];
    var body = JSON.stringify({ event: event, distinct_id: id, properties: props });
    try {
      if (navigator.sendBeacon && navigator.sendBeacon(ENDPOINT, new Blob([body], { type: "application/json" }))) return;
    } catch (e) { /* fall back to fetch */ }
    try {
      fetch(ENDPOINT, { method: "POST", body: body, headers: { "Content-Type": "application/json" }, credentials: "omit", keepalive: true }).catch(function () {});
    } catch (e) { /* analytics never breaks the page */ }
  }
  // data-analytics="cta_clicked" data-analytics-cta="start_building" sends
  // cta_clicked with {cta: "start_building"}. The proxy drops any event or
  // value it doesn't know.
  function marked(el) {
    var props = {};
    for (var k in el.dataset) {
      if (k.length > 9 && k.indexOf("analytics") === 0) props[k.charAt(9).toLowerCase() + k.slice(10)] = el.dataset[k];
    }
    send(el.getAttribute("data-analytics"), props);
  }
  send("$pageview");
  var left = false;
  window.addEventListener("pagehide", function () {
    if (!left) {
      left = true;
      send("$pageleave");
    }
  });
  window.addEventListener("pageshow", function (e) {
    if (e.persisted && left) { // back from the back/forward cache
      left = false;
      send("$pageview");
    }
  });
  document.addEventListener("click", function (e) {
    var t = e.target;
    if (!t || !t.closest) return;
    var el = t.closest("[data-analytics]");
    if (el && el.tagName !== "FORM") return marked(el);
    // The agent picker (apps/agentpicker): which agent was picked.
    var pill = t.closest(".ap-pill[data-agent]");
    if (pill) send("agent_picked", { agent: pill.getAttribute("data-agent") });
  }, true);
  document.addEventListener("submit", function (e) {
    var f = e.target;
    if (f && f.getAttribute && f.getAttribute("data-analytics")) marked(f);
  }, true);
})();
