// APRS symbol → Leaflet icon helper. Uses Hessu's aprs-symbols sprite sheets
// (https://github.com/hessu/aprs-symbols, MIT licensed).
//
// Layout: each sprite is 16 columns × 8 rows of 24px cells, starting at ASCII '!'.
//   row = (code - 0x21) >> 4
//   col = (code - 0x21) & 0x0F
//
// Tables:
//   '/'  → primary table (aprs-symbols-24-0.png)
//   '\\' → alternate table (aprs-symbols-24-1.png)
//   any other char (0-9, A-Z) → alternate base + overlay char drawn from
//     aprs-symbols-24-2.png at the position of that char.
(function () {
  const SIZE = 24;
  const COLS = 16;

  function offsetFor(code) {
    const c = code.charCodeAt(0);
    if (c < 0x21 || c > 0x7e) return { x: 0, y: 0 };
    const idx = c - 0x21;
    return { x: -((idx % COLS) * SIZE), y: -(Math.floor(idx / COLS) * SIZE) };
  }

  function spriteUrl(table) {
    if (table === "/") return "/static/aprs-symbols-24-0.png";
    if (table === "\\") return "/static/aprs-symbols-24-1.png";
    return "/static/aprs-symbols-24-1.png"; // overlay base = alternate table
  }

  function isOverlay(table) {
    if (!table) return false;
    if (table === "/" || table === "\\") return false;
    return (table >= "0" && table <= "9") || (table >= "A" && table <= "Z");
  }

  // Returns an L.divIcon for a given 2-char symbol (table + code), or null.
  window.aprsIcon = function (symbol) {
    if (!symbol || symbol.length < 2 || !window.L) return null;
    const table = symbol[0];
    const code = symbol[1];
    const off = offsetFor(code);
    const url = spriteUrl(table);
    let html =
      '<div class="aprs-sym-base" style="background-image:url(' +
      url +
      ');background-position:' +
      off.x +
      "px " +
      off.y +
      'px;width:' +
      SIZE +
      "px;height:" +
      SIZE +
      'px;"></div>';
    if (isOverlay(table)) {
      const oOff = offsetFor(table);
      html +=
        '<div class="aprs-sym-overlay" style="background-image:url(/static/aprs-symbols-24-2.png);background-position:' +
        oOff.x +
        "px " +
        oOff.y +
        'px;width:' +
        SIZE +
        "px;height:" +
        SIZE +
        'px;position:absolute;left:0;top:0;pointer-events:none;"></div>';
    }
    return L.divIcon({
      html: '<div class="aprs-sym-wrap" style="position:relative;width:' + SIZE + "px;height:" + SIZE + 'px;">' + html + "</div>",
      className: "aprs-sym-icon",
      iconSize: [SIZE, SIZE],
      iconAnchor: [SIZE / 2, SIZE / 2],
      popupAnchor: [0, -SIZE / 2],
    });
  };
})();
