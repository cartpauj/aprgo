// Setup wizard — location step. Renders a Leaflet map; click drops a pin
// and fills the lat/lon inputs. SVG-only marker so no PNG assets needed.
(function () {
  function init() {
    var latIn = document.getElementById("lat-in");
    var lonIn = document.getElementById("lon-in");
    if (!latIn || !lonIn) return;
    var lat = parseFloat(latIn.value) || 40;
    var lon = parseFloat(lonIn.value) || -100;
    var zoom = latIn.value && lonIn.value ? 10 : 4;
    var map = L.map("setup-map").setView([lat, lon], zoom);
    L.tileLayer("https://{s}.tile.openstreetmap.org/{z}/{x}/{y}.png", {
      attribution: "© OpenStreetMap",
      maxZoom: 18,
    }).addTo(map);
    var marker = null;
    function place(la, lo) {
      if (marker) map.removeLayer(marker);
      marker = L.circleMarker([la, lo], {
        radius: 8,
        color: "#ffffff",
        weight: 2,
        fillColor: "#f59e0b",
        fillOpacity: 0.95,
      }).addTo(map);
      latIn.value = la.toFixed(6);
      lonIn.value = lo.toFixed(6);
    }
    if (latIn.value && lonIn.value) place(lat, lon);
    map.on("click", function (e) { place(e.latlng.lat, e.latlng.lng); });
  }
  if (typeof L !== "undefined") init();
  else window.addEventListener("load", init);
})();
