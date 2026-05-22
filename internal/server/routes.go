package server

import (
	"encoding/json"
	"fmt"
	"html"
	"html/template"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"aprgo/internal/aprs"
	"aprgo/internal/ax25"
	"aprgo/internal/config"
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
	a.HandleFunc("/bulletins", s.handleBulletins)
	a.HandleFunc("/bulletins/save", s.handleBulletinsSave)
	a.HandleFunc("/bulletins/send", s.handleBulletinSend)
	a.HandleFunc("/messages/send", s.handleMessageSend)
	a.HandleFunc("/messages/thread", s.handleMessagesThread)
	a.HandleFunc("/messages/conv-list", s.handleMessagesConvList)
	a.HandleFunc("/messages/cancel/", s.handleMessageCancel)
	a.HandleFunc("/messages/retry/", s.handleMessageRetry)
	a.HandleFunc("/settings", s.handleSettings)
	a.HandleFunc("/settings/save", s.handleSettingsSave)
	a.HandleFunc("/settings/account", s.handleAccount)
	a.HandleFunc("/logs", s.handleDiagnostics)
	a.HandleFunc("/logs/rows", s.handleDiagnosticsRows)
	a.HandleFunc("/logs/tail", s.handleDiagnosticsLog)
	a.HandleFunc("/stats", s.handleStats)
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
	// behind login (default password applies on first run).
	mux.Handle("/", s.auth.RequireLogin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.state.Snapshot().SetupComplete && !strings.HasPrefix(r.URL.Path, "/setup") && r.URL.Path != "/logout" {
			http.Redirect(w, r, "/setup", http.StatusFound)
			return
		}
		a.ServeHTTP(w, r)
	})))
	// Wrap everything with the transport gate. Non-loopback HTTP gets
	// redirected to the HTTPS port; HTTPS and loopback pass through.
	// /healthz + /readyz remain reachable over plain HTTP so a monitor
	// (or systemd watchdog) on the LAN doesn't have to bother with TLS.
	return s.transportGate(mux)
}

// transportGate scopes HTTPS-vs-HTTP per path.
//
//   - HTTPS from anywhere → pass.
//   - Loopback (HTTP or HTTPS) → pass; the operator on the box itself has
//     shell access already, no point inconveniencing them.
//   - HTTP from non-loopback, on a non-critical path → pass. Read-only
//     pages (Dashboard, Map, Stations, Stats, Diagnostics, login,
//     /static, /healthz, /readyz) work over plain HTTP.
//   - HTTP from non-loopback, on a critical path → 308-redirect to HTTPS.
//     Critical paths are the ones that mutate state or expose private
//     content: /settings, /messages, /bulletins, /setup.
//
// First-run carve-out: until state.SetupComplete=true the gate stands
// down entirely so a fresh operator can complete onboarding without
// first wrestling with a self-signed cert warning. The default
// admin/admin credential is the only secret in play during that window
// and it isn't actually a secret.
//
// 307 (temporary + preserves method) is used so a form POST that lands
// on HTTP by mistake survives the bounce — browsers re-issue the same
// method and body against the new URL. We deliberately do NOT use 308
// (permanent) because permanent redirects get cached aggressively by
// browsers; if a path's classification ever changes (critical → not, or
// vice versa) clients with a cached 308 would skip the server roundtrip
// and behave incorrectly until cache eviction. Cache-Control: no-store
// belt-and-suspenders.
func (s *Server) transportGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS != nil || isLoopback(r) {
			next.ServeHTTP(w, r)
			return
		}
		if !s.state.Snapshot().SetupComplete {
			next.ServeHTTP(w, r)
			return
		}
		if !isCriticalPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		target := s.httpsRedirectURL(r)
		w.Header().Set("Location", target)
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusTemporaryRedirect)
	})
}

// isCriticalPath reports whether the URL path mutates state or exposes
// content the operator opted to require HTTPS for (settings forms,
// private messages, bulletin compose, the wizard). Anything else
// (read-only views, public health probes, login flow, static assets)
// is reachable over plain HTTP from the LAN.
func isCriticalPath(p string) bool {
	return strings.HasPrefix(p, "/settings") ||
		strings.HasPrefix(p, "/messages") ||
		strings.HasPrefix(p, "/bulletins") ||
		strings.HasPrefix(p, "/setup")
}

// isLoopback reports whether the request originated from 127.0.0.0/8 or ::1.
// Trusts r.RemoteAddr — aprgo does NOT honor X-Forwarded-For here because
// the transport gate is a *transport* check, not an identity check, and
// upstream proxies are responsible for re-terminating TLS themselves.
func isLoopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}

// httpsRedirectURL rewrites the inbound HTTP URL to the HTTPS equivalent on
// the configured TLS port. Falls back to the request's Host (minus its
// existing port, if any) when the configured ListenHTTPS uses :PORT form.
func (s *Server) httpsRedirectURL(r *http.Request) string {
	host := r.Host
	if i := strings.LastIndex(host, ":"); i >= 0 {
		// Strip a port off the Host header. IPv6 literals are bracketed
		// (`[::1]:14473`) so LastIndex(":") lands on the port colon.
		host = host[:i]
	}
	port := s.opts.ListenHTTPS
	if strings.HasPrefix(port, ":") {
		port = port[1:]
	} else if i := strings.LastIndex(port, ":"); i >= 0 {
		port = port[i+1:]
	}
	if port == "" {
		port = "443"
	}
	target := "https://" + host + ":" + port + r.URL.RequestURI()
	return target
}

// requireUnlocked returns true (writes 403 + false) if the given lockdown
// flag is set. Use at the top of mutating handlers after CSRF.
func (s *Server) requireUnlocked(w http.ResponseWriter, flag func(config.Lockdown) bool, what string) bool {
	if flag(s.config.LockdownEffective()) {
		http.Error(w, "locked by config: "+what+" is disabled. Edit aprgo.conf and restart aprgo to undo.", http.StatusForbidden)
		return false
	}
	return true
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
		s.auth.IssueCookie(w, r, user)
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
	snap := s.state.Snapshot()
	data["St"] = snap
	data["RenderedPackets"] = rendered
	data["FeedCursor"] = cursor
	data["StatusCards"] = s.dashboardStatusCards(snap)
	if r.URL.Query().Get("saved") == "1" {
		data["Flash"] = "Settings saved."
	}
	s.render(w, "dashboard.html", data)
}

// statusCard is one tile on the dashboard's mode-aware status strip.
// Tone drives the colored accent on the left edge: "ok" = green, "warn"
// = amber, "err" = red, "" = neutral.
type statusCard struct {
	Label string // small uppercase eyebrow
	Value string // big main value (or "● connected" / "● verified" for connection cards)
	Sub   string // dim secondary line below
	Tone  string // "ok" / "warn" / "err" / "" — drives the left-edge accent color
}

// dashboardStatusCards picks the set of status cards to show on the
// dashboard based on the operator's current mode. Each mode emphasizes
// the stats that matter for its role — a digi cares about digipeat
// counts but not IS-gated msg counts, a messaging-only iGate is the
// opposite, etc. Cards order matters; first card sits leftmost.
func (s *Server) dashboardStatusCards(snap state.State) []statusCard {
	cards := []statusCard{}

	// Connection cards are common to most modes (only Offline-digi skips IS).
	rfConn := s.rf.Connected()
	rfCard := statusCard{Label: "RF", Tone: "err", Value: "● disconnected"}
	if snap.TNCKind == state.TNCNone {
		rfCard = statusCard{Label: "RF", Tone: "", Value: "○ no TNC", Sub: "configure in Settings"}
	} else if rfConn {
		rfCard = statusCard{Label: "RF", Tone: "ok", Value: "● connected", Sub: s.rf.IFace()}
	} else {
		rfCard.Sub = "see Settings"
	}

	var isCard statusCard
	if !snap.OfflineMode {
		isConn := s.is.Connected()
		switch {
		case !isConn:
			// Distinguish "we just started and haven't reached the server
			// yet" (no error history → still connecting) from "we tried
			// and failed, currently in backoff" (last error recorded →
			// disconnected). Same convention used by settings + stats.
			if lastErr, _ := s.is.LastError(); lastErr == "" {
				isCard = statusCard{Label: "APRS-IS", Tone: "warn", Value: "● connecting", Sub: snap.Callsign}
			} else {
				isCard = statusCard{Label: "APRS-IS", Tone: "err", Value: "● disconnected"}
			}
		case s.is.Verification() == igate.VerificationUnverified:
			isCard = statusCard{Label: "APRS-IS", Tone: "warn", Value: "● unverified", Sub: snap.Callsign}
		case s.is.Verification() == igate.VerificationVerified:
			isCard = statusCard{Label: "APRS-IS", Tone: "ok", Value: "● verified", Sub: snap.Callsign}
		default:
			isCard = statusCard{Label: "APRS-IS", Tone: "warn", Value: "● connecting", Sub: snap.Callsign}
		}
	}

	// Stats lifted from the in-memory counters. Always read both atomic
	// loads to a local before subtraction; #46 fix is in place but we
	// follow the same pattern in case future code touches these.
	rfToIS := s.stats.sentIS.Load()
	isToRF := s.stats.igateMsgsRF.Load()
	digi := s.stats.digipeats.Load()
	beacons := s.stats.beacons.Load()
	heardRF := s.stats.pktsRF.Load()

	// All counts below are SESSION-lifetime (since process start). The
	// dashboard is the at-a-glance live view; historical / daily stats
	// live on /stats. Sub-line annotates this so the user isn't confused
	// about scope.
	gatedCard := statusCard{
		Label: "GATED",
		Value: fmt.Sprintf("%d ↑  %d ↓", rfToIS, isToRF),
		Sub:   "RF→IS · IS→RF",
	}
	digiCard := statusCard{
		Label: "DIGI",
		Value: fmt.Sprintf("%d", digi),
		Sub:   "session",
	}
	beaconCard := statusCard{
		Label: "BEACONS",
		Value: fmt.Sprintf("%d", beacons),
		Sub:   "session",
	}
	heardCard := statusCard{
		Label: "RF RX",
		Value: fmt.Sprintf("%d", heardRF),
		Sub:   "session",
	}

	// Per-mode layout. Each branch picks 3-5 cards in priority order.
	// Modes that shouldn't surface a stat just leave it out — the user
	// can dig into /stats for the full picture.
	switch snap.Mode {
	case state.ModeRXOnly:
		// Listen + upload — no TX, no digi, no IS→RF.
		cards = append(cards, rfCard)
		if !snap.OfflineMode {
			cards = append(cards, isCard)
		}
		cards = append(cards, statusCard{Label: "UPLOADED", Value: fmt.Sprintf("%d", rfToIS), Sub: "session"})
		cards = append(cards, heardCard)

	case state.ModeDigi, state.ModeOffline:
		// Digi-first; IS card optional based on OfflineMode.
		cards = append(cards, rfCard)
		if !snap.OfflineMode {
			cards = append(cards, isCard)
		}
		cards = append(cards, digiCard)
		cards = append(cards, beaconCard)
		cards = append(cards, heardCard)

	case state.ModeMessaging:
		// Selective-gate iGate: focus is gating counts.
		cards = append(cards, rfCard)
		if !snap.OfflineMode {
			cards = append(cards, isCard)
		}
		cards = append(cards, gatedCard)
		cards = append(cards, heardCard)

	case state.ModeFillinIG, state.ModeTXIGate:
		// Full iGate role — show RF/IS health + both directions of gating.
		cards = append(cards, rfCard)
		if !snap.OfflineMode {
			cards = append(cards, isCard)
		}
		cards = append(cards, gatedCard)
		if snap.Mode == state.ModeFillinIG {
			cards = append(cards, digiCard)
		}
		cards = append(cards, heardCard)

	default:
		// Advanced or unset — show the works.
		cards = append(cards, rfCard)
		if !snap.OfflineMode {
			cards = append(cards, isCard)
		}
		cards = append(cards, gatedCard)
		cards = append(cards, digiCard)
		cards = append(cards, beaconCard)
		cards = append(cards, heardCard)
	}
	return cards
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
	return s.renderPacketHTMLWithConfig(pkt, nil)
}

// renderPacketHTMLWithConfig is the variant that applies a station's
// telemetry config (PARM names + UNIT labels + EQNS coefficients) when
// rendering T# packets — used by the station-detail page where we've
// reconstructed the per-station config. The dashboard live-feed passes
// nil and renders raw analog values.
// feedSymbolHTML renders the APRS symbol as a small inline sprite for
// the live-feed callsign cell. Returns empty when sym isn't a valid
// 2-char symbol or contains non-printable chars. Mirrors the aprsIcon
// template func at server.go but emits 16px-scaled sprite positions so
// the row keeps its line height tight beside text.
func feedSymbolHTML(sym string) string {
	if len(sym) < 2 {
		return ""
	}
	t, c := sym[0], sym[1]
	if c < 0x21 || c > 0x7e {
		return ""
	}
	sprite := "1"
	if t == '/' {
		sprite = "0"
	}
	// Sprite sheet is laid out 16 columns × 6 rows at the *native* 24px
	// cell size. Our CSS scales background-size to 256×96 (16px cells),
	// so the position offsets are computed at 16px stride to match.
	const cell = 16
	idx := int(c) - 0x21
	x := -((idx % 16) * cell)
	y := -((idx / 16) * cell)
	out := fmt.Sprintf(
		`<span class="feed-sym" title="%s" style="background-image:url(/static/aprs-symbols-48-%s.png);background-position:%dpx %dpx"></span>`,
		html.EscapeString(sym), sprite, x, y,
	)
	if t != '/' && t != '\\' {
		oc := int(t) - 0x21
		if oc >= 0 && oc < 16*6 {
			ox := -((oc % 16) * cell)
			oy := -((oc / 16) * cell)
			out = fmt.Sprintf(
				`<span class="feed-sym-wrap">%s<span class="feed-sym-overlay" style="background-image:url(/static/aprs-symbols-48-2.png);background-position:%dpx %dpx"></span></span>`,
				out, ox, oy,
			)
		}
	}
	return out
}

func (s *Server) renderPacketHTMLWithConfig(pkt aprs.Packet, tc *aprs.TelemConfig) string {
	snap := s.state.Snapshot()
	ts := pkt.Frame.RxAt.In(resolveTZ(snap.Timezone)).Format(clockLayout(snap.TimeFormat))
	dirLabel := pkt.Frame.Origin.String()
	// Distinct CSS class per origin so dashboard styling can color them
	// differently: RF/IS in green-ish, TX in amber/orange so own
	// transmissions stand out from received traffic.
	dirClass := strings.ToLower(dirLabel)
	if dirClass == "" {
		dirClass = "rx"
	}
	pathPart := renderPathHTML(strings.Join(pkt.Frame.Path, ","))
	parsedInfo := parsedInfoHTMLWithConfig(pkt, tc)
	rawInfo := html.EscapeString(string(pkt.Frame.Info))
	// Source callsign + APRS symbol icon (when one was decoded from the
	// packet). Putting the icon to the left of the link gives the live
	// feed the same visual scanability as the map — at a glance you can
	// tell a car beacon from a weather station from a digi.
	srcLink := fmt.Sprintf(`%s<a class="src" href="/stations/%s">%s</a>`,
		feedSymbolHTML(pkt.Decoded.Symbol),
		html.EscapeString(pkt.Frame.Src), html.EscapeString(pkt.Frame.Src))
	// Device chip — derived from the AX.25 dest tocall via the embedded
	// aprs-deviceid registry. Empty when we don't have a match.
	devChip := ""
	if dev := aprs.LookupDevice(pkt.Frame.Dest); dev.Model != "" || dev.Vendor != "" {
		devChip = fmt.Sprintf(` <span class="dev-chip" title="%s">%s</span>`,
			html.EscapeString(dev.Tocall), html.EscapeString(dev.Display()))
	}
	// `parsed-line` is the styled row body (chips, pills, parsed info);
	// `raw-line` is the literal TNC2 form. CSS toggles between them based
	// on the `view-raw` body class so the operator sees a clean pure-text
	// view in raw mode rather than the styled chrome with raw bytes
	// substituted into one field.
	rawTNC2 := html.EscapeString(string(pkt.Frame.Src))
	rawTNC2 += "&gt;" + html.EscapeString(pkt.Frame.Dest)
	if path := strings.Join(pkt.Frame.Path, ","); path != "" {
		rawTNC2 += "," + html.EscapeString(path)
	}
	rawTNC2 += ":" + rawInfo
	return fmt.Sprintf(
		`<div class="pkt %s"><span class="t">%s</span> <span class="dir %s">%s</span> <span class="parsed-line">%s&gt;<span class="dst">%s</span>%s%s <span class="info-parsed">%s</span></span><span class="raw-line">%s</span></div>`,
		dirClass, ts, dirClass, dirLabel,
		srcLink, html.EscapeString(pkt.Frame.Dest),
		devChip, pathPart, parsedInfo, rawTNC2,
	)
}

// renderPathHTML beautifies a comma-joined path into a hop chain: used hops
// (digi*) get an "is-used" class for accent-color rendering; q-construct
// hops (qAR + iGate call) get a separate "is-q" class. Empty input returns
// an empty fragment.
func renderPathHTML(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return ""
	}
	p := aprs.ParsePath(raw)
	if len(p.Hops) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(` <span class="path">via `)
	for i, h := range p.Hops {
		if i > 0 {
			b.WriteString(",")
		}
		cls := "hop"
		switch {
		case h.IsQConstruct:
			cls = "hop is-q"
		case h.Used:
			cls = "hop is-used"
		}
		marker := ""
		if h.Used && !h.IsQConstruct {
			marker = "*"
		}
		fmt.Fprintf(&b, `<span class="%s">%s%s</span>`,
			cls, html.EscapeString(h.Call), marker)
	}
	b.WriteString(`</span>`)
	return b.String()
}

func parsedInfoHTML(pkt aprs.Packet) string {
	return parsedInfoHTMLWithConfig(pkt, nil)
}

func parsedInfoHTMLWithConfig(pkt aprs.Packet, tc *aprs.TelemConfig) string {
	d := pkt.Decoded
	var b strings.Builder
	wrote := false
	// Object/Item name first — for `;` and `)` packets it's the most useful
	// thing (typically a repeater identifier or WX station name) so put it
	// in front of the position.
	if d.ObjectName != "" {
		label := d.ObjectName
		if d.ObjectKilled {
			label = label + " (killed)"
		}
		fmt.Fprintf(&b, `<span class="obj-name">⊙ %s</span>`, html.EscapeString(label))
		wrote = true
	}
	// Telemetry-config messages: short chip describing the kind so operators
	// can see what arrived without scanning the raw line. Detail rendering
	// (channel labels applied to T# data) lives on the station-detail page.
	if d.TelemConfig != nil {
		fmt.Fprintf(&b, `<span class="telem-cfg">⚙ Telemetry %s → %s</span>`,
			html.EscapeString(strings.ToUpper(d.TelemConfig.Kind)),
			html.EscapeString(d.MsgTo))
		wrote = true
	}
	if d.Lat != nil && d.Lon != nil {
		fmt.Fprintf(&b, ` <span class="pos">📍 %.4f, %.4f</span>`, *d.Lat, *d.Lon)
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
	if d.FreqTone != "" {
		// CTCSS tone (Hz) or DCS code (D-prefixed). Distinct chip style.
		fmt.Fprintf(&b, ` <span class="freq-extra">T %s</span>`, html.EscapeString(d.FreqTone))
		wrote = true
	}
	if d.FreqOffset != "" {
		fmt.Fprintf(&b, ` <span class="freq-extra">Δ %s</span>`, html.EscapeString(d.FreqOffset))
		wrote = true
	}
	if d.Status != "" {
		fmt.Fprintf(&b, ` <span class="status">[%s]</span>`, html.EscapeString(d.Status))
		wrote = true
	}
	if d.Weather != nil {
		fmt.Fprintf(&b, ` <span class="wx">%s</span>`, weatherInlineHTML(d.Weather))
		wrote = true
	}
	if d.PHG != nil {
		fmt.Fprintf(&b, ` <span class="phg">%s</span>`, html.EscapeString(phgInline(d.PHG)))
		wrote = true
	}
	if d.RNG != nil {
		fmt.Fprintf(&b, ` <span class="phg">~%d mi range</span>`, d.RNG.Miles)
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
	if d.IsRej {
		if d.MsgTo != "" {
			return fmt.Sprintf(`<span class="rej">✗ REJ %s</span> <span class="dim">→ %s</span>`,
				html.EscapeString(d.AckedID), html.EscapeString(d.MsgTo))
		}
		return fmt.Sprintf(`<span class="rej">✗ REJ %s</span>`, html.EscapeString(d.AckedID))
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
		// If we have a station's telemetry config, render labeled values:
		//   "Battery 13.4 V · Temp 67.5 °F · …"
		// Otherwise fall back to bare numeric "v1 · v2 · …" rendering.
		if tc != nil {
			vals := tc.Apply(d.TelemAnalog)
			var labeled strings.Builder
			for i := 0; i < 5; i++ {
				if i > 0 {
					labeled.WriteString(" · ")
				}
				name := tc.ParamNames[i]
				unit := tc.UnitNames[i]
				if name == "" {
					fmt.Fprintf(&labeled, "%g", vals[i])
					continue
				}
				labeled.WriteString(html.EscapeString(name))
				labeled.WriteString(" ")
				fmt.Fprintf(&labeled, "%g", vals[i])
				if unit != "" {
					labeled.WriteString(" ")
					labeled.WriteString(html.EscapeString(unit))
				}
			}
			return fmt.Sprintf(
				`<span class="telem">📊 T#%d</span> <span class="telem-vals">%s · <code>%s</code></span>`,
				d.TelemSeq, labeled.String(), bits.String(),
			)
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

// weatherText renders a compact one-line weather summary in plain text.
func weatherText(w *aprs.Weather) string {
	var parts []string
	if w.TempSet {
		parts = append(parts, fmt.Sprintf("%d°F", w.TempF))
	}
	if w.WindDirSet && w.WindSpeedSet {
		wind := fmt.Sprintf("%d mph %d°", w.WindSpeedMPH, w.WindDirDeg)
		if w.WindGustSet && w.WindGustMPH > w.WindSpeedMPH {
			wind += fmt.Sprintf(" (g%d)", w.WindGustMPH)
		}
		parts = append(parts, wind)
	}
	if w.HumiditySet {
		parts = append(parts, fmt.Sprintf("%d%% RH", w.HumidityPct))
	}
	if w.PressureSet {
		parts = append(parts, fmt.Sprintf("%.1f mb", float64(w.PressureTenthMb)/10.0))
	}
	if w.Rain1hSet && w.Rain1hHundIn > 0 {
		parts = append(parts, fmt.Sprintf("%.2f\"/h", float64(w.Rain1hHundIn)/100.0))
	}
	return strings.Join(parts, " · ")
}

// weatherInlineHTML is weatherText with HTML escaping for the live feed.
func weatherInlineHTML(w *aprs.Weather) string {
	return html.EscapeString(weatherText(w))
}

// phgInline renders a single-line PHG summary suitable for the live feed.
func phgInline(p *aprs.PHG) string {
	dir := "omni"
	if !p.Omni {
		dir = fmt.Sprintf("%d°", p.DirDeg)
	}
	return fmt.Sprintf("%dW · %dft · %ddB %s · ~%.0f mi",
		p.PowerW, p.HeightFt, p.GainDB, dir, p.RangeMiles)
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
	defaultTR, opts := resolveWindow("1h", "1h", snap.RetentionDays)
	data["WindowOptions"] = opts
	data["DefaultWindow"] = defaultTR.Value
	s.render(w, "map.html", data)
}

// parseISFilterRadius extracts (lat, lon, km) from an APRS-IS server-side
// filter that begins with "r/<lat>/<lon>/<km>" (possibly followed by
// additional space-separated filter elements like "-t/pwntso"). Returns
// ok=false for any other filter shape or malformed inputs.
func parseISFilterRadius(s string) (lat, lon float64, km int, ok bool) {
	if !strings.HasPrefix(s, "r/") {
		return 0, 0, 0, false
	}
	// Take only the leading r/.../.../... segment; anything after a space
	// is a separate filter element (e.g. "-t/pwntso") that doesn't carry
	// radius information.
	if sp := strings.IndexByte(s, ' '); sp >= 0 {
		s = s[:sp]
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
	retention := s.state.Snapshot().RetentionDays
	tr, _ := resolveWindow(r.URL.Query().Get("window"), "1h", retention)
	since := time.Now().Add(-tr.Dur)
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
		// Lat=0 AND lon=0 (Null Island): classic cold-boot GPS — fix not
		// yet acquired when packet generated. Match aprs.fi convention.
		// Use AND not OR so we keep legitimate equator (lat=0) and prime-
		// meridian (lon=0) positions.
		if p.Lat == 0 && p.Lon == 0 {
			continue
		}
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
			// Equirectangular distance with cos(lat) on the longitude term.
			// At lat L, one degree of longitude is 111·cos(L) km, not 111.
			// Manhattan sum would over-estimate by up to √2 on diagonal
			// travel; Euclidean is the honest answer.
			cosLat := math.Cos(p.Lat * math.Pi / 180)
			dLatKm := dLat * degToKm
			dLonKm := dLon * degToKm * cosLat
			distKm := math.Sqrt(dLatKm*dLatKm + dLonKm*dLonKm)
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
	retention := s.state.Snapshot().RetentionDays
	tr, _ := resolveWindow(r.URL.Query().Get("window"), "1h", retention)
	cutoff := time.Now().Add(-tr.Dur)
	list, err := s.store.HeardSince(cutoff, "")
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
		// Parsed extras for the map popup. All omitempty so old packets
		// that lack these fields don't bloat the JSON.
		DeviceName  string  `json:"device,omitempty"`
		DeviceTocall string `json:"device_tocall,omitempty"`
		WxSummary   string  `json:"wx,omitempty"`
		PHGSummary  string  `json:"phg,omitempty"`
		PHGRange    int     `json:"phg_range_mi,omitempty"`
		RNGRange    int     `json:"rng_mi,omitempty"`
		HopSummary  string  `json:"hops,omitempty"`
		QConstruct  string  `json:"q,omitempty"`
		IGate       string  `json:"igate,omitempty"`
		// Object/Item — only set when the latest packet was a `;` or `)`
		// report. ObjectName is the 9-char name (trimmed); ObjectKilled
		// indicates the object was marked dead.
		ObjectName    string `json:"object_name,omitempty"`
		ObjectKilled  bool   `json:"object_killed,omitempty"`
		// Ambiguity is 0-4 per APRS spec §6 (uncompressed positions).
		// >0 means the operator deliberately blanked trailing decimals;
		// the marker should not be trusted to street-level precision.
		Ambiguity int `json:"ambiguity,omitempty"`
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
		if dev := aprs.LookupDevice(st.LastDest); dev.Model != "" || dev.Vendor != "" {
			o.DeviceName = dev.Display()
			o.DeviceTocall = dev.Tocall
		}
		if dec.Weather != nil {
			o.WxSummary = weatherText(dec.Weather)
		}
		if dec.PHG != nil {
			o.PHGSummary = phgInline(dec.PHG)
			o.PHGRange = int(dec.PHG.RangeMiles + 0.5)
		}
		if dec.RNG != nil {
			o.RNGRange = dec.RNG.Miles
		}
		if st.LastPath != "" {
			ps := aprs.ParsePath(st.LastPath)
			o.HopSummary = ps.HopSummary()
			o.QConstruct = ps.QConstruct
			o.IGate = ps.IGateCall
		}
		o.ObjectName = dec.ObjectName
		o.ObjectKilled = dec.ObjectKilled
		o.Ambiguity = dec.Ambiguity
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

	stations, _ := s.store.HeardSince(time.Now().Add(-30*24*time.Hour), "")
	var st *store.Station
	for i := range stations {
		if strings.EqualFold(stations[i].Callsign, call) {
			st = &stations[i]
			break
		}
	}
	packets, _ := s.store.PacketsBySource(call, 500)
	// Build the merged telemetry config first (newest-config-of-each-kind)
	// so we can apply it when rendering T# data packets below.
	var tcMerged *aprs.TelemConfig
	for _, p := range packets {
		d := aprs.Decode(p.Info, p.Dest)
		if d.TelemConfig == nil {
			continue
		}
		if tcMerged == nil {
			tcMerged = &aprs.TelemConfig{}
		}
		switch d.TelemConfig.Kind {
		case "parm":
			if tcMerged.ParamNames[0] == "" {
				tcMerged.ParamNames = d.TelemConfig.ParamNames
			}
		case "unit":
			if tcMerged.UnitNames[0] == "" {
				tcMerged.UnitNames = d.TelemConfig.UnitNames
			}
		case "eqns":
			if tcMerged.Coeffs[0][0] == 0 && tcMerged.Coeffs[0][1] == 0 && tcMerged.Coeffs[0][2] == 0 {
				tcMerged.Coeffs = d.TelemConfig.Coeffs
			}
		case "bits":
			if tcMerged.Title == "" {
				tcMerged.Sense = d.TelemConfig.Sense
				tcMerged.Title = d.TelemConfig.Title
			}
		}
	}
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
		rendered = append(rendered, template.HTML(s.renderPacketHTMLWithConfig(pkt, tcMerged)))
	}
	data["Station"] = st
	data["RenderedPackets"] = rendered
	data["PacketCount"] = len(packets)
	// Decode the most recently heard packet's info field to surface rich
	// parsed fields (weather, PHG, RNG) on the station detail page.
	// Device ID comes from the AX.25 dest tocall; lookup is cheap.
	if st != nil {
		if st.LastDest != "" {
			if dev := aprs.LookupDevice(st.LastDest); dev.Model != "" || dev.Vendor != "" {
				data["Device"] = dev
			}
		}
		if st.LastInfo != "" {
			dec := aprs.Decode(st.LastInfo, st.LastDest)
			if dec.Weather != nil {
				data["Weather"] = dec.Weather
			}
			if dec.PHG != nil {
				data["PHG"] = dec.PHG
			}
			if dec.RNG != nil {
				data["RNG"] = dec.RNG
			}
			if dec.ObjectName != "" {
				data["ObjectName"] = dec.ObjectName
				data["ObjectKilled"] = dec.ObjectKilled
			}
			if dec.Ambiguity > 0 {
				// Map level to a human-readable uncertainty radius per
				// APRS spec §6. Operators see "approximate" instead of
				// a dot that lies about its precision.
				labels := []string{"", "≈ ±185 m", "≈ ±1.8 km", "≈ ±18 km", "≈ ±111 km"}
				if dec.Ambiguity < len(labels) {
					data["AmbiguityLabel"] = labels[dec.Ambiguity]
				}
			}
		}
		if tcMerged != nil {
			data["TelemConfig"] = tcMerged
		}
		if st.LastPath != "" {
			data["PathSummary"] = aprs.ParsePath(st.LastPath)
		}
	}
	s.render(w, "station_detail.html", data)
}

func (s *Server) handleStations(w http.ResponseWriter, r *http.Request) {
	snap := s.state.Snapshot()
	tr, opts := resolveWindow(r.URL.Query().Get("window"), "24h", snap.RetentionDays)
	dur := tr.Dur
	matched := tr.Value
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if len(query) > 64 {
		query = query[:64]
	}

	cutoff := time.Now().Add(-dur)
	list, _ := s.store.HeardSince(cutoff, query)
	blockedMap := make(map[string]bool)
	for _, b := range s.srcLimiter.BlockedSources() {
		blockedMap[b.Source] = true
	}
	var msgMatches []store.Message
	if query != "" {
		msgMatches, _ = s.store.SearchMessages(query, 50)
	}
	data := s.common("Stations", r)
	data["Stations"] = list
	data["BlockedSrcs"] = blockedMap
	data["Window"] = matched
	data["WindowOptions"] = opts
	data["Query"] = query
	data["MessageMatches"] = msgMatches
	s.render(w, "stations.html", data)
}

// handleBulletins renders the bulletins page — broadcast-form message
// frames (BLN*, NWS*, SKY*, CWA-*) grouped by kind and sorted per spec.
// Read-only display; the form below the list saves the operator's
// group whitelist + NWS subscription flag.
func (s *Server) handleBulletins(w http.ResponseWriter, r *http.Request) {
	snap := s.state.Snapshot()
	// Look back 24 h — announcements (BLNA-Z) live longer than numbered
	// per Bruninga, but 24h covers the spec retention envelope of both.
	cutoff := time.Now().Add(-24 * time.Hour)
	rows, err := s.store.LatestBulletins(cutoff, 200)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	view := buildBulletinsView(rows, snap, cutoff)
	data := s.common("Bulletins", r)
	data["St"] = snap
	data["View"] = view
	if r.URL.Query().Get("saved") == "1" {
		data["Flash"] = "Subscription saved."
	}
	if r.URL.Query().Get("sent") == "1" {
		data["Flash"] = "Bulletin transmitted."
	}
	if e := r.URL.Query().Get("err"); e != "" {
		data["FlashErr"] = e
	}
	s.render(w, "bulletins.html", data)
}

// handleBulletinsSave persists the operator's bulletin-group whitelist
// and NWS-subscription flag. POST-only. Empty group list (or all
// whitespace) clears the whitelist back to "receive all" semantics.
func (s *Server) handleBulletinsSave(w http.ResponseWriter, r *http.Request) {
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
	if !s.requireUnlocked(w, func(l config.Lockdown) bool { return l.DisableBulletins }, "bulletins") {
		return
	}
	raw := r.FormValue("groups")
	nws := r.FormValue("nws_subscribed") == "1"
	var groups []string
	for _, g := range strings.Split(raw, ",") {
		g = strings.TrimSpace(strings.ToUpper(g))
		if g != "" {
			groups = append(groups, g)
		}
	}
	_ = s.state.Update(func(st *state.State) error {
		st.MessageGroups = groups
		st.NWSSubscribed = nws
		return nil
	})
	http.Redirect(w, r, "/bulletins?saved=1", http.StatusSeeOther)
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
		flash(w, false, "bad peer callsign")
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
		flash(w, false, "POST required")
		return
	}
	if err := r.ParseForm(); err != nil {
		flash(w, false, "bad form")
		return
	}
	if !s.requireCSRF(w, r) {
		return
	}
	if !s.requireUnlocked(w, func(l config.Lockdown) bool { return l.DisableMessaging }, "messaging") {
		return
	}
	idStr := strings.TrimPrefix(r.URL.Path, "/messages/cancel/")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		flash(w, false, "bad id")
		return
	}
	m, err := s.store.GetMessage(id)
	if err != nil || m.Direction != "out" {
		flash(w, false, "not found")
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
		flash(w, false, "POST required")
		return
	}
	if err := r.ParseForm(); err != nil {
		flash(w, false, "bad form")
		return
	}
	if !s.requireCSRF(w, r) {
		return
	}
	if !s.requireUnlocked(w, func(l config.Lockdown) bool { return l.DisableMessaging }, "messaging") {
		return
	}
	idStr := strings.TrimPrefix(r.URL.Path, "/messages/retry/")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		flash(w, false, "bad id")
		return
	}
	m, err := s.store.GetMessage(id)
	if err != nil || m.Direction != "out" {
		flash(w, false, "not found")
		return
	}
	if m.State != "failed" && m.State != "cancelled" {
		flash(w, false, "not in a retryable state")
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
		flash(w, false, "POST required")
		return
	}
	if err := r.ParseForm(); err != nil {
		flash(w, false, "bad form")
		return
	}
	if !s.requireCSRF(w, r) {
		return
	}
	if !s.requireUnlocked(w, func(l config.Lockdown) bool { return l.DisableMessaging }, "messaging") {
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
	// APRS101 §14: strip chars reserved by the protocol — `{` (msg-id
	// delimiter, would confuse receiver's parser), `|` and `~` (reserved
	// for telemetry / future use). Quietly remove rather than reject so
	// the user's intent ("hi :{ " becomes "hi :") is preserved.
	body = strings.Map(func(r rune) rune {
		switch r {
		case '{', '|', '~':
			return -1
		}
		return r
	}, body)
	// APRS101 §14: message body capped at 67 chars. UI textarea has
	// maxlength=67, but enforce server-side too — a hand-crafted POST or
	// a future non-browser client could exceed it, and some IS hubs drop
	// overlong messages silently.
	if len(body) > 67 {
		body = body[:67]
	}
	if body == "" {
		flash(w, false, "body required")
		return
	}
	viaRF := r.FormValue("via_rf") == "on"
	viaIS := r.FormValue("via_is") == "on"
	// Monotonic msg-id counter wrapping 1..99999, persisted across restarts.
	// Matches APRSdroid / YAAC / UI-View32 convention — no collisions within
	// a 99,999-msg window, naturally orders in logs, easy to recall when
	// referring to a recent send.
	msgID := fmt.Sprintf("%d", s.state.NextOutboundMsgID())
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
	data := s.common("Logs", r)
	data["Drops"] = s.drops.snapshot()
	data["Blocked"] = s.srcLimiter.BlockedSources()
	data["LogLines"] = s.recentLogLines(20)
	s.render(w, "diagnostics.html", data)
}

// recentLogLines returns the most recent n log lines newest-first.
// Empty slice if logBuf isn't wired (tests, etc).
func (s *Server) recentLogLines(n int) []LogLine {
	if s.logBuf == nil {
		return nil
	}
	lines := s.logBuf.Snapshot()
	if n > len(lines) {
		n = len(lines)
	}
	out := make([]LogLine, 0, n)
	for i := len(lines) - 1; i >= len(lines)-n; i-- {
		out = append(out, lines[i])
	}
	return out
}

// handleDiagnosticsRows returns just the rows fragment so the page can poll
// every few seconds without rerendering the chrome.
func (s *Server) handleDiagnosticsRows(w http.ResponseWriter, r *http.Request) {
	data := s.common("Logs", r)
	data["Drops"] = s.drops.snapshot()
	data["Blocked"] = s.srcLimiter.BlockedSources()
	renderFragment(w, r, s.tmpl, "diagRows", data)
}

// handleDiagnosticsLog returns just the log <pre> fragment so the page's
// log panel can refresh its contents on its own HTMX poll without
// swapping the outer <details> element (which would reset the operator's
// expand/collapse choice).
func (s *Server) handleDiagnosticsLog(w http.ResponseWriter, r *http.Request) {
	data := s.common("Logs", r)
	data["LogLines"] = s.recentLogLines(20)
	renderFragment(w, r, s.tmpl, "diagLog", data)
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
		flash(w, false, "POST required")
		return
	}
	if err := r.ParseForm(); err != nil {
		flash(w, false, "bad form")
		return
	}
	if !s.requireCSRF(w, r) {
		return
	}
	if !s.requireUnlocked(w, func(l config.Lockdown) bool { return l.LockSettings }, "settings") {
		return
	}
	// Per-field validation up front. Accumulate all errors so the user can
	// fix them in one pass instead of whack-a-mole.
	var verrs []string
	// Compose CALL[-SSID] from split inputs. Fall back to the legacy combined
	// "callsign" field for hand-crafted/api callers, but the UI now sends both
	// callsign_base + callsign_ssid.
	combinedCS := strings.TrimSpace(r.FormValue("callsign"))
	if base := strings.TrimSpace(r.FormValue("callsign_base")); base != "" {
		combinedCS = strings.ToUpper(base)
		if ssid := strings.TrimSpace(r.FormValue("callsign_ssid")); ssid != "" && ssid != "0" {
			n, err := strconv.Atoi(ssid)
			if err != nil || n < 0 || n > 15 {
				verrs = append(verrs, "ssid must be 0-15")
			} else {
				combinedCS = fmt.Sprintf("%s-%d", combinedCS, n)
			}
		}
	}
	if combinedCS != "" {
		if _, err := validateCallsign(combinedCS); err != nil {
			verrs = append(verrs, "callsign: "+err.Error())
		}
	}
	passcodeIn := strings.TrimSpace(r.FormValue("passcode"))
	if err := validatePasscode(passcodeIn); err != nil {
		verrs = append(verrs, err.Error())
	} else if passcodeIn != "" && passcodeIn != "-1" && combinedCS != "" {
		if !aprsISPasscodeMatches(combinedCS, passcodeIn) {
			verrs = append(verrs, fmt.Sprintf("passcode doesn't match callsign %s", combinedCS))
		}
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
		cs, _ := validateCallsign(combinedCS)
		st.Callsign = cs
		st.Passcode = sanitizeAPRSField(passcodeIn)
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
			st.AllowSendBulletins = r.FormValue("allow_send_bulletins") == "1"
			if v := r.FormValue("igate_recent_rf_minutes"); v != "" {
				if m, err := strconv.Atoi(v); err == nil && m >= 5 && m <= 1440 {
					st.IGateRecentRFMinutes = m
				}
			}
			// IS→RF outer-frame path. Empty string is valid (direct only).
			// Re-use the beacon-path validator (rejects WIDE>2 hops and
			// unrecognized tokens). Silently ignore malformed input rather
			// than aborting the entire settings save.
			if v := r.FormValue("igate_tx_path"); v != "" {
				if hops, perr := validateBeaconPath(v); perr == nil && len(hops) <= 2 {
					st.IGateTXPath = strings.Join(hops, ",")
				}
			} else {
				st.IGateTXPath = ""
			}
		}
		// KISS advanced TNC params. 0 = "don't send the command". Range clamps
		// stop a malicious POST from causing a TNC to lock keyed for minutes.
		clampMs := func(v int) int {
			if v < 0 {
				return 0
			}
			if v > 2550 {
				return 2550
			}
			return v
		}
		if v, err := strconv.Atoi(r.FormValue("tnc_tx_delay_ms")); err == nil {
			st.TNCTXDelayMs = clampMs(v)
		}
		if v, err := strconv.Atoi(r.FormValue("tnc_persist")); err == nil {
			if v < 0 {
				v = 0
			}
			if v > 255 {
				v = 255
			}
			st.TNCPersist = v
		}
		if v, err := strconv.Atoi(r.FormValue("tnc_slot_time_ms")); err == nil {
			st.TNCSlotTimeMs = clampMs(v)
		}
		if v, err := strconv.Atoi(r.FormValue("tnc_tx_tail_ms")); err == nil {
			st.TNCTXTailMs = clampMs(v)
		}
		if t := r.FormValue("theme"); t == "auto" || t == "light" || t == "dark" {
			st.Theme = t
		}
		if d, err := strconv.Atoi(r.FormValue("retention_days")); err == nil && d >= 0 && d <= 3650 {
			st.RetentionDays = d
		}
		if tf := r.FormValue("time_format"); tf == "12h" || tf == "24h" {
			st.TimeFormat = tf
		}
		// Validate the IANA name actually loads — silently accept blank
		// (= use server's local) but reject typos so the UI doesn't end
		// up showing UTC unexpectedly when LoadLocation fails at render.
		if tz := strings.TrimSpace(r.FormValue("timezone")); tz != "" {
			if _, err := time.LoadLocation(tz); err != nil {
				return fmt.Errorf("timezone: %q is not a valid IANA name", tz)
			}
			st.Timezone = tz
		} else {
			st.Timezone = ""
		}
		return nil
	})
	if err != nil {
		flash(w, false, err.Error())
		return
	}
	// Drop any in-progress wizard drafts so the next /setup visit
	// reflects what we just committed instead of replaying the operator's
	// pre-save state. Without this, a setting changed here (e.g. Allow
	// Bulletins) would still show its old value in the wizard's advanced-
	// flags step until the 30-min draft TTL aged it out.
	s.wmu.Lock()
	for k := range s.wdrafts {
		delete(s.wdrafts, k)
	}
	s.wmu.Unlock()
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

// handleAccount: HTMX endpoint posted from the Settings → Account form.
// Saves username, password (optional), and lockdown flags in one shot.
//
// The current password is required as a re-auth challenge only when the
// operator is actually changing username or password — toggling a
// lockdown checkbox alone doesn't need it. (The CSRF token + session
// cookie already authenticate the request; current-password is the
// extra "are you really you?" gate for identity-affecting changes.)
//
// Lockdown enforcement: enabling LockSettings here does NOT lock this
// handler against itself, because the gate check runs at request entry
// before the new value is applied. Once the operator clicks Save with
// "Lock settings" ticked, this handler succeeds — but every subsequent
// settings/account write returns 403 until the operator edits aprgo.conf
// and restarts.
func (s *Server) handleAccount(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		flash(w, false, "POST required")
		return
	}
	if err := r.ParseForm(); err != nil {
		flash(w, false, "bad form")
		return
	}
	if !s.requireCSRF(w, r) {
		return
	}
	if !s.requireUnlocked(w, func(l config.Lockdown) bool { return l.LockSettings }, "settings") {
		return
	}

	newUser := strings.TrimSpace(r.FormValue("username"))
	newPass := r.FormValue("new")
	confirm := r.FormValue("confirm")
	usernameChanging := newUser != "" && newUser != s.config.Username()
	passwordChanging := newPass != "" || confirm != ""

	if usernameChanging || passwordChanging {
		current := r.FormValue("current")
		if current == "" {
			flash(w, false, "Current password required to change username or password.")
			return
		}
		if !s.config.CheckPassword(current) {
			flash(w, false, "Current password incorrect.")
			return
		}
	}

	if usernameChanging {
		if err := config.ValidateUsername(newUser); err != nil {
			flash(w, false, err.Error())
			return
		}
		if err := s.config.SetUsername(newUser); err != nil {
			flash(w, false, err.Error())
			return
		}
	}

	if passwordChanging {
		if newPass != confirm {
			flash(w, false, "New password and confirmation do not match.")
			return
		}
		if err := s.config.SetPassword(newPass); err != nil {
			flash(w, false, err.Error())
			return
		}
	}

	// Lockdown flags. Each checkbox posts "1" when ticked, absent
	// otherwise. The lockdown is RATCHETED — the UI can only flip OFF→ON,
	// never ON→OFF. Once a flag is on, the only way back is to edit
	// aprgo.conf on the server and restart aprgo. This is enforced two
	// ways: the template hides the checkbox for any already-true flag,
	// and the handler OR's the form value against the existing raw value
	// so a hand-crafted POST that omits a locked flag cannot clear it.
	curRaw := s.config.Snapshot().Lockdown
	newLock := config.Lockdown{
		LockSettings:     curRaw.LockSettings || r.FormValue("lock_settings") == "1",
		DisableMessaging: curRaw.DisableMessaging || r.FormValue("disable_messaging") == "1",
		DisableBulletins: curRaw.DisableBulletins || r.FormValue("disable_bulletins") == "1",
		LockAll:          curRaw.LockAll || r.FormValue("lock_all") == "1",
	}
	if err := s.config.SetLockdown(newLock); err != nil {
		flash(w, false, err.Error())
		return
	}

	flash(w, true, "Account saved.")
}

// handleStats renders the operator stats summary: at-a-glance counters,
// time-windowed packet activity, top sources, top drop reasons, queue
// depths, and storage info.
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	c1h, _ := s.store.CountPacketsSince(now.Add(-1 * time.Hour))
	c24h, _ := s.store.CountPacketsSince(now.Add(-24 * time.Hour))
	c7d, _ := s.store.CountPacketsSince(now.Add(-7 * 24 * time.Hour))
	cAll, _ := s.store.CountPacketsSince(time.Time{})
	top24, _ := s.store.TopSourcesSince(now.Add(-24*time.Hour), 10)
	storage, _ := s.store.PacketStorageStats()

	snap := s.state.Snapshot()
	data := s.common("Stats", r)
	data["Uptime"] = now.Sub(s.stats.startedAt)
	data["StartedAt"] = s.stats.startedAt
	data["PktsRF"] = s.stats.pktsRF.Load()
	data["PktsIS"] = s.stats.pktsIS.Load()
	data["PktsTX"] = s.stats.pktsTX.Load()
	data["SentIS"] = s.stats.sentIS.Load()
	data["SentRF"] = s.stats.sentRF.Load()
	data["Digipeats"] = s.stats.digipeats.Load()
	data["DropsTotal"] = s.stats.dropsTotal.Load()
	data["RateLimited"] = s.stats.rateLimited.Load()
	data["DistDropped"] = s.stats.distDropped.Load()
	data["DupesDropped"] = s.stats.dupesDropped.Load()
	data["TopDropReasons"] = s.stats.TopDropReasons(8)
	data["Count1h"] = c1h
	data["Count24h"] = c24h
	data["Count7d"] = c7d
	data["CountAll"] = cAll
	data["TopSources24h"] = top24
	data["Storage"] = storage
	data["RFConnected"] = s.rf.Connected()
	data["ISConnected"] = s.is.Connected()
	data["BlockedCount"] = len(s.srcLimiter.BlockedSources())
	data["RetentionDays"] = snap.RetentionDays
	s.render(w, "stats.html", data)
}
