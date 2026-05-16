package server

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"html"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"

	"aprgo/internal/aprs"
	"aprgo/internal/ax25"
	"aprgo/internal/igate"
	"aprgo/internal/state"
	"aprgo/internal/store"
)

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	// Public
	mux.HandleFunc("/login", s.handleLogin)
	mux.HandleFunc("/logout", s.handleLogout)
	mux.HandleFunc("/static/", s.serveStatic)
	// /healthz: liveness (process up). Always 200 unless the binary is dead.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	// Browsers ask for /favicon.ico on every cold load regardless of <link>
	// hints. Serve the SVG; modern browsers accept it for the .ico request.
	mux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/static/favicon.svg", http.StatusMovedPermanently)
	})
	// /readyz: readiness (RF + IS + DB all good). 503 when degraded so an
	// external monitor (or k8s/systemd watchdog) can react.
	mux.HandleFunc("/readyz", s.handleReadyz)

	// Authed
	a := http.NewServeMux()
	a.HandleFunc("/", s.handleDashboard)
	a.HandleFunc("/api/feed", s.handleFeedPoll)
	a.HandleFunc("/map", s.handleMap)
	a.HandleFunc("/api/stations", s.apiStations)
	a.HandleFunc("/api/trails", s.apiTrails)
	a.HandleFunc("/stations", s.handleStations)
	a.HandleFunc("/stations/", s.handleStationDetail)
	a.HandleFunc("/messages", s.handleMessages)
	a.HandleFunc("/messages/send", s.handleMessageSend)
	a.HandleFunc("/messages/thread", s.handleMessagesThread)
	a.HandleFunc("/messages/conv-list", s.handleMessagesConvList)
	a.HandleFunc("/messages/cancel/", s.handleMessageCancel)
	a.HandleFunc("/messages/retry/", s.handleMessageRetry)
	a.HandleFunc("/settings", s.handleSettings)
	a.HandleFunc("/settings/save", s.handleSettingsSave)
	a.HandleFunc("/settings/password", s.handleChangePassword)
	a.HandleFunc("/diagnostics", s.handleDiagnostics)
	a.HandleFunc("/diagnostics/rows", s.handleDiagnosticsRows)
	// Wizard routes
	a.HandleFunc("/setup", s.wizardStart)
	a.HandleFunc("/setup/", s.wizardRouter)
	a.HandleFunc("/setup/save/", s.wizardSave)
	a.HandleFunc("/setup/save/tnc-confirm", s.handleTNCConfirm)
	a.HandleFunc("/setup/back", s.wizardBack)
	a.HandleFunc("/setup/tnc/scan", s.wizardBTScan)
	a.HandleFunc("/setup/tnc/pair-status", s.handlePairStatus)
	a.HandleFunc("/setup/tnc/pairing", s.handlePairingPage)

	// First-run wall: any unauthenticated landing that doesn't have a callsign
	// is forced through the wizard. The wizard itself is auth-gated to keep it
	// behind login (default password is still 'admin').
	mux.Handle("/", s.auth.RequireLogin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.state.Snapshot().SetupComplete && !strings.HasPrefix(r.URL.Path, "/setup") && r.URL.Path != "/logout" {
			http.Redirect(w, r, "/setup", http.StatusFound)
			return
		}
		a.ServeHTTP(w, r)
	})))
	return mux
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	snap := s.state.Snapshot()
	rfOK := s.rf.Connected() || snap.TNCKind == state.TNCNone
	isOK := s.is.Connected() || snap.Callsign == "" || snap.Passcode == ""
	verified := s.is.Verification() != igate.VerificationUnverified
	if rfOK && isOK && verified {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "ready (rf=%v is=%v verified=%v)\n", rfOK, isOK, verified)
		return
	}
	w.WriteHeader(http.StatusServiceUnavailable)
	fmt.Fprintf(w, "degraded (rf=%v is=%v verified=%v)\n", rfOK, isOK, verified)
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	theme := s.state.Snapshot().Theme
	switch r.Method {
	case http.MethodGet:
		s.render(w, "login.html", map[string]any{"Title": "Sign in", "Authed": false, "Theme": theme, "Next": r.URL.Query().Get("next")})
	case http.MethodPost:
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		ip := clientIP(r)
		if !s.loginLimit.Allow(ip) {
			w.WriteHeader(http.StatusTooManyRequests)
			s.render(w, "login.html", map[string]any{"Title": "Sign in", "Authed": false, "Theme": theme, "Error": "Too many attempts. Try again in 10 minutes.", "Next": r.FormValue("next")})
			return
		}
		user := r.FormValue("user")
		pass := r.FormValue("pass")
		if !s.auth.Check(user, pass) {
			s.loginLimit.Fail(ip)
			w.WriteHeader(http.StatusUnauthorized)
			s.render(w, "login.html", map[string]any{"Title": "Sign in", "Authed": false, "Theme": theme, "Error": "Invalid credentials", "Next": r.FormValue("next")})
			return
		}
		s.loginLimit.Success(ip)
		s.auth.IssueCookie(w, user)
		next := validateNext(r.FormValue("next"))
		http.Redirect(w, r, next, http.StatusFound)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.auth.ClearCookie(w)
	http.Redirect(w, r, "/login", http.StatusFound)
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	packets, _, cursor := s.recentSince(0)
	rendered := make([]template.HTML, len(packets))
	for i := range packets {
		j := len(packets) - 1 - i
		rendered[i] = template.HTML(s.renderPacketHTML(packets[j]))
	}
	data := s.common("Dashboard", r)
	data["St"] = s.state.Snapshot()
	data["RenderedPackets"] = rendered
	data["FeedCursor"] = cursor
	if r.URL.Query().Get("saved") == "1" {
		data["Flash"] = "Settings saved."
	}
	s.render(w, "dashboard.html", data)
}

// handleFeedPoll is the dashboard live-feed AJAX endpoint. Clients send
// ?since=<last_id> every few seconds. Response is JSON:
//
//	{"cursor": 1234, "items": [{"id": 1233, "html": "<div ..."}, ...]}
//
// `items` are returned oldest-first (caller prepends each in order so the
// newest lands on top). `cursor` is the current head — the client uses it
// as the next ?since= value. Returns an empty items array if nothing new.
func (s *Server) handleFeedPoll(w http.ResponseWriter, r *http.Request) {
	var cursor uint64
	if v := r.URL.Query().Get("since"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			cursor = n
		}
	}
	pkts, ids, head := s.recentSince(cursor)
	type item struct {
		ID   uint64 `json:"id"`
		HTML string `json:"html"`
	}
	out := struct {
		Cursor uint64 `json:"cursor"`
		Items  []item `json:"items"`
	}{Cursor: head, Items: make([]item, 0, len(pkts))}
	for i, p := range pkts {
		out.Items = append(out.Items, item{ID: ids[i], HTML: s.renderPacketHTML(p)})
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(out)
}

func (s *Server) renderPacketHTML(pkt aprs.Packet) string {
	ts := pkt.Frame.RxAt.Format("15:04:05")
	dirLabel := pkt.Frame.Origin.String()
	// Distinct CSS class per origin so dashboard styling can color them
	// differently: RF/IS in green-ish, TX in amber/orange so own
	// transmissions stand out from received traffic.
	dirClass := strings.ToLower(dirLabel)
	if dirClass == "" {
		dirClass = "rx"
	}
	pathPart := ""
	if len(pkt.Frame.Path) > 0 {
		pathPart = ` <span class="path">via ` + html.EscapeString(strings.Join(pkt.Frame.Path, ",")) + `</span>`
	}
	parsedInfo := parsedInfoHTML(pkt)
	rawInfo := html.EscapeString(string(pkt.Frame.Info))
	srcLink := fmt.Sprintf(`<a class="src" href="/stations/%s">%s</a>`,
		html.EscapeString(pkt.Frame.Src), html.EscapeString(pkt.Frame.Src))
	return fmt.Sprintf(
		`<div class="pkt %s"><span class="t">%s</span> <span class="dir %s">%s</span> %s&gt;<span class="dst">%s</span>%s <span class="info-parsed">%s</span><span class="info-raw">%s</span></div>`,
		dirClass, ts, dirClass, dirLabel,
		srcLink, html.EscapeString(pkt.Frame.Dest),
		pathPart, parsedInfo, rawInfo,
	)
}

func parsedInfoHTML(pkt aprs.Packet) string {
	d := pkt.Decoded
	var b strings.Builder
	wrote := false
	if d.Lat != nil && d.Lon != nil {
		fmt.Fprintf(&b, `<span class="pos">📍 %.4f, %.4f</span>`, *d.Lat, *d.Lon)
		wrote = true
	}
	if d.SymbolName != "" {
		fmt.Fprintf(&b, ` <span class="sym">[%s]</span>`, html.EscapeString(d.SymbolName))
		wrote = true
	} else if d.Symbol != "" {
		fmt.Fprintf(&b, ` <span class="sym">[%s]</span>`, html.EscapeString(d.Symbol))
		wrote = true
	}
	if d.Course >= 0 && d.Speed >= 0 {
		fmt.Fprintf(&b, ` <span class="motion">%d° @ %d mph</span>`, d.Course, d.Speed)
		wrote = true
	}
	if d.Altitude > 0 {
		fmt.Fprintf(&b, ` <span class="alt">%d ft</span>`, d.Altitude)
		wrote = true
	}
	if d.Frequency != "" {
		fmt.Fprintf(&b, ` <span class="freq">%s</span>`, html.EscapeString(d.Frequency))
		wrote = true
	}
	if d.Status != "" {
		fmt.Fprintf(&b, ` <span class="status">[%s]</span>`, html.EscapeString(d.Status))
		wrote = true
	}
	if d.Comment != "" {
		fmt.Fprintf(&b, ` <span class="cmt">%s</span>`, html.EscapeString(d.Comment))
		wrote = true
	}
	if d.IsAck {
		// MsgTo on an ACK is the callsign the ACK is being delivered to.
		// Showing it disambiguates "ACK 1 to whom?" especially when we're
		// relaying multiple message threads simultaneously.
		if d.MsgTo != "" {
			return fmt.Sprintf(`<span class="ack">✓ ACK %s</span> <span class="dim">→ %s</span>`,
				html.EscapeString(d.AckedID), html.EscapeString(d.MsgTo))
		}
		return fmt.Sprintf(`<span class="ack">✓ ACK %s</span>`, html.EscapeString(d.AckedID))
	}
	if d.IsTelemetry {
		var bits strings.Builder
		for _, on := range d.TelemBits {
			if on {
				bits.WriteByte('1')
			} else {
				bits.WriteByte('0')
			}
		}
		return fmt.Sprintf(
			`<span class="telem">📊 T#%d</span> <span class="telem-vals">%g · %g · %g · %g · %g · <code>%s</code></span>`,
			d.TelemSeq,
			d.TelemAnalog[0], d.TelemAnalog[1], d.TelemAnalog[2], d.TelemAnalog[3], d.TelemAnalog[4],
			bits.String(),
		)
	}
	if d.IsMessage {
		idPart := ""
		if d.MsgID != "" {
			idPart = ` <span class="msgid">{` + html.EscapeString(d.MsgID) + `}</span>`
		}
		return fmt.Sprintf(`<span class="msg">💬 → %s:</span> <span class="cmt">%s</span>%s`,
			html.EscapeString(d.MsgTo), html.EscapeString(d.MsgBody), idPart)
	}
	if wrote {
		return b.String()
	}
	return html.EscapeString(string(pkt.Frame.Info))
}

func (s *Server) handleMap(w http.ResponseWriter, r *http.Request) {
	data := s.common("Map", r)
	snap := s.state.Snapshot()
	// Pre-compute the iGate's own-marker symbol + comment from the first
	// enabled beacon. Doing this in Go keeps the template free of fragile
	// expressions that confuse html/template's JS-context auto-escaper.
	sym, cmt := "I&", ""
	for _, b := range snap.Beacons {
		if !b.Enabled {
			continue
		}
		if b.Symbol != "" && len(b.Symbol) == 2 {
			sym = b.Symbol
			cmt = b.Comment
			break
		}
	}
	data["MySymbol"] = sym
	data["MyComment"] = cmt
	// Pre-parse the APRS-IS filter for the radius-circle overlay. Only the
	// "r/<lat>/<lon>/<km>" form is supported; other filter shapes (callsign-
	// scoped "b/", prefix "p/", etc.) get nothing rendered.
	if !snap.OfflineMode {
		if lat, lon, km, ok := parseISFilterRadius(snap.ISFilter); ok {
			data["FilterLat"] = lat
			data["FilterLon"] = lon
			data["FilterKm"] = km
		}
	}
	s.render(w, "map.html", data)
}

// parseISFilterRadius extracts (lat, lon, km) from an APRS-IS server-side
// filter of the form "r/<lat>/<lon>/<km>". Returns ok=false for any other
// filter shape or malformed inputs.
func parseISFilterRadius(s string) (lat, lon float64, km int, ok bool) {
	if !strings.HasPrefix(s, "r/") {
		return 0, 0, 0, false
	}
	parts := strings.Split(s, "/")
	if len(parts) < 4 {
		return 0, 0, 0, false
	}
	var err error
	if lat, err = strconv.ParseFloat(parts[1], 64); err != nil {
		return 0, 0, 0, false
	}
	if lon, err = strconv.ParseFloat(parts[2], 64); err != nil {
		return 0, 0, 0, false
	}
	if km, err = strconv.Atoi(parts[3]); err != nil || km <= 0 {
		return 0, 0, 0, false
	}
	return lat, lon, km, true
}

// apiTrails returns per-station movement tracks within the given window.
// Only stations that have actually moved (>= 2 distinct positions) are
// included — static iGates / weather stations would just clutter the map.
// Response is a JSON object keyed by callsign: { "CALL-1": [[lat,lon,ts],...] }.
func (s *Server) apiTrails(w http.ResponseWriter, r *http.Request) {
	minutes := 60
	if v := r.URL.Query().Get("minutes"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			if n > 7*24*60 {
				n = 7 * 24 * 60
			}
			minutes = n
		}
	}
	since := time.Now().Add(-time.Duration(minutes) * time.Minute)
	// Direct indexed select on (ts, lat, lon) — no info-field re-parsing.
	// Intake (server.go LogPacket call) already applied range / null-island
	// / third-party guards before storing, so this loop only needs to deal
	// with consecutive-position dedupe + the teleport sanity check.
	pkts, err := s.store.PacketPositionsSince(since)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	const (
		minDeltaDeg = 0.00015 // ~16 m
		maxKmh      = 800.0   // teleport ceiling — airliner-cruise; planes survive
		degToKm     = 111.0   // rough avg km per degree of lat
	)
	trails := make(map[string][][3]float64)
	for _, p := range pkts {
		existing := trails[p.Source]
		if n := len(existing); n > 0 {
			last := existing[n-1]
			dLat, dLon := p.Lat-last[0], p.Lon-last[1]
			if dLat < 0 {
				dLat = -dLat
			}
			if dLon < 0 {
				dLon = -dLon
			}
			if dLat < minDeltaDeg && dLon < minDeltaDeg {
				continue
			}
			distKm := (dLat + dLon) * degToKm
			elapsedHr := (float64(p.Time.Unix()) - last[2]) / 3600.0
			if elapsedHr > 0 && distKm/elapsedHr > maxKmh {
				continue
			}
		}
		trails[p.Source] = append(existing, [3]float64{p.Lat, p.Lon, float64(p.Time.Unix())})
	}
	// Keep only stations with a real track (>= 2 distinct points).
	out := make(map[string][][3]float64, len(trails))
	for src, pts := range trails {
		if len(pts) >= 2 {
			out[src] = pts
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(out)
}

func (s *Server) apiStations(w http.ResponseWriter, r *http.Request) {
	minutes := 60
	if v := r.URL.Query().Get("minutes"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			if n > 7*24*60 {
				n = 7 * 24 * 60
			}
			minutes = n
		}
	}
	cutoff := time.Now().Add(-time.Duration(minutes) * time.Minute)
	list, err := s.store.HeardSince(cutoff)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	type stationOut struct {
		Callsign   string  `json:"callsign"`
		LastSeen   int64   `json:"last_seen"`
		Lat        float64 `json:"lat,omitempty"`
		Lon        float64 `json:"lon,omitempty"`
		HasPos     bool    `json:"has_pos"`
		Symbol     string  `json:"symbol,omitempty"`
		SymbolName string  `json:"symbol_name,omitempty"`
		Comment    string  `json:"comment,omitempty"`
		LastInfo   string  `json:"last_info,omitempty"`
		LastPath   string  `json:"last_path,omitempty"`
		PktCount   int     `json:"pkt_count"`
		Altitude   int     `json:"altitude,omitempty"`
		Speed      int     `json:"speed,omitempty"`
		Course     int     `json:"course,omitempty"`
		Frequency  string  `json:"frequency,omitempty"`
		Status     string  `json:"status,omitempty"`
	}
	out := make([]stationOut, 0, len(list))
	for _, st := range list {
		o := stationOut{
			Callsign: st.Callsign, LastSeen: st.LastSeen.Unix(),
			Symbol: st.Symbol, Comment: st.Comment,
			LastInfo: st.LastInfo, LastPath: st.LastPath, PktCount: st.PktCount,
		}
		if st.Lat.Valid && st.Lon.Valid {
			o.Lat = st.Lat.Float64
			o.Lon = st.Lon.Float64
			o.HasPos = true
		}
		dec := aprs.Decode(st.LastInfo, st.LastDest)
		o.SymbolName = dec.SymbolName
		o.Altitude = dec.Altitude
		if dec.Speed >= 0 {
			o.Speed = dec.Speed
		}
		if dec.Course >= 0 {
			o.Course = dec.Course
		}
		o.Frequency = dec.Frequency
		o.Status = dec.Status
		out = append(out, o)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (s *Server) handleStationDetail(w http.ResponseWriter, r *http.Request) {
	call := strings.ToUpper(strings.TrimPrefix(r.URL.Path, "/stations/"))
	// Defense in depth: must match callsign-SSID grammar (drops nulls, control chars, etc).
	if !callsignRE.MatchString(call) {
		http.Redirect(w, r, "/stations", http.StatusFound)
		return
	}

	snap := s.state.Snapshot()
	data := s.common("Station "+call, r)
	data["Callsign"] = call

	// Self-detection: we don't log our own callsign to the heard-stations
	// table (digipeated copies of our beacon would otherwise pollute that
	// "who have I heard" list), but TX-origin packets *are* stored so we
	// can show transmit history here. Hero is config-driven; packet list
	// below comes from the same PacketsBySource path as anyone else.
	if strings.EqualFold(call, snap.Callsign) {
		data["IsSelf"] = true
		data["St"] = snap
		for _, b := range snap.Beacons {
			if b.Enabled {
				data["SelfBeacon"] = b
				break
			}
		}
		// fall through to the shared packet-history rendering below.
	}

	stations, _ := s.store.HeardSince(time.Now().Add(-30 * 24 * time.Hour))
	var st *store.Station
	for i := range stations {
		if strings.EqualFold(stations[i].Callsign, call) {
			st = &stations[i]
			break
		}
	}
	packets, _ := s.store.PacketsBySource(call, 500)
	rendered := make([]template.HTML, 0, len(packets))
	for _, p := range packets {
		var origin ax25.Source
		if p.SrcKind == "IS" {
			origin = ax25.SrcIS
		} else {
			origin = ax25.SrcRF
		}
		fr := ax25.Frame{
			Src: p.Source, Dest: p.Dest, Info: []byte(p.Info), RxAt: p.Time,
			Origin: origin,
		}
		if p.Path != "" {
			fr.Path = strings.Split(p.Path, ",")
		}
		pkt := aprs.Parse(fr)
		rendered = append(rendered, template.HTML(s.renderPacketHTML(pkt)))
	}
	data["Station"] = st
	data["RenderedPackets"] = rendered
	data["PacketCount"] = len(packets)
	s.render(w, "station_detail.html", data)
}

func (s *Server) handleStations(w http.ResponseWriter, r *http.Request) {
	cutoff := time.Now().Add(-24 * time.Hour)
	list, _ := s.store.HeardSince(cutoff)
	data := s.common("Stations", r)
	data["Stations"] = list
	s.render(w, "stations.html", data)
}

// handleMessages renders the chat-style messages page: left rail = list of
// conversations grouped by peer, right pane = active thread.
// Active peer is selected via ?with=<callsign>; absent → "no chat selected"
// empty pane on the right.
func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	snap := s.state.Snapshot()
	myCall := snap.Callsign
	convs, _ := s.store.Conversations(myCall)

	peer := strings.TrimSpace(r.URL.Query().Get("with"))
	if peer != "" {
		if v, err := validateCallsign(peer); err == nil {
			peer = v
		} else {
			peer = "" // bad callsign → drop back to empty pane rather than 400
		}
	}
	var thread []store.Message
	if peer != "" {
		thread, _ = s.store.MessagesWithPeer(myCall, peer, 500)
	}

	data := s.common("Messages", r)
	data["MyCall"] = myCall
	data["Conversations"] = convs
	data["Peer"] = peer
	data["Thread"] = thread
	s.render(w, "messages.html", data)
}

// handleMessagesConvList returns just the conversation-list fragment for
// HTMX polling. Lets the left rail of the messages page surface new peers
// (or freshly-active old ones) without a full reload — wired to the same
// 5s trigger that already refreshes the thread pane.
func (s *Server) handleMessagesConvList(w http.ResponseWriter, r *http.Request) {
	snap := s.state.Snapshot()
	convs, _ := s.store.Conversations(snap.Callsign)
	peer := strings.TrimSpace(r.URL.Query().Get("with"))
	if peer != "" {
		if v, err := validateCallsign(peer); err == nil {
			peer = v
		} else {
			peer = ""
		}
	}
	data := s.common("Messages", r)
	data["Conversations"] = convs
	data["Peer"] = peer
	renderFragment(w, r, s.tmpl, "msgConvList", data)
}

// handleMessagesThread returns just the message-thread fragment for HTMX
// polling. Lets the active conversation auto-refresh without reloading the
// entire two-pane layout. Same ?with= contract as handleMessages.
func (s *Server) handleMessagesThread(w http.ResponseWriter, r *http.Request) {
	snap := s.state.Snapshot()
	myCall := snap.Callsign
	peer := strings.TrimSpace(r.URL.Query().Get("with"))
	if peer == "" {
		return
	}
	if v, err := validateCallsign(peer); err == nil {
		peer = v
	} else {
		http.Error(w, "bad peer callsign", http.StatusBadRequest)
		return
	}
	thread, _ := s.store.MessagesWithPeer(myCall, peer, 500)
	data := s.common("Messages", r)
	data["MyCall"] = myCall
	data["Peer"] = peer
	data["Thread"] = thread
	renderFragment(w, r, s.tmpl, "msgThread", data)
}

// handleMessageCancel drops an in-flight outbound message from the retry
// queue and marks it cancelled. POST /messages/cancel/<id>. Idempotent —
// calling on a row that's not pending is a no-op.
func (s *Server) handleMessageCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	if !s.requireCSRF(w, r) {
		return
	}
	idStr := strings.TrimPrefix(r.URL.Path, "/messages/cancel/")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	m, err := s.store.GetMessage(id)
	if err != nil || m.Direction != "out" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if m.State == "pending" {
		s.retries.Remove(id)
		_ = s.store.SetMessageState(id, "cancelled", m.Attempts)
	}
	w.Header().Set("HX-Trigger", "msg-sent") // re-render conv-list + thread
	w.WriteHeader(http.StatusOK)
}

// handleMessageRetry requeues a previously-failed or cancelled outbound
// message: re-TX the body with a fresh msg-id (a new attempt is logically
// a fresh send) and enroll it in the retry queue. The old row is updated
// in place so the conversation history doesn't grow a duplicate. POST
// /messages/retry/<id>.
func (s *Server) handleMessageRetry(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	if !s.requireCSRF(w, r) {
		return
	}
	idStr := strings.TrimPrefix(r.URL.Path, "/messages/retry/")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	m, err := s.store.GetMessage(id)
	if err != nil || m.Direction != "out" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if m.State != "failed" && m.State != "cancelled" {
		http.Error(w, "not in a retryable state", http.StatusConflict)
		return
	}
	// Fresh TX (re-encode and send). Reuse the existing msg-id so the
	// recipient's radio matches it to our existing thread.
	info := []byte(aprs.MessageInfo(m.Dest, m.Body, m.MsgID))
	dstCall := state.DefaultBeaconDest
	if m.ViaRF {
		_ = s.txRF(m.Source, dstCall, []string{"WIDE1-1"}, info)
	}
	if m.ViaIS {
		_ = s.txIS(m.Source, dstCall, info)
	}
	// Reset the row to pending @ attempt 1 and re-enroll in the queue.
	_ = s.store.SetMessageState(id, "pending", 1)
	m.State = "pending"
	m.Attempts = 1
	s.enqueueRetry(m)
	w.Header().Set("HX-Trigger", "msg-sent")
	w.WriteHeader(http.StatusOK)
}

// renderFragment writes a template fragment. Flicker-suppression for the
// 5s poll cycle lives client-side in messages.js (compare incoming body
// to the previous one and skip the swap if identical) — we tried server-
// side ETags first but browser/HTMX caching turned out to be unreliable.
func renderFragment(w http.ResponseWriter, _ *http.Request, tmpl *template.Template, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := tmpl.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) handleMessageSend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	if !s.requireCSRF(w, r) {
		return
	}
	snap := s.state.Snapshot()
	rawSrc := r.FormValue("source")
	if strings.TrimSpace(rawSrc) == "" {
		rawSrc = snap.Callsign
	}
	source, err := validateCallsign(rawSrc)
	if err != nil {
		flash(w, false, "source: "+err.Error())
		return
	}
	dest, err := validateCallsign(r.FormValue("dest"))
	if err != nil {
		flash(w, false, "dest: "+err.Error())
		return
	}
	body := sanitizeAPRSField(r.FormValue("body"))
	if body == "" {
		flash(w, false, "body required")
		return
	}
	viaRF := r.FormValue("via_rf") == "on"
	viaIS := r.FormValue("via_is") == "on"
	// Unique-ish msg id: nanosecond mod 1000 cycles slowly; mix in a random byte.
	var rb [1]byte
	_, _ = rand.Read(rb[:])
	msgID := fmt.Sprintf("%03d", (int(time.Now().UnixNano()/int64(time.Millisecond))+int(rb[0]))%1000)
	if !viaRF && !viaIS {
		viaIS = true
	}

	info := []byte(aprs.MessageInfo(dest, body, msgID))
	dstCall := state.DefaultBeaconDest
	var rfErr, isErr error
	if viaRF {
		rfErr = s.txRF(source, dstCall, []string{"WIDE1-1"}, info)
	}
	if viaIS {
		isErr = s.txIS(source, dstCall, info)
	}

	// Persist with state='pending' so the retry worker can drive this row
	// through retransmits until an ack lands or attempts run out. If both
	// legs failed at the TX stage (no RF link AND IS send error), mark
	// failed immediately — no point queuing a retry of nothing.
	rfOK := viaRF && rfErr == nil
	isOK := viaIS && isErr == nil
	state := "pending"
	if !rfOK && !isOK {
		state = "failed"
	}
	msgRow := store.Message{
		Time: time.Now(), Direction: "out", Source: source, Dest: dest,
		Body: body, MsgID: msgID, ViaRF: rfOK, ViaIS: isOK, Raw: string(info),
		State: state, Attempts: 1,
	}
	id, _ := s.store.LogMessage(msgRow)
	msgRow.ID = id
	if state == "pending" && msgID != "" {
		s.enqueueRetry(msgRow)
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Tell the chat thread to re-fetch itself so the just-sent message
	// shows up immediately, and the conversation list shows the new
	// most-recent activity row. Listeners use hx-trigger="msg-sent from:body".
	w.Header().Set("HX-Trigger", "msg-sent")
	resp := "<div class=\"flash ok\">Message queued"
	if rfErr != nil {
		resp += " — RF: " + html.EscapeString(rfErr.Error())
	}
	if isErr != nil {
		resp += " — IS: " + html.EscapeString(isErr.Error())
	}
	resp += "</div>"
	_, _ = w.Write([]byte(resp))
}

// handleDiagnostics renders the gate-drop ring buffer so operators can see
// why specific packets were dropped (NOGATE in path, bulletin, addressed-
// to-us, recipient-not-heard-on-RF, etc.) without grepping the journal or
// reading source.
func (s *Server) handleDiagnostics(w http.ResponseWriter, r *http.Request) {
	data := s.common("Diagnostics", r)
	data["Drops"] = s.drops.snapshot()
	s.render(w, "diagnostics.html", data)
}

// handleDiagnosticsRows returns just the rows fragment so the page can poll
// every few seconds without rerendering the chrome.
func (s *Server) handleDiagnosticsRows(w http.ResponseWriter, r *http.Request) {
	data := s.common("Diagnostics", r)
	data["Drops"] = s.drops.snapshot()
	renderFragment(w, r, s.tmpl, "diagRows", data)
}

// handleSettings shows the consolidated settings page.
func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	data := s.common("Settings", r)
	snap := s.state.Snapshot()
	// Render each beacon with its path pre-joined for the form.
	type beaconView struct {
		Idx     int
		B       state.Beacon
		PathStr string
	}
	views := make([]beaconView, 0, len(snap.Beacons))
	for i, b := range snap.Beacons {
		views = append(views, beaconView{Idx: i, B: b, PathStr: strings.Join(b.Path, ",")})
	}
	data["BeaconViews"] = views
	data["BeaconLastFired"] = s.beacon.LastFired()
	s.render(w, "settings.html", data)
}

// handleSettingsSave commits the whole settings form atomically.
func (s *Server) handleSettingsSave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	if !s.requireCSRF(w, r) {
		return
	}
	// Per-field validation up front. Accumulate all errors so the user can
	// fix them in one pass instead of whack-a-mole.
	var verrs []string
	if cs := strings.TrimSpace(r.FormValue("callsign")); cs != "" {
		if _, err := validateCallsign(cs); err != nil {
			verrs = append(verrs, "callsign: "+err.Error())
		}
	}
	if err := validatePasscode(r.FormValue("passcode")); err != nil {
		verrs = append(verrs, err.Error())
	}
	// Parse + validate the variable-length beacon list. Each beacon row in the
	// form uses indexed names: beacon_<i>_name, beacon_<i>_info, etc.
	beacons, bErrs := parseBeaconsForm(r)
	verrs = append(verrs, bErrs...)
	if len(verrs) > 0 {
		flash(w, false, strings.Join(verrs, "; "))
		return
	}
	err := s.state.Update(func(st *state.State) error {
		cs, _ := validateCallsign(r.FormValue("callsign"))
		st.Callsign = cs
		st.Passcode = sanitizeAPRSField(r.FormValue("passcode"))
		st.ISServer = sanitizeAPRSField(r.FormValue("is_server"))
		st.ISFilter = sanitizeAPRSField(r.FormValue("is_filter"))
		st.Beacons = beacons
		// Gating + digipeating flags are only writable in Advanced mode.
		// Other modes manage them via applyModeDefaults; ignoring the
		// form fields here prevents a hand-crafted POST from silently
		// flipping flags out of sync with the mode label the operator
		// sees in the UI.
		if st.Mode == state.ModeAdvanced {
			st.TXEnable = r.FormValue("tx_enable") == "1"
			st.GateRFtoIS = r.FormValue("gate_rf_to_is") == "1"
			st.GateIStoRF = r.FormValue("gate_is_to_rf") == "1"
			st.DigipeatWIDE1 = r.FormValue("digipeat_wide1") == "1"
			st.DigipeatWIDE2 = r.FormValue("digipeat_wide2") == "1"
			st.ViscousDelay = r.FormValue("viscous_delay") == "1"
			st.OfflineMode = r.FormValue("offline_mode") == "1"
			st.MessagingOnlyMode = r.FormValue("messaging_only_mode") == "1"
			st.PreemptiveDigipeat = r.FormValue("preemptive_digipeat") == "1"
		}
		if t := r.FormValue("theme"); t == "auto" || t == "light" || t == "dark" {
			st.Theme = t
		}
		if d, err := strconv.Atoi(r.FormValue("retention_days")); err == nil && d >= 0 && d <= 3650 {
			st.RetentionDays = d
		}
		return nil
	})
	if err != nil {
		flash(w, false, err.Error())
		return
	}
	flash(w, true, "Saved.")
}

func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := parts[:0]
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func flash(w http.ResponseWriter, ok bool, msg string) {
	class := "err"
	if ok {
		class = "ok"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<div class="flash %s">%s</div>`, class, html.EscapeString(msg))
}

// handleChangePassword: HTMX endpoint posted from the settings page.
func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	if !s.requireCSRF(w, r) {
		return
	}
	current := r.FormValue("current")
	newPass := r.FormValue("new")
	confirm := r.FormValue("confirm")
	if !s.state.CheckPassword(current) {
		flash(w, false, "Current password incorrect.")
		return
	}
	if newPass != confirm {
		flash(w, false, "New password and confirmation do not match.")
		return
	}
	if err := s.state.SetPassword(newPass); err != nil {
		flash(w, false, err.Error())
		return
	}
	flash(w, true, "Password updated.")
}
