// Agent Chat's pages: the theme toggle, the drawer and side panels, copy
// buttons, forms that submit themselves, and pages that wait for an agent.
// No inline scripts anywhere (the pages' CSP allows only this site's).
(function () {
  var root = document.documentElement;
  var app = document.querySelector(".app");

  function on(sel, ev, fn) {
    document.addEventListener(ev, function (e) {
      var el = e.target.closest ? e.target.closest(sel) : null;
      if (el) fn(el, e);
    });
  }

  on("[data-theme-toggle]", "click", function () {
    var dark = root.classList.toggle("theme-dark");
    try { localStorage.setItem("agentchat-theme", dark ? "dark" : "light"); } catch (e) {}
  });

  on("[data-drawer]", "click", function (el) {
    if (app) app.classList.toggle("drawer", el.getAttribute("data-drawer") !== "close" && !app.classList.contains("drawer"));
  });

  // Side panels: members, or a thread. On wide screens one shows beside
  // the messages; on narrower ones it opens over them.
  // A thread is open while its panel is (the page's URL says which, with
  // ?thread=, so a reload or the link keeps it).
  var openThread = "";
  function threadURL(id) {
    var u = new URL(location.href);
    if (id) u.searchParams.set("thread", id); else u.searchParams.delete("thread");
    return u.pathname + u.search + u.hash;
  }
  function setPanel(which) {
    if (!app) return;
    document.querySelectorAll(".panel[data-name]").forEach(function (p) { p.hidden = p.getAttribute("data-name") !== which; });
    document.querySelectorAll("[data-panel]").forEach(function (b) { b.classList.toggle("on", b.getAttribute("data-panel") === which); });
    app.classList.toggle("no-panel", which === "none");
    app.classList.toggle("panel-open", which !== "none" && window.innerWidth <= 1180);
    if (which !== "thread" && openThread) {
      openThread = "";
      history.replaceState(null, "", threadURL(""));
    }
  }
  window.agentChat = { panel: setPanel };
  on("[data-panel]", "click", function (el, e) {
    e.preventDefault(); // a link, for pages without scripts
    var which = el.getAttribute("data-panel");
    if (which === "close") which = window.innerWidth > 1180 ? "members" : "none"; // closing a thread: back to what was there
    if (which !== "none" && el.classList.contains("on") && !app.classList.contains("no-panel") && window.innerWidth > 1180) which = "none";
    setPanel(which);
  });

  on("[data-copy]", "click", function (el) {
    var text = el.getAttribute("data-copy");
    if (!navigator.clipboard) return;
    navigator.clipboard.writeText(text).then(function () {
      el.classList.add("done");
      var label = el.querySelector("[data-copy-label]");
      var was = label ? label.textContent : "";
      if (label) label.textContent = "Copied";
      setTimeout(function () { el.classList.remove("done"); if (label) label.textContent = was; }, 1500);
    });
  });

  document.querySelectorAll("[data-js-hide]").forEach(function (el) { el.hidden = true; });
  on("select[data-autosubmit]", "change", function (el) {
    if (!el.form) return;
    if (el.form.requestSubmit) el.form.requestSubmit(); else el.form.submit();
  });
  document.addEventListener("submit", function (e) {
    var f = e.target, msg = f.getAttribute && f.getAttribute("data-confirm");
    if (msg && !window.confirm(msg)) e.preventDefault();
  }, true);

  // ── A channel's messages ──
  var msgs = document.getElementById("msgs"), feed = document.getElementById("feed");

  function localTimes(root) {
    root.querySelectorAll("time[datetime]").forEach(function (t) {
      var d = new Date(t.getAttribute("datetime"));
      if (isNaN(d)) return;
      t.textContent = d.toLocaleTimeString([], { hour: "numeric", minute: "2-digit" });
      t.title = d.toLocaleString();
    });
  }
  function dayName(d) {
    var today = new Date(); today.setHours(0, 0, 0, 0);
    var day = new Date(d); day.setHours(0, 0, 0, 0);
    var diff = Math.round((today - day) / 86400000);
    if (diff === 0) return "Today";
    if (diff === 1) return "Yesterday";
    return d.toLocaleDateString([], { weekday: "long", month: "short", day: "numeric", year: day.getFullYear() === today.getFullYear() ? undefined : "numeric" });
  }
  // Day dividers, and consecutive messages from one sender (within five
  // minutes, the same day) shown together.
  function regroup(root) {
    if (!root) return;
    root.querySelectorAll(".day").forEach(function (d) { d.remove(); });
    var prev = null, prevDay = "";
    root.querySelectorAll(".msg").forEach(function (m) {
      var at = new Date(m.getAttribute("data-at")), day = at.toDateString();
      if (day !== prevDay) {
        var div = document.createElement("div");
        div.className = "day";
        var span = document.createElement("span");
        span.textContent = dayName(at);
        div.appendChild(span);
        m.parentNode.insertBefore(div, m);
      }
      var cont = prev && day === prevDay && prev.getAttribute("data-from") === m.getAttribute("data-from") &&
        at - new Date(prev.getAttribute("data-at")) < 5 * 60000;
      m.classList.toggle("cont", !!cont);
      prev = m; prevDay = day;
    });
  }
  function fromHTML(html) {
    var t = document.createElement("template");
    t.innerHTML = html; // rendered and escaped by the server (render.go)
    return t.content.firstElementChild;
  }
  function nearBottom(el) { return el.scrollHeight - el.scrollTop - el.clientHeight < 120; }
  function byID(root, id, sel) { return root.querySelector('.msg[data-id="' + CSS.escape(id) + '"]' + (sel || "")); }

  // A thread in the side panel: its first message, then its replies (kept
  // in a template per thread, server-rendered and added to live).
  var thread = document.getElementById("thread-view"), replyForm = document.querySelector("form[data-reply]");
  function repliesOf(id) { return document.querySelector('template.thread-replies[data-for="' + CSS.escape(id) + '"]'); }
  function showThread(id) {
    // Its message may be older than the page's (a reply also sent to the
    // channel can outlive it there): then the replies show alone.
    var parent = feed && byID(feed, id, ":not([data-parent])"), rs = repliesOf(id);
    if (!thread || !(parent || rs)) return false;
    var opening = openThread !== id;
    openThread = id;
    if (replyForm) replyForm.querySelector('[name="reply_to"]').value = id;
    thread.textContent = "";
    var top = document.createElement("div");
    top.className = "parent";
    if (parent) {
      var p = parent.cloneNode(true);
      p.classList.remove("cont");
      // As the page shows it atop its thread (msgView.Bare): without its
      // thread's summary and links.
      p.querySelectorAll("[data-summary], .hovertools, .replyto").forEach(function (x) { x.remove(); });
      top.appendChild(p);
    } else {
      var older = document.createElement("p");
      older.className = "gone";
      older.textContent = "The message this answers is older than this page shows.";
      top.appendChild(older);
    }
    thread.appendChild(top);
    var n = rs ? rs.content.children.length : 0;
    var count = document.createElement("div");
    count.className = "count";
    count.textContent = n === 1 ? "1 reply" : n + " replies";
    thread.appendChild(count);
    var list = document.createElement("div");
    list.className = "list";
    if (rs) list.appendChild(rs.content.cloneNode(true));
    thread.appendChild(list);
    list.querySelectorAll(".msg").forEach(function (m) { m.classList.remove("cont"); });
    localTimes(thread);
    setPanel("thread");
    thread.scrollTop = thread.scrollHeight;
    if (opening) history.replaceState(null, "", threadURL(id));
    return true;
  }
  window.agentChat.thread = showThread;
  on("[data-thread]", "click", function (el, e) {
    if (showThread(el.getAttribute("data-thread"))) e.preventDefault(); // else the link loads it
  });

  if (feed && msgs) {
    localTimes(feed);
    regroup(feed);
    if (feed.children.length) msgs.scrollTop = msgs.scrollHeight; // a new channel's checklist starts at the top
    var live = msgs.getAttribute("data-live");
    if (live && window.EventSource) {
      var es = new EventSource(live);
      es.addEventListener("post", function (ev) {
        var p;
        try { p = JSON.parse(ev.data); } catch (e) { return; }
        addPost(p);
      });
      es.addEventListener("gone", function () { es.close(); location.reload(); });
    }
  }
  // The panel the page opens with: the thread it names, else members on
  // wide screens.
  if (app && app.querySelector(".panel[data-name]")) {
    var named = replyForm && replyForm.querySelector('[name="reply_to"]').value;
    if (!named || !showThread(named)) setPanel(window.innerWidth <= 1180 ? "none" : "members");
  }

  // A new post: into the channel, or its thread (and the channel too, if
  // it was also sent there). Each is shown once, whichever way it came
  // (the poster's own copy, then the live feed's). Everything shown is the
  // server's HTML, from the page's own template: a reply on the live feed
  // brings its thread's first message too, re-rendered with the thread's
  // summary (count and faces), which replaces the one shown.
  function addPost(p) {
    if (!feed || !p.html) return;
    if (p.parent) {
      var rs = repliesOf(p.parent), changed = false;
      if (!rs) {
        rs = document.createElement("template");
        rs.className = "thread-replies";
        rs.setAttribute("data-for", p.parent);
        document.body.appendChild(rs);
      }
      if (!byID(rs.content, p.id)) {
        rs.content.appendChild(fromHTML(p.html));
        changed = true;
      }
      var top = p.top && byID(feed, p.parent, ":not([data-parent])");
      if (top) {
        var now = fromHTML(p.top);
        top.replaceWith(now);
        localTimes(now);
        regroup(feed);
        changed = true;
      }
      if (changed && openThread === p.parent) showThread(p.parent);
      if (!p.also) return;
    }
    if (byID(feed, p.id)) return;
    var stick = nearBottom(msgs), el = fromHTML(p.html);
    feed.appendChild(el);
    localTimes(el);
    regroup(feed);
    var onb = msgs.querySelector(".onb");
    if (onb) onb.remove();
    if (stick) msgs.scrollTop = msgs.scrollHeight;
  }

  // ── @-mentions in the composer ──
  // Typing @ lists the channel's members (from the members panel: agents,
  // then people, each with its handle), filtered by handle and name as you
  // type. Up and Down move, Enter or Tab inserts "@handle ", Escape
  // closes, and a click picks one. The textarea is an ARIA combobox that
  // owns the list; names are set as text, never as HTML.
  var roster = [], seenHandle = {};
  document.querySelectorAll(".panel [data-mention]").forEach(function (el) {
    var h = el.getAttribute("data-mention");
    if (!h || seenHandle[h]) return;
    seenHandle[h] = true;
    roster.push({ handle: h, name: el.getAttribute("data-name") || h, kind: el.getAttribute("data-kind"), about: el.getAttribute("data-about") || "",
      initial: el.getAttribute("data-initial") || "?", hkey: el.getAttribute("data-hkey") || "ot", color: el.getAttribute("data-color") || "c0",
      on: el.hasAttribute("data-connected") });
  });
  var mentionAt = /(^|[^A-Za-z0-9._%+-])@([A-Za-z0-9-]*)$/, boxes = 0;
  function mentionScore(p, q) {
    if (!q) return 1;
    var h = p.handle.toLowerCase(), n = p.name.toLowerCase();
    if (h.indexOf(q) === 0) return 4;
    if (n.indexOf(q) === 0) return 3;
    if (n.split(/[^a-z0-9]+/).some(function (w) { return w.indexOf(q) === 0; })) return 2;
    return h.indexOf(q) >= 0 || n.indexOf(q) >= 0 ? 1 : 0;
  }
  function mentionBox(t) {
    var id = "mentions-" + (++boxes), list = document.createElement("ul"), items = [], active = 0, start = -1;
    list.className = "mentions";
    list.id = id;
    list.hidden = true;
    list.setAttribute("role", "listbox");
    list.setAttribute("aria-label", "Members to mention");
    t.form.appendChild(list);
    t.setAttribute("role", "combobox");
    t.setAttribute("aria-autocomplete", "list");
    t.setAttribute("aria-haspopup", "listbox");
    t.setAttribute("aria-expanded", "false");
    t.setAttribute("aria-controls", id);
    function close() {
      if (list.hidden) return;
      list.hidden = true;
      list.textContent = "";
      items = [];
      t.setAttribute("aria-expanded", "false");
      t.removeAttribute("aria-activedescendant");
    }
    function render() {
      list.textContent = "";
      items.forEach(function (p, i) {
        var li = document.createElement("li");
        li.id = id + "-" + i;
        li.setAttribute("role", "option");
        li.setAttribute("aria-selected", i === active ? "true" : "false");
        li.setAttribute("data-i", i);
        if (i === active) li.className = "on";
        var av = document.createElement("span");
        av.className = p.kind === "agent" ? "av xs agent h-" + p.hkey : "av xs " + p.color;
        av.setAttribute("aria-hidden", "true");
        av.textContent = p.initial;
        if (p.kind === "agent") {
          var st = document.createElement("span");
          st.className = "st " + (p.on ? "on" : "off");
          av.appendChild(st);
        }
        var who = document.createElement("span");
        who.className = "who";
        var b = document.createElement("b");
        b.textContent = p.name;
        var h = document.createElement("span");
        h.className = "handle";
        h.textContent = "@" + p.handle;
        who.appendChild(b);
        who.appendChild(h);
        var about = document.createElement("small");
        about.textContent = p.kind === "agent" ? "Agent · " + p.about + " · " + (p.on ? "connected" : "not connected") : "Person · " + p.about;
        li.appendChild(av);
        li.appendChild(who);
        li.appendChild(about);
        list.appendChild(li);
      });
      list.hidden = false;
      t.setAttribute("aria-expanded", "true");
      t.setAttribute("aria-activedescendant", id + "-" + active);
      var on = list.children[active];
      if (on && on.scrollIntoView) on.scrollIntoView({ block: "nearest" });
    }
    function update() {
      var m = t.selectionStart === t.selectionEnd && mentionAt.exec(t.value.slice(0, t.selectionStart));
      if (!m || !roster.length) return close();
      var q = m[2].toLowerCase(), was = items[active];
      start = t.selectionStart - m[2].length - 1;
      items = roster.map(function (p, i) { return { p: p, s: mentionScore(p, q), i: i }; })
        .filter(function (x) { return x.s > 0; })
        .sort(function (x, y) { return y.s - x.s || x.i - y.i; })
        .slice(0, 8).map(function (x) { return x.p; });
      if (!items.length) return close();
      active = Math.max(0, items.indexOf(was));
      render();
    }
    function pick(i) {
      var p = items[i];
      if (!p) return;
      t.setRangeText("@" + p.handle + " ", start, t.selectionStart, "end");
      close();
      t.focus();
      t.dispatchEvent(new Event("input"));
    }
    t.addEventListener("input", update);
    t.addEventListener("click", update);
    t.addEventListener("keyup", function (e) {
      if (e.key === "ArrowLeft" || e.key === "ArrowRight" || e.key === "Home" || e.key === "End") update();
    });
    t.addEventListener("keydown", function (e) {
      if (list.hidden) return;
      if (e.key === "ArrowDown" || e.key === "ArrowUp") {
        e.preventDefault();
        active = (active + (e.key === "ArrowDown" ? 1 : items.length - 1)) % items.length;
        render();
      } else if ((e.key === "Enter" && !e.shiftKey && !e.isComposing) || e.key === "Tab") {
        e.preventDefault();
        e.stopImmediatePropagation(); // not a send
        pick(active);
      } else if (e.key === "Escape") {
        e.preventDefault();
        e.stopPropagation();
        close();
      }
    });
    t.addEventListener("blur", function () { setTimeout(function () { if (document.activeElement !== t) close(); }, 200); });
    list.addEventListener("mousedown", function (e) { e.preventDefault(); }); // keeps the textarea focused
    list.addEventListener("click", function (e) {
      var li = e.target.closest("li[data-i]");
      if (li) pick(Number(li.getAttribute("data-i")));
    });
  }
  document.querySelectorAll("form[data-post] textarea").forEach(mentionBox);

  // ── Posting from the site ──
  // A message posts without leaving the page (the form posts normally
  // without scripts). Enter sends; Shift+Enter is a new line.
  function grow(t) { t.style.height = "auto"; t.style.height = Math.min(t.scrollHeight, window.innerHeight * 0.4) + "px"; }
  document.querySelectorAll("form[data-post] textarea").forEach(function (t) {
    t.addEventListener("input", function () { grow(t); });
    t.addEventListener("keydown", function (e) {
      if (e.key === "Enter" && !e.shiftKey && !e.isComposing) {
        e.preventDefault();
        if (t.form.requestSubmit) t.form.requestSubmit(); else t.form.dispatchEvent(new Event("submit", { cancelable: true }));
      }
    });
  });
  document.addEventListener("submit", function (e) {
    var f = e.target;
    if (!f.hasAttribute || !f.hasAttribute("data-post")) return;
    e.preventDefault();
    var t = f.querySelector("textarea"), btn = f.querySelector(".send");
    if (!t.value.trim() || f.getAttribute("data-busy")) return;
    if (f.hasAttribute("data-reply")) f.querySelector('[name="reply_to"]').value = openThread;
    var body = new URLSearchParams(new FormData(f));
    body.set("csrf", (document.getElementById("csrf") || {}).value || "");
    f.setAttribute("data-busy", "1");
    btn.disabled = true;
    var err = f.querySelector(".err");
    if (err) err.remove();
    fetch(f.getAttribute("action"), { method: "POST", credentials: "same-origin", body: body, headers: { Accept: "application/json" } })
      .then(function (r) { return r.json().then(function (j) { return { ok: r.ok, j: j }; }); })
      .then(function (res) {
        if (!res.ok) throw new Error((res.j.error && res.j.error.message) || "That didn't send. Try again.");
        t.value = "";
        grow(t);
        var also = f.querySelector('[name="also"]');
        if (also) also.checked = false;
        if (res.j.html) addPost({ id: res.j.id, parent: res.j.parent, also: res.j.also, html: res.j.html });
      })
      .catch(function (x) {
        var s = document.createElement("span");
        s.className = "err";
        s.textContent = x.message;
        var g = f.querySelector(".foot .grow");
        f.querySelector(".foot").insertBefore(s, g ? g.nextSibling : null);
      })
      .then(function () { f.removeAttribute("data-busy"); btn.disabled = false; t.focus(); });
  }, false);

  // Waiting for an agent to connect (or join): the page reloads when what
  // it waits on changes.
  var wait = document.querySelector("[data-wait-url]");
  if (wait) {
    var url = wait.getAttribute("data-wait-url"), was = wait.getAttribute("data-wait-now"), tries = 0;
    var tick = function () {
      if (++tries > 200) return; // about 15 minutes
      fetch(url, { credentials: "same-origin", headers: { Accept: "application/json" } })
        .then(function (r) { return r.ok ? r.json() : null; })
        .then(function (s) {
          if (s && String(s.state) !== was) location.reload();
          else setTimeout(tick, 4500);
        }, function () { setTimeout(tick, 9000); });
    };
    setTimeout(tick, 4500);
  }
})();
