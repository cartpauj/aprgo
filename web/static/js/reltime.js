// Client-side relative-time renderer.
//
// Server emits `<time class="rel-time" data-ts="<unix>">` elements with
// empty bodies. We fill them in here on page load, repaint every 30s,
// and repaint after any HTMX swap so newly-inserted entries get text
// immediately. Centralized so pages that need relative times (messages,
// diagnostics, …) just need to drop in the right markup.
(function () {
  function fmtAgo(unixTs) {
    var d = Math.floor(Date.now() / 1000 - unixTs);
    if (d < 0)     return "just now";
    if (d < 10)    return "just now";
    if (d < 60)    return d + "s ago";
    if (d < 3600)  return Math.floor(d / 60) + "m ago";
    if (d < 86400) return Math.floor(d / 3600) + "h ago";
    return Math.floor(d / 86400) + "d ago";
  }
  function paint() {
    document.querySelectorAll(".rel-time[data-ts]").forEach(function (el) {
      var ts = parseInt(el.getAttribute("data-ts"), 10);
      if (isFinite(ts)) el.textContent = fmtAgo(ts);
    });
  }
  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", paint);
  } else {
    paint();
  }
  setInterval(paint, 30000);
  // HTMX swaps don't re-run page scripts, so listen for the post-swap event.
  document.body && document.body.addEventListener("htmx:afterSwap", paint);
  document.addEventListener("htmx:afterSwap", paint);
})();
