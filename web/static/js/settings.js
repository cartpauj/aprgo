// Settings page interactions: live-reload after save, beacon row add/remove.
(function () {
  // Reload after a successful settings save so any theme/state changes apply.
  var form = document.querySelector('form[hx-post="/settings/save"]');
  if (form && window.htmx) {
    form.addEventListener("htmx:afterRequest", function (e) {
      if (e.detail.successful) setTimeout(function () { location.reload(); }, 700);
    });
  }

  var addBtn = document.getElementById("beacon-add");
  var countEl = document.getElementById("beacon-count");
  var beaconList = document.getElementById("beacon-list");
  if (!beaconList) return;

  // ── Visual symbol picker ────────────────────────────────────────────
  // Replaces the cryptic dropdown of 2-char codes with rendered icon
  // swatches. The hidden input.beacon-symbol-value (one per picker) is
  // what the server reads — selecting a swatch updates it. "Custom"
  // reveals the adjacent text input; typing 2 chars there sets the value
  // and renders a preview swatch.
  //
  // Sprite math (matches map.js popupIcon): table char '/' = primary (0),
  // '\\' = alternate (1); anything else = overlay letter on alt table,
  // with the overlay char rendered on top from sprite 2.
  var SYMBOL_PRESETS = [
    {code: "I&",  label: "Tx-iGate"},
    {code: "R&",  label: "RX-only iGate"},
    {code: "/&",  label: "Gateway (primary)"},
    {code: "\\&", label: "Tx-iGate (no overlay)"},
    {code: "S#",  label: "Digipeater (S overlay)"},
    {code: "/#",  label: "Digipeater (primary)"},
    {code: "\\#", label: "Digipeater (alt)"},
    {code: "/-",  label: "House / QTH"},
    {code: "/r",  label: "Repeater"},
  ];
  function spriteIconHTML(code) {
    if (!code || code.length < 2) return "";
    var sprite = code[0] === "/" ? "0" : "1";
    var idx = code.charCodeAt(1) - 0x21;
    var x = -((idx % 16) * 24);
    var y = -(Math.floor(idx / 16) * 24);
    var base =
      '<span class="aprs-sym" style="background-image:url(/static/aprs-symbols-48-' +
      sprite + '.png);background-position:' + x + 'px ' + y + 'px;"></span>';
    // Overlay letter sprite for non-table-char first byte.
    if (code[0] !== "/" && code[0] !== "\\") {
      var oc = code.charCodeAt(0) - 0x21;
      var ox = -((oc % 16) * 24);
      var oy = -(Math.floor(oc / 16) * 24);
      return (
        '<span class="aprs-sym-wrap">' + base +
        '<span class="aprs-sym-overlay" style="background-image:url(/static/aprs-symbols-48-2.png);background-position:' +
        ox + 'px ' + oy + 'px;"></span></span>'
      );
    }
    return base;
  }
  function labelFor(code) {
    for (var i = 0; i < SYMBOL_PRESETS.length; i++) {
      if (SYMBOL_PRESETS[i].code === code) return SYMBOL_PRESETS[i].label;
    }
    return "Custom (" + code + ")";
  }
  function renderPicker(picker) {
    var hidden = picker.querySelector(".beacon-symbol-value");
    var swatches = picker.querySelector(".beacon-symbol-swatches");
    var labelEl = picker.querySelector(".beacon-symbol-label");
    var iconEl = picker.querySelector(".beacon-symbol-trigger-icon");
    var customIn = picker.querySelector(".beacon-symbol-custom");
    var current = hidden.value;
    var isCustom = current === "custom" || !SYMBOL_PRESETS.some(function (p) { return p.code === current; });
    // Popover: render preset swatches + a custom-toggle tile
    var html = "";
    for (var i = 0; i < SYMBOL_PRESETS.length; i++) {
      var p = SYMBOL_PRESETS[i];
      var sel = !isCustom && p.code === current ? " is-selected" : "";
      html +=
        '<button type="button" class="beacon-symbol-swatch' + sel + '"' +
        ' data-code="' + p.code.replace(/&/g, "&amp;") + '"' +
        ' title="' + p.label + '">' + spriteIconHTML(p.code) + '</button>';
    }
    var customSel = isCustom ? " is-selected" : "";
    html +=
      '<button type="button" class="beacon-symbol-swatch beacon-symbol-swatch-custom' + customSel + '"' +
      ' data-code="custom" title="Custom">✎</button>';
    swatches.innerHTML = html;
    // Trigger: show the current symbol + label; custom input only when custom.
    var displayCode = isCustom ? (customIn.value.length === 2 ? customIn.value : "") : current;
    iconEl.innerHTML = displayCode ? spriteIconHTML(displayCode) : '<span class="beacon-symbol-trigger-placeholder">?</span>';
    if (isCustom) {
      labelEl.textContent = customIn.value ? labelFor(customIn.value) : "Custom";
      customIn.style.display = "";
    } else {
      labelEl.textContent = labelFor(current);
      customIn.style.display = "none";
    }
  }
  function closeAllPopovers(except) {
    document.querySelectorAll(".beacon-symbol-picker").forEach(function (p) {
      if (p === except) return;
      var pop = p.querySelector(".beacon-symbol-popover");
      var trig = p.querySelector(".beacon-symbol-trigger");
      if (pop) pop.hidden = true;
      if (trig) trig.setAttribute("aria-expanded", "false");
    });
  }
  document.addEventListener("click", function (e) {
    if (!e.target.closest(".beacon-symbol-picker")) closeAllPopovers(null);
  });
  function pickersIn(scope) {
    return scope.querySelectorAll(".beacon-symbol-picker");
  }
  pickersIn(beaconList).forEach(renderPicker);

  // Delegated trigger-click: toggle the popover. Closes any others first.
  beaconList.addEventListener("click", function (e) {
    var trig = e.target.closest(".beacon-symbol-trigger");
    if (!trig) return;
    e.preventDefault();
    var picker = trig.closest(".beacon-symbol-picker");
    var pop = picker.querySelector(".beacon-symbol-popover");
    var willOpen = pop.hidden;
    closeAllPopovers(willOpen ? picker : null);
    pop.hidden = !willOpen;
    trig.setAttribute("aria-expanded", willOpen ? "true" : "false");
  });

  // Delegated swatch-click: pick a preset, or toggle "Custom". Closes the popover.
  beaconList.addEventListener("click", function (e) {
    var btn = e.target.closest(".beacon-symbol-swatch");
    if (!btn) return;
    e.preventDefault();
    var picker = btn.closest(".beacon-symbol-picker");
    if (!picker) return;
    var hidden = picker.querySelector(".beacon-symbol-value");
    var code = btn.getAttribute("data-code");
    // Decode any &amp; from the data attribute back to & for the value
    code = code.replace(/&amp;/g, "&");
    hidden.value = code;
    renderPicker(picker);
    // Close popover. If they picked Custom, leave focus on the text input.
    var pop = picker.querySelector(".beacon-symbol-popover");
    pop.hidden = true;
    picker.querySelector(".beacon-symbol-trigger").setAttribute("aria-expanded", "false");
    if (code === "custom") {
      picker.querySelector(".beacon-symbol-custom").focus();
    }
  });

  // Custom-input → live update the hidden value + label + trigger icon as
  // the operator types. Only commit a real code when input is exactly 2 chars.
  beaconList.addEventListener("input", function (e) {
    if (!e.target.matches(".beacon-symbol-custom")) return;
    var picker = e.target.closest(".beacon-symbol-picker");
    if (!picker) return;
    var v = e.target.value;
    if (v.length === 2) {
      picker.querySelector(".beacon-symbol-value").value = v;
    } else {
      picker.querySelector(".beacon-symbol-value").value = "custom";
    }
    renderPicker(picker);
  });

  // Soft-delete: flip the hidden remove flag so the server drops the row on
  // save, then visually hide the fieldset. The Go side rebuilds the Beacons
  // slice from the form, so the row simply won't survive submit.
  beaconList.addEventListener("click", function (e) {
    var btn = e.target.closest(".beacon-remove");
    if (!btn) return;
    var fs = btn.closest("fieldset.beacon-row");
    if (!fs) return;
    var flag = fs.querySelector(".beacon-remove-flag");
    if (flag) flag.value = "1";
    fs.style.display = "none";
  });

  if (!addBtn || !countEl) return;
  addBtn.addEventListener("click", function () {
    var i = parseInt(countEl.value, 10) || 0;
    var fs = document.createElement("fieldset");
    fs.className = "beacon-row";
    fs.innerHTML = beaconRowTemplate(i);
    beaconList.insertBefore(fs, beaconList.lastElementChild);
    pickersIn(fs).forEach(renderPicker);
    countEl.value = String(i + 1);
  });

  // Markup mirrors the server-side rendering for an existing beacon. Keep in
  // sync with the {{range .BeaconViews}} block in settings.html.
  function beaconRowTemplate(i) {
    return (
      '<legend class="beacon-legend"><span class="beacon-legend-meta"><span>Every 30 min</span><span class="sep">·</span><span>direct</span></span></legend>' +
      '<input type="hidden" name="beacon_' + i + '_remove" value="0" class="beacon-remove-flag">' +
      '<div class="beacon-head">' +
        '<label class="beacon-head-name-label">Name' +
          '<span class="info-tip" tabindex="0" aria-label="More info">i<span class="info-tip-pop">Local identifier only — not transmitted on air. The over-the-air packet identifies your station by Callsign-SSID, Symbol, and Comment. The name is used internally to track scheduling (last-fired time), as the key for "Fire now" actions, and in logs.</span></span>' +
          '<input type="text" class="beacon-head-name" name="beacon_' + i + '_name" value="beacon' + i + '" size="16" maxlength="32" pattern="[A-Za-z0-9_-]+" required title="Letters, digits, dash, underscore; must be unique">' +
        '</label>' +
        '<div class="beacon-head-actions">' +
          '<label class="cb"><input type="checkbox" name="beacon_' + i + '_enabled" value="1" checked> Enabled</label>' +
          '<label class="cb"><input type="checkbox" name="beacon_' + i + '_messages" value="1" checked> Messaging-capable</label>' +
          '<button type="button" class="btn ghost beacon-remove">Remove</button>' +
        '</div>' +
      '</div>' +
      '<div class="beacon-body">' +
        '<label class="beacon-symbol-row">Symbol' +
          '<div class="beacon-symbol-picker" data-idx="' + i + '">' +
            '<input type="hidden" name="beacon_' + i + '_symbol" class="beacon-symbol-value" value="I&amp;">' +
            '<div class="beacon-symbol-trigger-row">' +
              '<button type="button" class="beacon-symbol-trigger" aria-haspopup="true" aria-expanded="false">' +
                '<span class="beacon-symbol-trigger-icon"></span>' +
                '<span class="beacon-symbol-label"></span>' +
                '<span class="beacon-symbol-caret" aria-hidden="true">▾</span>' +
              '</button>' +
              '<input type="text" class="beacon-symbol-custom" name="beacon_' + i + '_symbol_custom" placeholder="2 chars (e.g. /j)" maxlength="2" minlength="2" pattern="..">' +
            '</div>' +
            '<div class="beacon-symbol-popover" hidden><div class="beacon-symbol-swatches"></div></div>' +
          '</div>' +
        '</label>' +
        '<label class="beacon-comment">Comment' +
          '<div class="comment-row">' +
            '<input type="text" id="beacon-comment-' + i + '" name="beacon_' + i + '_comment" placeholder="aprgo status" maxlength="43">' +
            '<button type="button" class="btn ghost comment-helpers-btn" data-target="beacon-comment-' + i + '" title="Open structured-field composer">Helpers</button>' +
          '</div>' +
        '</label>' +
      '</div>' +
      '<details class="beacon-advanced">' +
        '<summary>Advanced — path, interval, callsign override, ambiguity</summary>' +
        '<div class="beacon-advanced-body">' +
          '<label>Path<input type="text" name="beacon_' + i + '_path" placeholder="WIDE2-1"></label>' +
          '<label>Interval (min)<input type="number" name="beacon_' + i + '_every_min" value="30" min="10" max="1440"></label>' +
          '<label>Callsign override<input type="text" name="beacon_' + i + '_callsign" placeholder="(uses station callsign)" pattern="[A-Za-z0-9-]*"></label>' +
          '<label>Position ambiguity' +
            '<select name="beacon_' + i + '_ambiguity">' +
              '<option value="0" selected>0 — full precision (~18 m)</option>' +
              '<option value="1">1 — ~185 m</option>' +
              '<option value="2">2 — ~1.8 km</option>' +
              '<option value="3">3 — ~18 km</option>' +
              '<option value="4">4 — ~111 km (degree-only)</option>' +
            '</select>' +
          '</label>' +
        '</div>' +
      '</details>'
    );
  }
})();

// ─── APRS-IS server region picker ────────────────────────────────────────
// The text input (id "is-server-custom") still carries the submitted value;
// the select (id "is-server-region") is presentation-only. On page load, if
// the saved host matches a known regional rotate, snap the select to it and
// hide the text input. Otherwise show the text input with "Custom…" selected.
(function () {
  var sel = document.getElementById("is-server-region");
  var txt = document.getElementById("is-server-custom");
  if (!sel || !txt) return; // section not rendered (offline mode)

  function setCustomVisible(show) {
    txt.style.display = show ? "" : "none";
  }

  var current = txt.value.trim();
  var matched = false;
  for (var i = 0; i < sel.options.length; i++) {
    if (sel.options[i].value === current) {
      sel.value = current;
      matched = true;
      break;
    }
  }
  if (matched) {
    setCustomVisible(false);
  } else {
    sel.value = "custom";
    setCustomVisible(true);
  }

  sel.addEventListener("change", function () {
    if (sel.value === "custom") {
      setCustomVisible(true);
      txt.focus();
    } else {
      txt.value = sel.value;
      setCustomVisible(false);
    }
  });
})();

// ─── Advanced-mode flag dependencies ────────────────────────────────────
// Some gating/digi flags only do anything when others are on. e.g. you
// can't "Gate IS → RF" without "Master TX enable", and "Viscous delay"
// is meaningless without a fill-in digipeater to delay. Grey out the
// dependents whose prerequisites aren't met; preserve their stored
// values so the operator's intent survives a transient prerequisite
// toggle. Re-evaluate on every checkbox change.
(function () {
  function input(name) {
    return document.querySelector(
      '.settings-section input[name="' + name + '"]'
    );
  }
  function isOn(name) {
    var el = input(name);
    return !!(el && el.checked);
  }
  function setInactive(name, inactive, reason) {
    var el = input(name);
    if (!el) return;
    var label = el.closest("label");
    if (!label) return;
    label.classList.toggle("is-inactive", inactive);
    if (inactive) label.setAttribute("data-inactive-reason", reason);
    else label.removeAttribute("data-inactive-reason");
  }
  function reconcile() {
    var txOff = !isOn("tx_enable");
    var offline = isOn("offline_mode");
    var digi1 = isOn("digipeat_wide1");
    var rfToIs = isOn("gate_rf_to_is");

    // Audited against internal/gate/gate.go to make sure each rule
    // reflects where the flag is actually consulted.

    // Gate IS → RF: needs TX (to TX on radio) and IS (to receive).
    setInactive("gate_is_to_rf", txOff || offline,
      txOff ? "Master TX enable is off" : "Offline mode disables APRS-IS");

    // Digipeaters: need TX.
    setInactive("digipeat_wide1", txOff, "Master TX enable is off");
    setInactive("digipeat_wide2", txOff, "Master TX enable is off");

    // Gate RF → IS: needs IS connection. Doesn't need TX (uses TCP).
    setInactive("gate_rf_to_is", offline, "Offline mode disables APRS-IS");

    // Messaging-only is a filter on RF→IS gating (rfToISAction in
    // gate.go:116). IS→RF already only allows messages by spec, so the
    // flag has no effect on that direction. It's a no-op when there's
    // no RF→IS gating to filter.
    setInactive("messaging_only_mode", offline || !rfToIs,
      offline ? "Offline mode disables APRS-IS" : "Gate RF → APRS-IS is off");

    // Viscous delay only matters for WIDE1-1 fill-in packets (gate.go:254
    // gates viscous on n==1). Inactive when WIDE1 digi is off or TX is off.
    setInactive("viscous_delay", !digi1 || txOff,
      txOff ? "Master TX enable is off" : "No fill-in digipeater to delay");

    // Preemptive digipeat is sufficient on its own to enter the digipeat
    // decision (gate.go:88 — `WIDE1 || WIDE2 || Preemptive`). So it does
    // NOT require either WIDE flag — operator can run "preemptive-only"
    // digi for explicit MYCALL paths. Only TX is required.
    setInactive("preemptive_digipeat", txOff, "Master TX enable is off");

    // Recent-RF window is consulted by both the IS→RF gate decision AND
    // the dashboard's IS-side inclusion filter. Both require IS to be
    // connected; only Offline mode makes it truly no-op.
    setInactive("igate_recent_rf_minutes", offline, "Offline mode disables APRS-IS");
  }
  // Bind to every advanced-flag checkbox so any change re-evaluates the
  // whole graph (some rules depend on combinations).
  var names = [
    "tx_enable", "gate_rf_to_is", "gate_is_to_rf",
    "digipeat_wide1", "digipeat_wide2", "viscous_delay",
    "offline_mode", "messaging_only_mode", "preemptive_digipeat",
  ];
  names.forEach(function (n) {
    var el = input(n);
    if (el) el.addEventListener("change", reconcile);
  });
  reconcile();
})();

// Allow-send-bulletins confirmation. When the operator ticks the
// checkbox, ask them to explicitly agree before letting the form save
// with it enabled. If they cancel the confirm dialog we untick so they
// can't accidentally enable broadcasts. Doing this in JS is OK because
// the server still requires the box on POST — JS just keeps the UI
// honest.
(function () {
  var cb = document.getElementById("allow-send-bulletins");
  if (!cb) return;
  cb.addEventListener("change", function () {
    if (!cb.checked) return;
    var ok = confirm(
      "Enable bulletin sending?\n\n" +
      "Bulletins are BROADCASTS — every station in your RF range " +
      "and many APRS-IS subscribers will see them.\n\n" +
      "By enabling, you agree to use this responsibly:\n" +
      "• Use sparingly (nets, EmComm, club announcements)\n" +
      "• Don't repeat the same content too often\n" +
      "• Don't use for personal chat or off-topic content\n" +
      "• Don't impersonate NWS / SKYWARN / emergency services\n\n" +
      "Continue?"
    );
    if (!ok) {
      cb.checked = false;
    }
  });
})();
