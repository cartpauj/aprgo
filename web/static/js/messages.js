// Messages page: client-side conversation search, live char counter,
// auto-scroll-to-bottom when new messages arrive in the thread pane.
(function () {
  // ── Filter conversation list as you type ────────────────────────────
  var search = document.getElementById("conv-search");
  var list = document.getElementById("conv-list");
  if (search && list) {
    search.addEventListener("input", function () {
      var q = search.value.trim().toLowerCase();
      var items = list.querySelectorAll(".chat-conv");
      items.forEach(function (el) {
        var call = (el.getAttribute("data-call") || "").toLowerCase();
        var body = (el.querySelector(".chat-conv-body") || {}).textContent || "";
        var hit = !q || call.indexOf(q) !== -1 || body.toLowerCase().indexOf(q) !== -1;
        el.style.display = hit ? "" : "none";
      });
    });
  }

  // ── Composer: live char counter, Enter-to-send ──────────────────────
  var composer = document.querySelector(".chat-composer");
  if (composer) {
    var ta = composer.querySelector("textarea[name=body]");
    var counter = composer.querySelector(".chat-charcount");
    if (ta && counter) {
      var update = function () {
        var n = ta.value.length;
        counter.textContent = n + " / 67";
        counter.classList.toggle("near-limit", n >= 60);
      };
      ta.addEventListener("input", update);
      update();
    }
    // Enter → send; Shift+Enter → newline (which APRS will normalize away
    // server-side, but keeps the textarea feeling normal). IME composition
    // is honored: Enter while a CJK composer is open doesn't fire send.
    if (ta) {
      ta.addEventListener("keydown", function (e) {
        if (e.key !== "Enter" || e.shiftKey || e.isComposing) return;
        e.preventDefault();
        if (composer.requestSubmit) composer.requestSubmit();
        else composer.querySelector("button[type=submit]").click();
      });
    }

    // Auto-clear the "Message queued" flash 3s after it appears. Without
    // this it sits at the bottom of the composer until page reload, which
    // looks like the message is stuck pending forever.
    var flash = composer.querySelector("#composer-flash");
    if (flash) {
      composer.addEventListener("htmx:afterRequest", function () {
        setTimeout(function () { flash.innerHTML = ""; }, 3000);
      });
    }
  }

  // ── Auto-scroll the thread to the bottom when content swaps ─────────
  // Triggered both on initial load and on every HTMX swap (poll or send).
  var thread = document.getElementById("thread-pane");
  function scrollBottom() {
    if (!thread) return;
    thread.scrollTop = thread.scrollHeight;
  }
  scrollBottom();
  if (thread && window.htmx) {
    thread.addEventListener("htmx:afterSwap", scrollBottom);
  }

  // ── Skip identical-content swaps ────────────────────────────────────
  // Per-target memory of the last server response body. If the next
  // response is byte-identical, tell HTMX to skip the swap entirely
  // — no DOM mutation, no repaint, no flash.
  var lastResponse = new WeakMap();
  document.body.addEventListener("htmx:beforeSwap", function (e) {
    if (!e.detail.target) return;
    var body = (e.detail.serverResponse || "").trim();
    var prior = lastResponse.get(e.detail.target);
    if (prior === body) {
      e.detail.shouldSwap = false;
    }
    lastResponse.set(e.detail.target, body);
  });

  // Seed the baseline immediately on page load. The initial server-rendered
  // markup inside the polled targets isn't byte-equal to what the polling
  // endpoints return (browser parsing normalizes whitespace + attributes),
  // so without this seeding the first poll *always* causes a swap-with-
  // identical-content flash. We pre-fetch each polled URL once, store the
  // raw text as the baseline, and the 5s poll then compares cleanly.
  document.querySelectorAll("[hx-get]").forEach(function (el) {
    var url = el.getAttribute("hx-get");
    if (!url) return;
    if (url.indexOf("/messages/thread") !== 0 && url.indexOf("/messages/conv-list") !== 0) return;
    fetch(url, { credentials: "same-origin" })
      .then(function (r) { return r.text(); })
      .then(function (t) { lastResponse.set(el, t.trim()); })
      .catch(function () { /* best-effort */ });
  });

  // Relative-time rendering lives in /static/js/reltime.js (shared across
  // pages). HTMX swaps trigger re-paint there.
})();
