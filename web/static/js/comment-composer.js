// Beacon comment composer modal. Triggered from the "⚙ Helpers"
// button next to each beacon's Comment field; reads any encoded
// fragments already in the comment, populates the form, lets the
// operator edit/add structured fields, then re-encodes and writes
// back into the comment input. APRS spec refs:
//   altitude  /A=NNNNNN              APRS101 §8
//   frequency NNN.NNN[MHz] T<tone> +/-offset  Bruninga freqspec.txt
//   PHG       PHGxxxx                APRS101 §17
//   RNG       RNGxxxx                APRS101 §17

(function () {
  var dlg = document.getElementById("comment-composer");
  if (!dlg || typeof dlg.showModal !== "function") return; // no <dialog> support; skip

  // Current target input id while modal is open.
  var targetId = null;

  // Regex extractors / strippers for each encoded field. Used both to
  // pre-populate the form from an existing comment and to remove the
  // old fragment before writing the new one.
  var ALT_RE  = /\s*\/A=(\d{6})\b/;
  var FREQ_RE = /\b(\d{2,3}\.\d{2,4})\s?MHz/i;
  var TONE_RE = /\b(T\d{2,3}|D\d{3})\b/;
  // Offset like " +600" or "-600" (kHz). Match standalone signed integer.
  var OFFSET_RE = /(?:^|\s)([+-]\d{3,4})(?:\s|$)/;
  var PHG_RE  = /\bPHG(\d{4})[\dR]?\b/;
  var RNG_RE  = /\bRNG(\d{4})\b/;

  function $(id) { return document.getElementById(id); }

  function loadFromComment(s) {
    // Altitude
    var m = ALT_RE.exec(s);
    $("cc-alt-on").checked = !!m;
    $("cc-alt-val").value = m ? String(parseInt(m[1], 10)) : "";
    // Frequency + tone + offset
    m = FREQ_RE.exec(s);
    $("cc-freq-on").checked = !!m;
    $("cc-freq-mhz").value = m ? m[1] : "";
    m = TONE_RE.exec(s);
    $("cc-freq-tone").value = m ? m[1] : "";
    m = OFFSET_RE.exec(s);
    $("cc-freq-offset").value = m ? m[1] : "";
    // PHG
    m = PHG_RE.exec(s);
    $("cc-phg-on").checked = !!m;
    if (m) {
      $("cc-phg-power").value  = m[1].charAt(0);
      $("cc-phg-height").value = m[1].charAt(1);
      $("cc-phg-gain").value   = m[1].charAt(2);
      $("cc-phg-dir").value    = m[1].charAt(3);
    }
    // RNG
    m = RNG_RE.exec(s);
    $("cc-rng-on").checked = !!m;
    $("cc-rng-mi").value = m ? String(parseInt(m[1], 10)) : "";
  }

  function stripExisting(s) {
    s = s.replace(ALT_RE, "");
    s = s.replace(FREQ_RE, "");
    s = s.replace(TONE_RE, "");
    s = s.replace(OFFSET_RE, " ");
    s = s.replace(PHG_RE, "");
    s = s.replace(RNG_RE, "");
    return s.replace(/\s{2,}/g, " ").trim();
  }

  function buildFragments() {
    var parts = [];
    if ($("cc-alt-on").checked) {
      var ft = parseInt($("cc-alt-val").value, 10);
      if (isFinite(ft) && ft >= 0 && ft <= 999999) {
        parts.push("/A=" + String(ft).padStart(6, "0"));
      }
    }
    if ($("cc-freq-on").checked) {
      var mhz = $("cc-freq-mhz").value.trim();
      if (mhz) {
        var f = mhz + "MHz";
        var tone = $("cc-freq-tone").value.trim();
        if (tone) f += " " + tone;
        var off = $("cc-freq-offset").value.trim();
        if (off) f += " " + off;
        parts.push(f);
      }
    }
    if ($("cc-phg-on").checked) {
      var p = $("cc-phg-power").value;
      var h = $("cc-phg-height").value;
      var g = $("cc-phg-gain").value;
      var d = $("cc-phg-dir").value;
      parts.push("PHG" + p + h + g + d);
    }
    if ($("cc-rng-on").checked) {
      var mi = parseInt($("cc-rng-mi").value, 10);
      if (isFinite(mi) && mi > 0 && mi <= 9999) {
        parts.push("RNG" + String(mi).padStart(4, "0"));
      }
    }
    return parts;
  }

  var MAX_LEN = 43;

  function combinedFor(input) {
    var base = stripExisting(input.value);
    var frags = buildFragments();
    return (base + (frags.length ? " " + frags.join(" ") : "")).trim();
  }

  function refreshPreview() {
    var input = $(targetId);
    if (!input) return;
    var s = combinedFor(input);
    $("cc-preview-text").textContent = s || "(empty)";
    var count = $("cc-count");
    count.textContent = s.length + " / " + MAX_LEN;
    var over = s.length > MAX_LEN;
    count.classList.toggle("is-over", over);
    $("cc-preview-warn").hidden = !over;
  }

  // Delegate so dynamically-added beacon rows pick this up automatically.
  document.addEventListener("click", function (e) {
    var btn = e.target.closest(".comment-helpers-btn");
    if (!btn) return;
    targetId = btn.getAttribute("data-target");
    var input = $(targetId);
    if (!input) return;
    loadFromComment(input.value);
    refreshPreview();
    dlg.showModal();
  });

  // Live preview: any change inside the modal recomputes the combined string.
  dlg.addEventListener("input", refreshPreview);
  dlg.addEventListener("change", refreshPreview);

  $("cc-cancel").addEventListener("click", function () { dlg.close(); });

  $("cc-apply").addEventListener("click", function () {
    var input = $(targetId);
    if (!input) { dlg.close(); return; }
    var combined = combinedFor(input);
    if (combined.length > MAX_LEN) {
      var ok = confirm(
        "The combined comment is " + combined.length + " characters — over the " +
        MAX_LEN + "-char APRS limit. The end will be silently truncated on save. Apply anyway?"
      );
      if (!ok) return;
    }
    input.value = combined;
    input.dispatchEvent(new Event("input", { bubbles: true }));
    dlg.close();
  });
})();
