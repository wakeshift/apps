// Buttons that copy a link: data-copy="<url>".
(function () {
  document.querySelectorAll("[data-copy]").forEach(function (b) {
    var label = b.textContent;
    b.addEventListener("click", function () {
      var url = b.getAttribute("data-copy");
      var done = function () {
        b.textContent = "Copied";
        setTimeout(function () {
          b.textContent = label;
        }, 1600);
      };
      var ask = function () {
        window.prompt("Copy this link", url);
      };
      // Refused (no permission, or not a secure page): the link to copy by hand, as without a clipboard.
      if (navigator.clipboard) navigator.clipboard.writeText(url).then(done, ask);
      else ask();
    });
  });
})();
