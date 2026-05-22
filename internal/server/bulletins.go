package server

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"aprgo/internal/state"
	"aprgo/internal/store"
)

// bulletinSendCooldown is the minimum gap between consecutive sends of
// the same bulletin identifier (e.g. BLN1ARES). Prevents accidental
// double-clicks from flooding the channel; spec says bulletins decay
// from 30s up to once-per-NetCycle, but we don't auto-repeat — a fresh
// click after this window is intentional.
const bulletinSendCooldown = 5 * time.Minute

// bulletinSendTracker remembers the last-TX time per uppercase addressee
// so the send handler can refuse rapid repeats. In-memory only; the
// state is meant to bound operator mistakes, not persist forever.
type bulletinSendTracker struct {
	mu       sync.Mutex
	lastSent map[string]time.Time
}

func newBulletinSendTracker() *bulletinSendTracker {
	return &bulletinSendTracker{lastSent: make(map[string]time.Time)}
}

// CheckAndMark returns the remaining cooldown duration if the addressee
// was sent recently (zero == "send is allowed; tracker updated"). The
// caller treats a non-zero return as a refusal to send.
func (t *bulletinSendTracker) CheckAndMark(addr string) time.Duration {
	addr = strings.ToUpper(addr)
	t.mu.Lock()
	defer t.mu.Unlock()
	if last, ok := t.lastSent[addr]; ok {
		if elapsed := time.Since(last); elapsed < bulletinSendCooldown {
			return bulletinSendCooldown - elapsed
		}
	}
	t.lastSent[addr] = time.Now()
	return 0
}

// bulletinComposeRE validates the operator-supplied identifier+group.
// Format: "[0-9A-Z]" + optional 1-5 alphanumeric group, e.g. "1",
// "1ARES", "A", "AWX". Anchored full match. Uppercased before check.
var bulletinComposeRE = regexp.MustCompile(`^[0-9A-Z][A-Z0-9]{0,5}$`)

// composeBulletin validates the form fields and builds the 9-char
// addressee string + the encoded info-field bytes ready for TX. The
// addressee is space-padded to 9 chars per APRS101 §14.
func composeBulletin(idAndGroup, body string) (addressee, info string, err error) {
	idAndGroup = strings.ToUpper(strings.TrimSpace(idAndGroup))
	if !bulletinComposeRE.MatchString(idAndGroup) {
		return "", "", errors.New("identifier must be a digit (0-9) or letter (A-Z), optionally followed by 1-5 alphanumeric group chars (e.g. 1, 1ARES, AWX)")
	}
	body = strings.TrimSpace(body)
	if body == "" {
		return "", "", errors.New("body required")
	}
	// Body filter: reuse the standard APRS message rules. Strip CR/LF/
	// NUL, reject reserved chars `{`, `|`, `~`, clamp to 67 chars.
	body = sanitizeAPRSField(body)
	body = strings.Map(func(r rune) rune {
		switch r {
		case '{', '|', '~':
			return -1
		}
		return r
	}, body)
	if len(body) > 67 {
		body = body[:67]
	}
	if body == "" {
		return "", "", errors.New("body became empty after sanitizing — reserved chars `{` `|` `~` were stripped")
	}
	// Prefix with "BLN" + identifier+group, space-pad to 9 chars.
	prefix := "BLN" + idAndGroup
	if len(prefix) > 9 {
		return "", "", errors.New("identifier+group too long")
	}
	addr := prefix + strings.Repeat(" ", 9-len(prefix))
	// APRS message wire format: ":ADDRESSEE:body" — addressee field is
	// always exactly 9 chars (padded), no msg-id (bulletins aren't acked).
	return addr, fmt.Sprintf(":%s:%s", addr, body), nil
}

// handleBulletinSend POST: validates Advanced mode + AllowSendBulletins
// + TXEnable, then composes and transmits a bulletin frame. Per-
// identifier cooldown via bulletinSendTracker prevents accidental
// double-sends. NWS-/SKY-/CWA-style addressees are refused — those are
// reserved for NOAA / aviation services.
func (s *Server) handleBulletinSend(w http.ResponseWriter, r *http.Request) {
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
	if snap.Mode != state.ModeAdvanced {
		http.Error(w, "bulletin send requires Advanced mode", http.StatusForbidden)
		return
	}
	if !snap.AllowSendBulletins {
		http.Error(w, "bulletin send is disabled in Settings", http.StatusForbidden)
		return
	}
	if !snap.TXEnable {
		http.Error(w, "master TX is disabled", http.StatusForbidden)
		return
	}
	if snap.Callsign == "" {
		http.Error(w, "no callsign configured", http.StatusForbidden)
		return
	}
	idGroup := r.FormValue("id_group")
	bodyForm := r.FormValue("body")
	via := r.FormValue("via") // "rf", "is", "both"
	if via == "" {
		via = "both"
	}
	// Connectivity check — refuse a send when the chosen carrier is
	// down. Without this, the frame would silently queue (rf.TX and
	// igate.Send buffer up to 64 / 256) and the operator would think
	// it went out even though nothing reached the air or the server.
	rfUp := s.rf.Connected()
	isUp := s.is.Connected()
	switch via {
	case "rf":
		if !rfUp {
			http.Redirect(w, r, "/bulletins?err="+url.QueryEscape("RF not connected — connect the TNC or pick a different carrier"), http.StatusSeeOther)
			return
		}
	case "is":
		if !isUp {
			http.Redirect(w, r, "/bulletins?err="+url.QueryEscape("APRS-IS not connected — connect to the server or pick a different carrier"), http.StatusSeeOther)
			return
		}
	case "both":
		if !rfUp && !isUp {
			http.Redirect(w, r, "/bulletins?err="+url.QueryEscape("No carrier available — neither RF nor APRS-IS is connected"), http.StatusSeeOther)
			return
		}
		// Partial-availability: silently demote `both` to whichever side
		// is up. Operator gets a single-carrier send rather than the
		// confusing half-fail of "queued to dead channel."
		if !rfUp {
			via = "is"
		} else if !isUp {
			via = "rf"
		}
	}
	addr, info, err := composeBulletin(idGroup, bodyForm)
	if err != nil {
		http.Redirect(w, r, "/bulletins?err="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	// Cooldown — refuse fast double-clicks on the same identifier.
	if remaining := s.bulletinSends.CheckAndMark(addr); remaining > 0 {
		http.Redirect(w, r,
			"/bulletins?err="+url.QueryEscape(fmt.Sprintf("cooldown active — try again in %s", remaining.Round(time.Second))),
			http.StatusSeeOther)
		return
	}
	infoBytes := []byte(info)
	var sendErr error
	if via == "rf" || via == "both" {
		if err := s.txRF(snap.Callsign, state.DefaultBeaconDest, nil, infoBytes); err != nil {
			sendErr = fmt.Errorf("RF: %w", err)
		}
	}
	if (via == "is" || via == "both") && sendErr == nil {
		if err := s.txIS(snap.Callsign, state.DefaultBeaconDest, infoBytes); err != nil {
			sendErr = fmt.Errorf("IS: %w", err)
		}
	}
	if sendErr != nil {
		http.Redirect(w, r, "/bulletins?err="+url.QueryEscape(sendErr.Error()), http.StatusSeeOther)
		return
	}
	// Strip ":ADDR:" prefix to extract the body again for the store row.
	bodyStored := strings.TrimPrefix(info, ":"+addr+":")
	_, _ = s.store.LogMessage(store.Message{
		Time:      time.Now(),
		Direction: "out",
		Source:    snap.Callsign,
		Dest:      addr,
		Body:      bodyStored,
		ViaRF:     via == "rf" || via == "both",
		ViaIS:     via == "is" || via == "both",
		Raw:       info,
	})
	http.Redirect(w, r, "/bulletins?sent=1", http.StatusSeeOther)
}

// Bulletin is one classified bulletin row for the /bulletins page.
// Source field reflects the original transmitter (third-party original
// source if the frame arrived wrapped, else the AX.25 source).
type Bulletin struct {
	Source     string    // sender callsign
	Dest       string    // raw 9-char addressee, e.g. "BLN1ARES " or "NWS-LWX  "
	DestLabel  string    // trimmed addressee for display
	Body       string    // message body
	Time       time.Time // last RX time for this (source, dest)
	Kind       string    // "numbered" | "announce" | "group" | "nws" | "sky" | "cwa"
	Identifier string    // BLN forms: the digit ('0'-'9') or letter ('A'-'Z')
	Group      string    // BLN1ARES → "ARES"
	Office     string    // NWS-LWX → "LWX"; SKYFWD → "FWD"
	Product    string    // NWS source-call last 3 chars (e.g. "TOR", "FFW")
	Severity   string    // "severe" | "warning" | "watch" | "statement" | "info"
}

// isBulletinAddressee reports whether the given 1:9 message-addressee field
// matches a known broadcast pattern (BLN*, NWS*, SKY*, CWA-*). Case-
// insensitive; ignores trailing space padding. Used by the message-keep
// gate in server.parseLoop and by the /bulletins query filter.
func isBulletinAddressee(dest string) bool {
	d := strings.ToUpper(strings.TrimRight(dest, " "))
	switch {
	case strings.HasPrefix(d, "BLN"):
		return true
	case strings.HasPrefix(d, "NWS-"), strings.HasPrefix(d, "NWS_"):
		return true
	case strings.HasPrefix(d, "SKY"):
		return true
	case strings.HasPrefix(d, "CWA-"):
		return true
	}
	return false
}

// classifyBulletin populates kind/identifier/group/office/product/severity
// from the raw (source, dest) pair. Best-effort — unknown destinations get
// Kind="" and the caller may filter accordingly.
func classifyBulletin(source, dest string) Bulletin {
	b := Bulletin{Source: source, Dest: dest}
	d := strings.ToUpper(strings.TrimRight(dest, " "))
	b.DestLabel = d
	switch {
	case strings.HasPrefix(d, "BLN") && len(d) >= 4:
		idChar := d[3]
		b.Identifier = string(idChar)
		switch {
		case idChar >= '0' && idChar <= '9':
			if len(d) > 4 {
				b.Kind = "group"
				b.Group = d[4:]
			} else {
				b.Kind = "numbered"
			}
		case idChar >= 'A' && idChar <= 'Z':
			b.Kind = "announce"
		}
	case strings.HasPrefix(d, "NWS-") || strings.HasPrefix(d, "NWS_"):
		b.Kind = "nws"
		if len(d) > 4 {
			b.Office = d[4:]
		}
		// Product code: last 3 chars of source callsign (e.g. FWDFFW → FFW).
		if len(source) >= 3 {
			b.Product = strings.ToUpper(source[len(source)-3:])
			b.Severity = nwsSeverity(b.Product)
			// NWS_ (underscore) variant marks non-severe per WXSVR; downgrade
			// if the product code didn't already classify.
			if strings.HasPrefix(d, "NWS_") && b.Severity == "info" {
				b.Severity = "statement"
			}
		} else {
			b.Severity = "info"
		}
	case strings.HasPrefix(d, "SKY"):
		b.Kind = "sky"
		if len(d) > 3 {
			b.Office = d[3:]
		}
		b.Severity = "info"
	case strings.HasPrefix(d, "CWA-"):
		b.Kind = "cwa"
		if len(d) > 4 {
			b.Office = d[4:]
		}
		b.Severity = "info"
	}
	return b
}

// nwsSeverity maps a 3-letter NWS product code (from the last 3 chars of
// the WXSVR source callsign) to a UI severity bucket. List is sourced from
// aprs-is.net/WX/ProdCodes.aspx — incomplete coverage falls back to "info".
func nwsSeverity(pc string) string {
	switch pc {
	case "TOR", "SVR", "FFW", "TSW":
		return "severe"
	case "HUW", "TRW", "BZW", "WSW", "FLW", "HLS":
		return "warning"
	case "HUA", "FFA", "TOA", "SVA":
		return "watch"
	case "FLS", "SPS", "NPW", "WCN", "AFD":
		return "statement"
	}
	return "info"
}

// passesGroupWhitelist applies Bruninga's whitelist rule (PROTOCOL.TXT):
// empty list = accept all; non-empty = accept plain BLN0-9 unconditionally
// + only listed group bulletins (matched case-insensitively on the 5-char
// group suffix). NWS/SKY/CWA are NOT groups and bypass this filter.
func passesGroupWhitelist(b Bulletin, groups []string) bool {
	switch b.Kind {
	case "numbered", "announce":
		return true // plain BLN0-9 and BLNA-Z always accepted
	case "nws", "sky", "cwa":
		return true // gated separately via NWSSubscribed
	case "group":
		if len(groups) == 0 {
			return true // empty list = receive all groups
		}
		for _, g := range groups {
			if strings.EqualFold(strings.TrimSpace(g), b.Group) {
				return true
			}
		}
		return false
	}
	return false
}

// bulletinsView is the data passed to the bulletins.html template. Splits
// by kind so the template renders sections in priority order: NWS first
// (severe weather wants the eye), then announcements (sticky), then
// numbered + group bulletins together.
type bulletinsView struct {
	NWS         []Bulletin
	SKY         []Bulletin
	CWA         []Bulletin
	Announces   []Bulletin
	Numbered    []Bulletin // includes group bulletins
	GroupList   string     // current MessageGroups joined for the textarea
	NWSEnabled  bool
	Offline     bool
	TotalRows   int
}

// buildBulletinsView pulls bulletin rows from the store, classifies each,
// applies the group whitelist + NWS subscription, sorts into sections,
// and returns the template payload. `cutoff` bounds how far back we look
// (older bulletins are dropped — they're broadcasts, not history).
func buildBulletinsView(rows []store.Bulletin, snap state.State, cutoff time.Time) bulletinsView {
	view := bulletinsView{
		GroupList:  strings.Join(snap.MessageGroups, ", "),
		NWSEnabled: snap.NWSSubscribed,
		Offline:    snap.OfflineMode,
		TotalRows:  len(rows),
	}
	for _, r := range rows {
		if r.Time.Before(cutoff) {
			continue
		}
		b := classifyBulletin(r.Source, r.Dest)
		b.Body = r.Body
		b.Time = r.Time
		if !passesGroupWhitelist(b, snap.MessageGroups) {
			continue
		}
		switch b.Kind {
		case "nws":
			if snap.NWSSubscribed {
				view.NWS = append(view.NWS, b)
			}
		case "sky":
			if snap.NWSSubscribed {
				view.SKY = append(view.SKY, b)
			}
		case "cwa":
			if snap.NWSSubscribed {
				view.CWA = append(view.CWA, b)
			}
		case "announce":
			view.Announces = append(view.Announces, b)
		case "numbered", "group":
			view.Numbered = append(view.Numbered, b)
		}
	}
	// Within each section: severe-first for NWS, newest-first elsewhere.
	sortNewest := func(s []Bulletin) {
		sort.Slice(s, func(i, j int) bool { return s[i].Time.After(s[j].Time) })
	}
	sortNewest(view.SKY)
	sortNewest(view.CWA)
	sortNewest(view.Announces)
	sortNewest(view.Numbered)
	// NWS: severity bucket order severe→warning→watch→statement→info, then time.
	sevOrder := map[string]int{"severe": 0, "warning": 1, "watch": 2, "statement": 3, "info": 4}
	sort.Slice(view.NWS, func(i, j int) bool {
		si, sj := sevOrder[view.NWS[i].Severity], sevOrder[view.NWS[j].Severity]
		if si != sj {
			return si < sj
		}
		return view.NWS[i].Time.After(view.NWS[j].Time)
	})
	return view
}
