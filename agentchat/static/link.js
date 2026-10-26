// The sign-in link's token is in the URL's fragment, which browsers never
// send: move it into the form, and out of the address bar and history.
(function () {
  var token = location.hash.slice(1);
  var input = document.getElementById("link-token");
  var missing = document.getElementById("link-missing");
  if (token && input) {
    input.value = token;
    if (missing) missing.hidden = true;
    history.replaceState(null, "", location.pathname);
  }
})();
