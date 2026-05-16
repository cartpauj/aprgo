package server

import (
	"context"
	"fmt"
	"html"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"aprgo/internal/state"
	"aprgo/internal/tnc"
)

// Each wizard flavor is a fixed sequence of step keys.
// "first-run"  → identity, location, tnc, mode, beacon, done
// "tnc"        → tnc, done
// "location"   → location, done
// "mode"       → mode, done
type wizardFlavor string

const (
	flavorFirstRun wizardFlavor = "first-run"
	flavorTNC      wizardFlavor = "tnc"
	flavorLoc      wizardFlavor = "location"
	flavorMode     wizardFlavor = "mode"
)

var wizardSteps = map[wizardFlavor][]string{
	flavorFirstRun: {"identity", "location", "tnc", "mode", "advanced-flags", "beacon", "done"},
	flavorTNC:      {"tnc", "done"},
	flavorLoc:      {"location", "done"},
	flavorMode:     {"mode", "advanced-flags", "done"},
}

// shouldSkipStep returns true when a step is conditional on the current mode
// and the mode says not to show it. Used by wizardSave (forward), wizardBack,
// and renderStep (visible-step filter) so all three stay in sync.
func shouldSkipStep(stepKey string, m state.Mode) bool {
	switch stepKey {
	case "beacon":
		return m == state.ModeRXOnly
	case "advanced-flags":
		return m != state.ModeAdvanced
	}
	return false
}

var wizardLabels = map[wizardFlavor]string{
	flavorFirstRun: "First-time setup",
	flavorTNC:      "Change TNC",
	flavorLoc:      "Change location",
	flavorMode:     "Switch operating mode",
}

// wizardDraft is the in-progress state of a wizard. Holds a working copy of
// state.State; only committed on the final step. The mutex serializes
// concurrent access from two browser tabs sharing the same session cookie.
type wizardDraft struct {
	mu       sync.Mutex
	Flavor   wizardFlavor
	StepIdx  int
	Draft    state.State
	Modified time.Time

	// LastErr is shown on the next render of the current step (e.g. the user
	// hit Next with invalid form data). Cleared after one render.
	LastErr string
}

func (s *Server) draftFor(r *http.Request, create wizardFlavor) *wizardDraft {
	key, ok := sessionKey(r)
	if !ok {
		return nil
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if d, found := s.wdrafts[key]; found && time.Since(d.Modified) < 30*time.Minute {
		return d
	}
	if create == "" {
		return nil
	}
	d := &wizardDraft{
		Flavor:   create,
		StepIdx:  0,
		Draft:    s.state.Snapshot(),
		Modified: time.Now(),
	}
	s.wdrafts[key] = d
	return d
}

// sessionKey returns the value of the session cookie; ok=false means the
// request has no session cookie and we should refuse to track a draft.
func sessionKey(r *http.Request) (string, bool) {
	if c, err := r.Cookie("aprgo_session"); err == nil {
		return c.Value, true
	}
	return "", false
}

// wizardStart picks the right flavor based on state and entry URL.
//
// CRITICAL: this handler is reached from many indirect paths — the
// setup-incomplete middleware redirects any non-/setup URL here, browsers
// hit it via favicon prefetches, the user may have it open in two tabs.
// It MUST NOT mutate an existing draft's StepIdx; doing so reset users
// back to step 0 mid-wizard. Only freshly-created drafts start at 0
// (which draftFor already does). For existing drafts, just bounce to
// the flavor URL so wizardRouter renders whatever step they're on.
func (s *Server) wizardStart(w http.ResponseWriter, r *http.Request) {
	key, ok := sessionKey(r)
	if !ok {
		http.Redirect(w, r, "/login?next=/setup", http.StatusFound)
		return
	}
	s.wmu.Lock()
	d, existed := s.wdrafts[key]
	if existed && time.Since(d.Modified) >= 30*time.Minute {
		existed = false
		delete(s.wdrafts, key)
	}
	s.wmu.Unlock()
	flavor := flavorFirstRun
	if existed {
		flavor = d.Flavor
	}
	if !existed {
		_ = s.draftFor(r, flavor) // creates with StepIdx=0
	}
	http.Redirect(w, r, "/setup/"+string(flavor), http.StatusFound)
}

// wizardBack steps the wizard one step backwards (skipping beacon step if
// mode is RX-only, mirroring the forward-skip logic), then redirects to the
// flavor URL so the new current step renders. Idempotent at step 0 (just
// re-renders step 0).
func (s *Server) wizardBack(w http.ResponseWriter, r *http.Request) {
	d := s.draftFor(r, "")
	if d == nil {
		http.Redirect(w, r, "/setup", http.StatusFound)
		return
	}
	d.mu.Lock()
	steps := wizardSteps[d.Flavor]
	if d.StepIdx > 0 {
		d.StepIdx--
		for d.StepIdx > 0 && shouldSkipStep(steps[d.StepIdx], d.Draft.Mode) {
			d.StepIdx--
		}
	}
	d.Modified = time.Now()
	flavor := d.Flavor
	d.mu.Unlock()
	http.Redirect(w, r, "/setup/"+string(flavor), http.StatusFound)
}

// wizardRouter handles /setup/<flavor> and renders the current step.
func (s *Server) wizardRouter(w http.ResponseWriter, r *http.Request) {
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/setup/"), "/", 2)
	flavor := wizardFlavor(parts[0])
	if _, ok := wizardSteps[flavor]; !ok {
		http.NotFound(w, r)
		return
	}
	d := s.draftFor(r, flavor)
	if d.Flavor != flavor {
		// User switched flavors mid-wizard (e.g. visiting /setup/tnc while
		// a /setup/first-run draft existed). Recycle the draft for the new
		// flavor — start fresh from current persisted state.
		d.Flavor = flavor
		d.StepIdx = 0
		d.Draft = s.state.Snapshot()
	}
	// If the draft was finished (parked at "done") and the user is coming
	// BACK to this URL later — e.g. clicking "Change TNC" from Settings
	// after completing the TNC wizard once — reset to step 0 with a
	// fresh state snapshot so they can re-walk the flow.
	//
	// Critically: we use a 10-second floor on draft age so we DON'T reset
	// during the natural post-save redirect (wizardSave just advanced to
	// done and bounced to this URL milliseconds ago — the user is supposed
	// to see the done page, not get teleported back to step 1).
	steps := wizardSteps[d.Flavor]
	if d.StepIdx >= 0 && d.StepIdx < len(steps) && steps[d.StepIdx] == "done" &&
		time.Since(d.Modified) > 10*time.Second {
		d.StepIdx = 0
		d.Draft = s.state.Snapshot()
		d.Modified = time.Now()
	}
	s.renderStep(w, r, d)
}

// wizardSave is POSTed by each step's form. Path: /setup/save/<step>.
func (s *Server) wizardSave(w http.ResponseWriter, r *http.Request) {
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
	step := strings.TrimPrefix(r.URL.Path, "/setup/save/")
	d := s.draftFor(r, "")
	if d == nil {
		http.Redirect(w, r, "/setup", http.StatusFound)
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	switch step {
	case "identity":
		d.Draft.Callsign = strings.TrimSpace(r.FormValue("callsign"))
		d.Draft.Passcode = strings.TrimSpace(r.FormValue("passcode"))
	case "location":
		lat, latErr := strconv.ParseFloat(strings.TrimSpace(r.FormValue("lat")), 64)
		lon, lonErr := strconv.ParseFloat(strings.TrimSpace(r.FormValue("lon")), 64)
		if latErr != nil || lonErr != nil {
			d.LastErr = "Please click on the map to drop a pin, or enter latitude and longitude manually."
			s.renderStep(w, r, d)
			return
		}
		if lat < -90 || lat > 90 || lon < -180 || lon > 180 {
			d.LastErr = "Latitude must be -90..90 and longitude -180..180."
			s.renderStep(w, r, d)
			return
		}
		if lat == 0 && lon == 0 {
			d.LastErr = "Position 0,0 isn't a real iGate location. Click on the map where your station is."
			s.renderStep(w, r, d)
			return
		}
		d.Draft.Lat = lat
		d.Draft.Lon = lon
		if km, err := strconv.Atoi(r.FormValue("filter_radius")); err == nil && km > 0 {
			d.Draft.ISFilter = fmt.Sprintf("r/%.2f/%.2f/%d", lat, lon, km)
		}
	case "tnc":
		kindPath := r.FormValue("kind_path")
		tcpHost := strings.TrimSpace(r.FormValue("tcp_host"))
		tcpPort := strings.TrimSpace(r.FormValue("tcp_port"))
		switch {
		case strings.HasPrefix(kindPath, "serial::"):
			d.Draft.TNCKind = state.TNCSerial
			d.Draft.TNCSerial = strings.TrimPrefix(kindPath, "serial::")
			d.Draft.TNCAddr = ""
		case strings.HasPrefix(kindPath, "bt::"):
			// Classic BT pairing can take 20-30s; run it as an async job and
			// return immediately so the wizard isn't a blank-screen wait.
			// startBTPair handles the HTTP response itself; we return early.
			addr := strings.TrimPrefix(kindPath, "bt::")
			s.startBTPair(w, r, addr)
			return
		case strings.HasPrefix(kindPath, "tcp::"):
			if tcpHost == "" {
				flash(w, false, "TCP host required")
				return
			}
			if tcpPort == "" {
				tcpPort = "8001"
			}
			d.Draft.TNCKind = state.TNCTCP
			d.Draft.TNCAddr = tcpHost + ":" + tcpPort
			d.Draft.TNCSerial = ""
		}
	case "mode":
		m := state.Mode(r.FormValue("mode"))
		d.Draft.Mode = m
		applyModeDefaults(&d.Draft, m)
	case "advanced-flags":
		d.Draft.TXEnable = r.FormValue("tx_enable") == "1"
		d.Draft.GateRFtoIS = r.FormValue("gate_rf_to_is") == "1"
		d.Draft.GateIStoRF = r.FormValue("gate_is_to_rf") == "1"
		d.Draft.DigipeatWIDE1 = r.FormValue("digipeat_wide1") == "1"
		d.Draft.DigipeatWIDE2 = r.FormValue("digipeat_wide2") == "1"
		d.Draft.ViscousDelay = r.FormValue("viscous_delay") == "1"
		d.Draft.OfflineMode = r.FormValue("offline_mode") == "1"
		d.Draft.MessagingOnlyMode = r.FormValue("messaging_only_mode") == "1"
		d.Draft.PreemptiveDigipeat = r.FormValue("preemptive_digipeat") == "1"
	case "beacon":
		comment := strings.TrimSpace(r.FormValue("beacon_comment"))
		secs := state.DefaultBeaconEveryS
		if mins, err := strconv.Atoi(r.FormValue("beacon_every_min")); err == nil && mins >= 10 {
			secs = mins * 60
		}
		sym, defaultCmt, msg := beaconDefaultsFor(d.Draft.Mode)
		if comment == "" {
			comment = defaultCmt
		}
		replaced := false
		for i := range d.Draft.Beacons {
			if d.Draft.Beacons[i].Name == "position" {
				d.Draft.Beacons[i].Symbol = sym
				d.Draft.Beacons[i].Comment = comment
				d.Draft.Beacons[i].Messages = msg
				d.Draft.Beacons[i].EveryS = secs
				d.Draft.Beacons[i].Enabled = true
				replaced = true
				break
			}
		}
		if !replaced {
			d.Draft.Beacons = append(d.Draft.Beacons, state.Beacon{
				Name: "position", Symbol: sym, Comment: comment, Messages: msg,
				Dest: state.DefaultBeaconDest, EveryS: secs, Enabled: true,
			})
		}
	}

	// Advance, with skip-logic where appropriate.
	steps := wizardSteps[d.Flavor]
	d.StepIdx++
	for d.StepIdx < len(steps) && shouldSkipStep(steps[d.StepIdx], d.Draft.Mode) {
		d.StepIdx++
	}
	d.Modified = time.Now()

	if d.StepIdx >= len(steps) || steps[d.StepIdx] == "done" {
		if err := s.commitWizardDraft(d); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		d.StepIdx = len(steps) - 1
	}
	// Post-Redirect-Get so the URL bar reflects the new step (and a browser
	// refresh re-fetches the step page, not the save POST). Eliminates the
	// "URL says save/location but content moved on" confusion.
	flavor := d.Flavor
	// Cache-busting query string defeats Chrome's prefetch / cached-redirect-
	// follow behavior that has been silently re-serving the previous step's
	// HTML for some operators.
	http.Redirect(w, r, fmt.Sprintf("/setup/%s?t=%d", flavor, time.Now().UnixNano()), http.StatusSeeOther)
}

// commitWizardDraft persists the in-memory draft to state.State. Called when
// the wizard reaches its "done" step from either wizardSave (normal flow) or
// handleTNCConfirm (the async Bluetooth-pair → continue branch, which
// bypasses wizardSave entirely and otherwise leaves state untouched).
// Caller must hold d.mu.
func (s *Server) commitWizardDraft(d *wizardDraft) error {
	return s.state.Update(func(st *state.State) error {
		st.Callsign = d.Draft.Callsign
		st.Passcode = d.Draft.Passcode
		st.Lat = d.Draft.Lat
		st.Lon = d.Draft.Lon
		st.ISServer = d.Draft.ISServer
		st.ISFilter = d.Draft.ISFilter
		st.TNCKind = d.Draft.TNCKind
		st.TNCSerial = d.Draft.TNCSerial
		st.TNCAddr = d.Draft.TNCAddr
		st.TNCChannel = d.Draft.TNCChannel
		st.Mode = d.Draft.Mode
		st.Beacons = d.Draft.Beacons
		st.TXEnable = d.Draft.TXEnable
		st.GateRFtoIS = d.Draft.GateRFtoIS
		st.GateIStoRF = d.Draft.GateIStoRF
		st.DigipeatWIDE1 = d.Draft.DigipeatWIDE1
		st.DigipeatWIDE2 = d.Draft.DigipeatWIDE2
		st.ViscousDelay = d.Draft.ViscousDelay
		st.OfflineMode = d.Draft.OfflineMode
		st.MessagingOnlyMode = d.Draft.MessagingOnlyMode
		st.PreemptiveDigipeat = d.Draft.PreemptiveDigipeat
		st.SetupComplete = true
		return nil
	})
}

// renderStep paints the current wizard step.
func (s *Server) renderStep(w http.ResponseWriter, r *http.Request, d *wizardDraft) {
	steps := wizardSteps[d.Flavor]
	if d.StepIdx < 0 {
		d.StepIdx = 0
	}
	if d.StepIdx >= len(steps) {
		d.StepIdx = len(steps) - 1
	}
	stepKey := steps[d.StepIdx]
	// For the progress bar, hide steps that won't be reached given the
	// current mode (beacon skipped in RX-only; advanced-flags skipped
	// unless mode is Advanced).
	visibleSteps := make([]string, 0, len(steps))
	for _, k := range steps {
		if shouldSkipStep(k, d.Draft.Mode) {
			continue
		}
		visibleSteps = append(visibleSteps, k)
	}
	visIdx := 0
	for _, k := range steps[:d.StepIdx] {
		if shouldSkipStep(k, d.Draft.Mode) {
			continue
		}
		visIdx++
	}
	data := s.common("Setup", r)
	data["WizardLabel"] = wizardLabels[d.Flavor]
	data["StepIdx"] = visIdx
	data["Steps"] = wizardStepTitles(visibleSteps)
	data["StepKey"] = stepKey
	data["StepTitle"] = wizardStepTitle(stepKey)
	data["StepHelp"] = wizardStepHelp(stepKey)
	data["St"] = d.Draft
	data["PrevURL"] = "/setup/back"
	data["HasPrev"] = d.StepIdx > 0
	data["StepErr"] = d.LastErr
	d.LastErr = "" // one-shot — clears after this render
	// Step-specific extras
	switch stepKey {
	case "location":
		// Filter radius is encoded as the trailing number in ISFilter
		// ("r/<lat>/<lon>/<km>"). Extract it so the field reflects the
		// user's previous choice on re-entry instead of always defaulting
		// to 150.
		data["FilterRadiusKm"] = filterRadiusFromIS(d.Draft.ISFilter)
	case "tnc":
		serials, _ := tnc.ListSerial()
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		paired, _ := tnc.Paired(ctx)
		data["Serials"] = serials
		data["BTPaired"] = paired
		host, port := splitTNCHostPort(d.Draft.TNCAddr)
		data["TNCHost"] = host
		data["TNCPort"] = port
	case "beacon":
		_, cmt, _ := beaconDefaultsFor(d.Draft.Mode)
		data["BeaconCommentDefault"] = cmt
		data["ModeSummary"] = string(d.Draft.Mode)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Wizard pages must never be cached — a cached form would carry a stale
	// CSRF token and stale step state, producing silent submit failures.
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	if err := s.tmpl.ExecuteTemplate(w, "setup.html", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// wizardBTScan: HTMX endpoint that triggers a Bluetooth scan and returns
// a fragment of radio buttons. Scans both Classic Bluetooth (SPP via
// bluetoothctl) and BLE (advertising BLE-KISS service UUID).
func (s *Server) wizardBTScan(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	_ = r.ParseForm()
	if !s.requireCSRF(w, r) {
		return
	}
	// Only one BT scan at a time so concurrent clicks don't trash BlueZ state.
	s.scanMu.Lock()
	defer s.scanMu.Unlock()

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	classicDevs, classicErr := tnc.Scan(ctx, 8*time.Second)
	if classicErr != nil {
		fmt.Fprintf(w, `<div class="flash err">Scan failed: %s</div>`,
			html.EscapeString(classicErr.Error()))
		return
	}
	if len(classicDevs) == 0 {
		fmt.Fprintf(w, `<div class="dim">No new devices found. Put your TNC in pairing/discoverable mode and try again.</div>`)
		return
	}

	var b strings.Builder
	b.WriteString(`<div class="scan-results">`)
	for _, d := range classicDevs {
		fmt.Fprintf(&b, `<label class="cb"><input type="radio" name="kind_path" value="bt::%s"> <span class="eyebrow">Classic</span> <code>%s</code> <span class="dim">— %s</span></label>`,
			html.EscapeString(d.Address), html.EscapeString(d.Address), html.EscapeString(d.Name))
	}
	b.WriteString(`</div>`)
	_, _ = w.Write([]byte(b.String()))
}

// applyModeDefaults pre-fills appropriate flags for the chosen mode and seeds
// a single "position" beacon if none exist. Users can edit / add / remove
// beacons later from the settings page.
func applyModeDefaults(st *state.State, m state.Mode) {
	var path []string
	// Every preset clears PreemptiveDigipeat — it's an Advanced-only opt-in.
	// ModeAdvanced (handled below) leaves the operator's setting alone.
	if m != state.ModeAdvanced {
		st.PreemptiveDigipeat = false
	}
	switch m {
	case state.ModeRXOnly:
		st.TXEnable = false
		st.GateRFtoIS = true
		st.GateIStoRF = false
		st.DigipeatWIDE1 = false
		st.DigipeatWIDE2 = false
		// RX-only disables every existing beacon defensively.
		for i := range st.Beacons {
			st.Beacons[i].Enabled = false
		}
		return
	case state.ModeTXIGate:
		st.TXEnable = true
		st.GateRFtoIS = true
		st.GateIStoRF = true
		st.DigipeatWIDE1 = false
		st.DigipeatWIDE2 = false
		path = []string{"WIDE2-1"}
	case state.ModeFillinIG:
		st.TXEnable = true
		st.GateRFtoIS = true
		st.GateIStoRF = true
		st.DigipeatWIDE1 = true
		st.DigipeatWIDE2 = false
		st.ViscousDelay = true // polite fill-in default
		path = []string{"WIDE1-1"}
	case state.ModeDigi:
		st.TXEnable = true
		st.GateRFtoIS = false
		st.GateIStoRF = false
		st.DigipeatWIDE1 = true
		st.DigipeatWIDE2 = true
		st.ViscousDelay = true // even full digis are polite on WIDE1-1
		st.OfflineMode = false
		st.MessagingOnlyMode = false
		path = nil // mountaintop digis beacon direct
	case state.ModeMessaging:
		// Selective-gating iGate: bridges person-to-person messages
		// between RF and APRS-IS but skips position beacons / weather /
		// telemetry / status. Lower TX duty cycle than TX-iGate; useful
		// in dense areas where iGates already exist.
		st.TXEnable = true
		st.GateRFtoIS = true
		st.GateIStoRF = true
		st.DigipeatWIDE1 = false
		st.DigipeatWIDE2 = false
		st.ViscousDelay = false
		st.OfflineMode = false
		st.MessagingOnlyMode = true
		path = []string{"WIDE2-1"}
	case state.ModeOffline:
		// Off-grid digi: no APRS-IS at all. Mountaintop hilltop, EMCOMM
		// field-day, any RF relay station with no internet uplink.
		st.TXEnable = true
		st.GateRFtoIS = false
		st.GateIStoRF = false
		st.DigipeatWIDE1 = true
		st.DigipeatWIDE2 = true
		st.ViscousDelay = true
		st.OfflineMode = true
		st.MessagingOnlyMode = false
		path = nil
	case state.ModeAdvanced:
		// Don't touch any flags — operator manages them directly via
		// Settings. We just snap the label.
		return
	}
	// Seed a single position beacon if none exists yet.
	if len(st.Beacons) == 0 {
		sym, cmt, msg := beaconDefaultsFor(m)
		st.Beacons = []state.Beacon{{
			Name:     "position",
			Symbol:   sym,
			Comment:  cmt,
			Messages: msg,
			Dest:     state.DefaultBeaconDest,
			Path:     path,
			EveryS:   state.DefaultBeaconEveryS,
			Enabled:  true,
		}}
	}
}

// beaconDefaultsFor returns the recommended symbol + comment + messaging flag
// for a station running in the given mode.
func beaconDefaultsFor(m state.Mode) (symbol, comment string, messages bool) {
	switch m {
	case state.ModeRXOnly:
		return "R&", "aprgo RX iGate", false
	case state.ModeTXIGate:
		return "I&", "aprgo iGate", true
	case state.ModeFillinIG:
		return "I&", "aprgo fill-in digi+iGate", true
	case state.ModeDigi:
		return "S#", "aprgo digi", false
	case state.ModeMessaging:
		return "I&", "aprgo messaging-only iGate", true
	case state.ModeOffline:
		return "S#", "aprgo offline digi", false
	}
	return "I&", "aprgo", true
}

func wizardStepTitles(steps []string) []string {
	out := make([]string, 0, len(steps))
	for _, s := range steps {
		out = append(out, wizardStepTitle(s))
	}
	return out
}

func wizardStepTitle(key string) string {
	switch key {
	case "identity":
		return "Identity"
	case "location":
		return "Location"
	case "tnc":
		return "TNC"
	case "mode":
		return "Mode"
	case "advanced-flags":
		return "Flags"
	case "beacon":
		return "Beacon"
	case "done":
		return "Done"
	}
	return key
}

func wizardStepHelp(key string) string {
	switch key {
	case "identity":
		return "Your station's callsign and APRS-IS passcode."
	case "location":
		return "Where this station lives — used for the map and to scope the APRS-IS firehose."
	case "tnc":
		return "Pick how aprgo will reach your radio."
	case "mode":
		return "What role this station plays on the air."
	case "advanced-flags":
		return "Set each gating + digipeating flag individually."
	case "beacon":
		return "Your own position beacon, transmitted periodically."
	case "done":
		return "All set."
	}
	return ""
}

// splitTNCHostPort splits a stored TNCAddr ("host:port") into its parts for
// pre-filling the wizard's separate host + port fields. Splits on the LAST
// colon so IPv6 literals like "[::1]:8001" still split correctly. Returns
// ("", "8001") for an empty addr — 8001 is the Direwolf / tnc-server / WiFi
// TNC convention and the right default for a fresh setup.
func splitTNCHostPort(addr string) (host, port string) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "", "8001"
	}
	idx := strings.LastIndex(addr, ":")
	if idx < 0 {
		return addr, "8001"
	}
	host = addr[:idx]
	port = addr[idx+1:]
	if port == "" {
		port = "8001"
	}
	return host, port
}

// filterRadiusFromIS parses the trailing km from an APRS-IS server filter
// string of the form "r/<lat>/<lon>/<km>". Returns 150 (a sensible default
// for a typical home iGate) when the filter is empty or in some other
// unexpected shape.
func filterRadiusFromIS(s string) int {
	if !strings.HasPrefix(s, "r/") {
		return 150
	}
	parts := strings.Split(s, "/")
	if len(parts) < 4 {
		return 150
	}
	if km, err := strconv.Atoi(parts[3]); err == nil && km >= 10 && km <= 1000 {
		return km
	}
	return 150
}
