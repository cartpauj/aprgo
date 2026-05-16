// Live RX feed + parsed/raw view toggle.
//
// Polls /api/feed every 2.5s with the last-seen cursor and prepends any
// new packet fragments to #rx-feed. For an APRS console where packets
// arrive every few seconds at most, 2.5s latency is invisible.
window.addEventListener("load", function () {
  // ── View mode toggle (parsed by default, raw on demand) ─────────────
  const btn = document.getElementById("view-mode-btn");
  function applyMode(mode) {
    if (mode === "raw") {
      document.body.classList.add("view-raw");
      if (btn) btn.textContent = "Show parsed";
    } else {
      document.body.classList.remove("view-raw");
      if (btn) btn.textContent = "Show raw";
    }
  }
  applyMode(localStorage.getItem("viewMode") || "parsed");
  if (btn) {
    btn.addEventListener("click", () => {
      const next = document.body.classList.contains("view-raw") ? "parsed" : "raw";
      localStorage.setItem("viewMode", next);
      applyMode(next);
    });
  }

  // ── Feed filter chips (all / rf / is / tx) ──────────────────────────
  // Toggles `data-feed-filter` on the feed container; CSS hides non-
  // matching .pkt rows. Choice persisted in localStorage so an operator
  // who lives on "TX only" doesn't have to re-pick every page load.
  const feedEl = document.getElementById("rx-feed");
  const chips = document.querySelectorAll(".feed-chip");
  function applyFilter(f) {
    if (!feedEl) return;
    feedEl.setAttribute("data-feed-filter", f || "all");
    chips.forEach((c) => c.classList.toggle("is-active", c.dataset.filter === f));
  }
  applyFilter(localStorage.getItem("feedFilter") || "all");
  chips.forEach((c) => {
    c.addEventListener("click", () => {
      const f = c.dataset.filter || "all";
      localStorage.setItem("feedFilter", f);
      applyFilter(f);
    });
  });

  // ── Live feed via polling ───────────────────────────────────────────
  const ledWrap = document.getElementById("feed-led");
  function setStatus(state) {
    if (!ledWrap) return;
    ledWrap.classList.remove("is-on", "is-warn", "is-err");
    if (state === "ok")   ledWrap.classList.add("is-on");
    if (state === "warn") ledWrap.classList.add("is-warn");
    if (state === "err")  ledWrap.classList.add("is-err");
    const lbl = ledWrap.querySelector(".led-label");
    if (lbl) {
      lbl.textContent =
        state === "ok"   ? "Live" :
        state === "warn" ? "Reconnecting…" :
        state === "err"  ? "Disconnected" :
                           "Connecting…";
    }
  }

  const feed = document.getElementById("rx-feed");
  if (!feed) return;

  // Initial cursor lives in a data attribute on #rx-feed, set by the
  // server when it renders the page. Fallback to 0 → poll returns the
  // server's current buffer on first request, but that would re-insert
  // packets we already rendered. The dashboard handler stamps the
  // attribute so we resume cleanly.
  let cursor = parseInt(feed.getAttribute("data-cursor") || "0", 10) || 0;
  let consecutiveErrors = 0;

  async function pollOnce() {
    try {
      const r = await fetch("/api/feed?since=" + cursor, { cache: "no-store" });
      if (!r.ok) throw new Error("HTTP " + r.status);
      const body = await r.json();
      cursor = body.cursor || cursor;
      consecutiveErrors = 0;
      setStatus("ok");
      if (body.items && body.items.length) renderItems(body.items);
    } catch (e) {
      consecutiveErrors++;
      // Tolerate one bad poll silently — second one in a row → "Reconnecting"
      // → repeated failures → "Disconnected".
      if (consecutiveErrors >= 3) setStatus("err");
      else if (consecutiveErrors >= 1) setStatus("warn");
    }
  }

  function renderItems(items) {
    // The empty state can be sitting at the top of the feed; clear it on
    // the first real packet so we don't double-stamp.
    const empty = feed.querySelector(".empty");
    if (empty) empty.remove();
    // Insert oldest-first so newest lands on top after all prepends.
    for (const it of items) {
      const tmpl = document.createElement("template");
      tmpl.innerHTML = (it.html || "").trim();
      const node = tmpl.content.firstChild;
      if (!node) continue;
      feed.insertBefore(node, feed.firstChild);
    }
    while (feed.children.length > 500) feed.removeChild(feed.lastChild);
  }

  // Kick off immediately, then every 2.5s. Pause polling when the tab is
  // hidden — saves bandwidth and battery. Catches up on visibility return.
  setStatus("");
  pollOnce();
  let timer = setInterval(pollOnce, 2500);
  document.addEventListener("visibilitychange", () => {
    if (document.hidden) {
      clearInterval(timer);
    } else {
      pollOnce();
      timer = setInterval(pollOnce, 2500);
    }
  });
});
