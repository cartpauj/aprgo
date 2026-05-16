// Map page: render every recently-heard station as a Leaflet marker with
// a rich popup. The iGate's own beacon is rendered with a YOU badge and
// a separate popup builder that links into Settings.
//
// Reads page-scoped config from the data-* attributes on #map.
(function () {
  function init() {
    var mapEl = document.getElementById("map");
    var status = document.getElementById("map-status");
    if (!mapEl || !status) return;
    if (typeof L === "undefined") {
      status.textContent = "ERROR: Leaflet failed to load";
      status.style.color = "var(--err)";
      return;
    }
    var cfg = mapEl.dataset;
    var myLat = parseFloat(cfg.myLat) || 0;
    var myLon = parseFloat(cfg.myLon) || 0;
    var myCall = cfg.myCall || "";
    var mySymbol = cfg.mySymbol || "";
    var myComment = cfg.myComment || "";
    var startLat = myLat !== 0 || myLon !== 0 ? myLat : 40;
    var startLon = myLat !== 0 || myLon !== 0 ? myLon : -100;

    var map;
    try {
      map = L.map("map").setView([startLat, startLon], 8);
      L.tileLayer("https://{s}.tile.openstreetmap.org/{z}/{x}/{y}.png", {
        attribution: "© OpenStreetMap",
        maxZoom: 19,
      }).addTo(map);
    } catch (e) {
      status.textContent = "Map init error: " + e.message;
      status.style.color = "var(--err)";
      return;
    }

    // APRS-IS filter radius overlay — drawn once at init from the
    // r/<lat>/<lon>/<km> filter the server parsed out of state.ISFilter.
    // Anything outside this circle is invisible to your iGate via IS
    // (RF receipt is independent and unbounded by this).
    var filterLat = parseFloat(cfg.filterLat);
    var filterLon = parseFloat(cfg.filterLon);
    var filterKm = parseFloat(cfg.filterKm);
    if (isFinite(filterLat) && isFinite(filterLon) && filterKm > 0) {
      L.circle([filterLat, filterLon], {
        radius: filterKm * 1000, // meters
        color: "#000000",
        weight: 1,
        opacity: 0.9,
        fill: false,
        interactive: false,
      }).addTo(map);
      // Permanent label sitting just above the circle's top edge. Rough
      // conversion: 1° latitude ≈ 111 km. We put the tooltip's anchor at
      // the top point and tell it to render "above" (direction:top) so it
      // floats just outside the ring.
      var topLat = filterLat + filterKm / 111;
      L.tooltip({
        permanent: true,
        direction: "top",
        className: "filter-label",
        interactive: false,
        offset: [0, -2],
      }).setLatLng([topLat, filterLon])
        .setContent("IS (" + filterKm + " km)")
        .addTo(map);
    }

    var markers = new Map();
    var windowSel = document.getElementById("window-min");

    function esc(s) {
      if (s == null) return "";
      return String(s).replace(/[&<>"']/g, function (c) {
        return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c];
      });
    }
    function ago(ts) {
      var d = Math.floor(Date.now() / 1000 - ts);
      if (d < 60) return d + "s ago";
      if (d < 3600) return Math.floor(d / 60) + "m ago";
      if (d < 86400) return Math.floor(d / 3600) + "h ago";
      return Math.floor(d / 86400) + "d ago";
    }
    function popupIcon(symbol) {
      if (!symbol || symbol.length < 2) return "";
      var sprite = symbol[0] === "/" ? "0" : "1";
      var idx = symbol.charCodeAt(1) - 0x21;
      var x = -((idx % 16) * 24);
      var y = -(Math.floor(idx / 16) * 24);
      var html =
        '<span class="popup-icon" style="background-image:url(/static/aprs-symbols-24-' +
        sprite + '.png);background-position:' + x + 'px ' + y + 'px;"></span>';
      if (symbol[0] !== "/" && symbol[0] !== "\\") {
        var oc = symbol.charCodeAt(0) - 0x21;
        var ox = -((oc % 16) * 24);
        var oy = -(Math.floor(oc / 16) * 24);
        html =
          '<span class="popup-icon-wrap">' + html +
          '<span class="popup-icon-overlay" style="background-image:url(/static/aprs-symbols-24-2.png);background-position:' +
          ox + 'px ' + oy + 'px;"></span></span>';
      }
      return html;
    }
    function factsDL(facts) {
      var dl = '<dl class="popup-facts">';
      for (var i = 0; i < facts.length; i++) {
        dl += "<dt>" + esc(facts[i][0]) + "</dt><dd>" + esc(facts[i][1]) + "</dd>";
      }
      dl += "</dl>";
      return dl;
    }
    function buildPopup(st) {
      var parts = [];
      var header = popupIcon(st.symbol);
      header +=
        '<a class="popup-call" href="/stations/' + encodeURIComponent(st.callsign) +
        '">' + esc(st.callsign) + "</a>";
      // Symbol name + device chip share the right side of the header row.
      var symBits = [];
      if (st.symbol_name) symBits.push(esc(st.symbol_name));
      else if (st.symbol) symBits.push(esc(st.symbol));
      if (st.device) {
        symBits.push('<span class="popup-dev" title="' + esc(st.device_tocall || "") +
                     '">' + esc(st.device) + "</span>");
      }
      if (symBits.length) header += ' <span class="popup-symname">' + symBits.join(" · ") + "</span>";
      parts.push('<div class="popup-header">' + header + "</div>");
      parts.push(
        '<div class="popup-actions">' +
          '<a href="#" data-focus="' + esc(st.callsign) + '">👁 View only this station</a>' +
        '</div>'
      );

      var facts = [["Position", st.lat.toFixed(4) + ", " + st.lon.toFixed(4)]];
      if (st.altitude) facts.push(["Altitude", st.altitude.toLocaleString() + " ft"]);
      if (st.speed) facts.push(["Speed", st.speed + " mph"]);
      if (st.course) facts.push(["Heading", st.course + "°"]);
      if (st.frequency) facts.push(["Frequency", st.frequency]);
      if (st.status) facts.push(["Status", st.status]);
      if (st.phg) facts.push(["Coverage", st.phg]);
      else if (st.rng_mi) facts.push(["Range", "~" + st.rng_mi + " mi (RNG)"]);
      facts.push(["Heard", ago(st.last_seen)]);
      facts.push(["Packets", st.pkt_count]);
      if (st.last_path) {
        var pathVal = st.last_path;
        if (st.hops) pathVal += "  ·  " + st.hops;
        if (st.q) pathVal += "  ·  " + st.q +
          (st.igate ? " by " + st.igate : "");
        facts.push(["Path", pathVal]);
      }
      parts.push(factsDL(facts));

      // Weather summary line replaces the alphabet-soup comment for WX stations.
      // Non-WX stations show the regular cleaned-up comment as before.
      if (st.wx) parts.push('<div class="popup-comment popup-wx">' + esc(st.wx) + "</div>");
      else if (st.comment) parts.push('<div class="popup-comment">' + esc(st.comment) + "</div>");
      if (st.last_info)
        parts.push(
          '<details class="popup-raw"><summary>raw packet</summary><code>' +
            esc(st.last_info) + "</code></details>"
        );
      return '<div class="popup">' + parts.join("") + "</div>";
    }
    function buildSelfPopup(st) {
      var parts = [];
      var header = popupIcon(st.symbol);
      header +=
        '<a class="popup-call" target="_blank" rel="noopener" href="https://aprs.fi/info/a/' +
        encodeURIComponent(st.callsign) + '">' + esc(st.callsign) + "</a>";
      // YOU badge belongs adjacent to the callsign — pushing it to the
      // far right collides with Leaflet's close X and wraps awkwardly on
      // narrow popups. The symname (right-aligned) acts as the flex
      // spacer instead.
      header += '<span class="popup-self-badge">YOU</span>';
      if (st.symbol_name) header += ' <span class="popup-symname">' + esc(st.symbol_name) + "</span>";
      parts.push('<div class="popup-header">' + header + "</div>");

      var facts = [
        ["Position", st.lat.toFixed(4) + ", " + st.lon.toFixed(4)],
        ["Role", "This station (aprgo)"],
      ];
      parts.push(factsDL(facts));

      if (st.comment) parts.push('<div class="popup-comment">' + esc(st.comment) + "</div>");
      parts.push('<div class="popup-actions"><a href="/settings">Edit beacon settings →</a></div>');
      return '<div class="popup">' + parts.join("") + "</div>";
    }

    // Place our iGate. Same icon/popup conventions as other stations so it
    // visually anchors the map without looking like a special-case alien.
    if (isFinite(myLat) && isFinite(myLon)) {
      var symNames = {
        "I&": "Tx-iGate",
        "R&": "RX-only iGate",
        "I#": "iGate + Digipeater",
        "/&": "Gateway",
      };
      var selfStation = {
        callsign: myCall || "iGate",
        lat: myLat,
        lon: myLon,
        has_pos: true,
        symbol: mySymbol,
        symbol_name: symNames[mySymbol] || "",
        comment: myComment,
        last_seen: Math.floor(Date.now() / 1000),
      };
      var selfIcon = window.aprsIcon && mySymbol ? window.aprsIcon(mySymbol) : null;
      var selfMarker = selfIcon
        ? L.marker([myLat, myLon], { icon: selfIcon, title: selfStation.callsign, zIndexOffset: 1000 })
        : L.circleMarker([myLat, myLon], { radius: 8, color: "#58a6ff", fillColor: "#58a6ff", fillOpacity: 0.6 });
      selfMarker.bindPopup(buildSelfPopup(selfStation), { maxWidth: 360, minWidth: 260 }).addTo(map);
    }

    // Track trail polylines so a window-change can clear the old set.
    var trails = new Map();
    // Focus mode: when non-null, only this callsign's marker + trail render.
    // Toggled by the "View only this station" link inside the popup, the
    // ?focus=CALL URL param (from station detail's "Show on map" link), or
    // the floating "back to all" pill at the top of the map.
    var focused = null;
    var pendingPanTo = null; // callsign to pan/zoom to after first marker load
    try {
      var urlFocus = new URLSearchParams(window.location.search).get("focus");
      if (urlFocus) {
        focused = urlFocus.toUpperCase();
        pendingPanTo = focused;
      }
    } catch (e) { /* old browsers — silently skip */ }
    var focusPill = document.getElementById("focus-pill");
    var focusPillCall = document.getElementById("focus-pill-call");

    function applyFocusVisibility() {
      if (!focusPill) return;
      if (focused) {
        if (focusPillCall) focusPillCall.textContent = focused;
        focusPill.style.display = "";
      } else {
        focusPill.style.display = "none";
      }
    }

    async function loadStations(mins) {
      var r = await fetch("/api/stations?minutes=" + mins);
      var list = await r.json();
      markers.forEach(function (m) { map.removeLayer(m); });
      markers.clear();
      var withPos = 0;
      for (var i = 0; i < list.length; i++) {
        var st = list[i];
        if (!st.has_pos) continue;
        if (focused && st.callsign !== focused) continue;
        withPos++;
        var icon = window.aprsIcon && st.symbol ? window.aprsIcon(st.symbol) : null;
        var m = icon
          ? L.marker([st.lat, st.lon], { icon: icon, title: st.callsign })
          : L.circleMarker([st.lat, st.lon], { radius: 5, color: "#3fb950", fillColor: "#3fb950", fillOpacity: 0.6 });
        m.bindPopup(buildPopup(st), { maxWidth: 360, minWidth: 260 });
        m.addTo(map);
        markers.set(st.callsign, m);
      }
      return { withPos: withPos, total: list.length };
    }

    async function loadTrails(mins) {
      var r = await fetch("/api/trails?minutes=" + mins);
      var body = await r.json();
      trails.forEach(function (pl) { map.removeLayer(pl); });
      trails.clear();
      var movers = 0;
      for (var call in body) {
        if (focused && call !== focused) continue;
        var pts = body[call];
        if (!Array.isArray(pts) || pts.length < 2) continue;
        movers++;
        var latlngs = pts.map(function (p) { return [p[0], p[1]]; });
        // One polyline per moving station. Amber, semi-transparent, with
        // a tiny start-point marker so the operator can tell where the
        // station began the window. Latest position is the regular marker.
        var pl = L.polyline(latlngs, {
          color: "#ef4444",
          weight: 2.5,
          opacity: 0.65,
          smoothFactor: 1.5,
        });
        pl.addTo(map);
        // Start-of-window dot
        var start = L.circleMarker(latlngs[0], {
          radius: 4, color: "#ffffff", weight: 1.5,
          fillColor: "#ef4444", fillOpacity: 0.9,
        }).addTo(map);
        start.bindTooltip(call + " · start of window", { direction: "top" });
        trails.set(call, L.layerGroup([pl, start]).addTo(map));
      }
      return movers;
    }

    async function load() {
      var mins = windowSel.value;
      status.textContent = "loading…";
      try {
        var sRes = await loadStations(mins);
        var movers = await loadTrails(mins);
        if (focused) {
          status.textContent = "focused on " + focused;
        } else {
          status.textContent = sRes.withPos + " on map / " + sRes.total + " heard" +
            (movers ? " · " + movers + " moving" : "");
        }
        applyFocusVisibility();
        // After the first load only: if we came in with ?focus=CALL, pan
        // and zoom to that station's marker so the operator actually sees
        // their target. Subsequent refreshes keep whatever view they panned to.
        if (pendingPanTo) {
          var m = markers.get(pendingPanTo);
          if (m) {
            map.setView(m.getLatLng(), Math.max(map.getZoom(), 11));
          }
          pendingPanTo = null;
        }
      } catch (e) {
        status.textContent = "load error: " + e.message;
      }
    }
    windowSel.addEventListener("change", load);

    // Focus toggling via popup links + the floating pill.
    map.on("popupopen", function (e) {
      var node = e.popup.getElement();
      if (!node) return;
      var link = node.querySelector("[data-focus]");
      if (!link) return;
      link.addEventListener("click", function (ev) {
        ev.preventDefault();
        focused = link.getAttribute("data-focus");
        map.closePopup();
        load();
      });
    });
    if (focusPill) {
      focusPill.addEventListener("click", function () {
        focused = null;
        load();
      });
    }

    load();
    applyFocusVisibility();
    // Auto-refresh cadence. 15s is frequent enough that movement looks
    // smooth without hammering the DB on long windows.
    setInterval(load, 15000);
  }
  window.addEventListener("load", init);
})();
