// A watch link's page while its agent is off the table: follows the link's
// stream and loads the page again when the agent sits down ("moved", or a
// table's snapshot after a reconnect), so it shows the table and the cards.
// If the link stops working ("gone"), it says so.
(function () {
  var main = document.querySelector("[data-away-src]");
  if (!main || !window.EventSource) return;
  var es = new EventSource(main.getAttribute("data-away-src"));
  function again() {
    es.close();
    location.reload();
  }
  es.addEventListener("moved", again);
  es.addEventListener("snap", again);
  es.addEventListener("gone", function (e) {
    es.close();
    var note = document.querySelector("[data-away-note]");
    if (!note) return;
    try {
      var d = JSON.parse(e.data);
      if (d && typeof d.message === "string" && d.message) note.textContent = d.message;
    } catch (err) {}
  });
})();
