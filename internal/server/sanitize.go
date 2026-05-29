package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"aprgo/internal/state"
	"aprgo/internal/webhook"
)

// sanitizeAPRSField strips bytes that would break APRS-IS line protocol or
// AX.25 framing: CR, LF, NUL. Applied to every user-supplied field that
// eventually flows into an IS line or a TNC2 string.
func sanitizeAPRSField(s string) string {
	s = strings.Map(func(r rune) rune {
		switch r {
		case '\r', '\n', 0:
			return -1
		}
		return r
	}, s)
	return strings.TrimSpace(s)
}

// MaxBeaconCommentLen is the position-comment cap from APRS101 §8 ("max 43
// chars after the symbol code"). Status text technically has a higher limit
// but beacons in aprgo are always position reports.
const MaxBeaconCommentLen = 43

// sanitizeBeaconComment cleans operator-supplied beacon comment text per
// APRS101 §5/§8: keep only printable ASCII (0x20-0x7E) excluding `|` and `~`
// (reserved for telemetry / future use), then cap at 43 bytes.
func sanitizeBeaconComment(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c == 0x7F || c == '|' || c == '~' {
			continue
		}
		b.WriteByte(c)
	}
	out := strings.TrimSpace(b.String())
	if len(out) > MaxBeaconCommentLen {
		out = out[:MaxBeaconCommentLen]
	}
	return out
}

// callsignRE matches AX.25 + APRS callsign-SSID format: 1-6 alphanumeric
// followed by optional -SSID where SSID is 0-15 (the AX.25 spec limit:
// the SSID byte has 4 bits for the number).
var callsignRE = regexp.MustCompile(`^[A-Z0-9]{1,6}(-([0-9]|1[0-5]))?$`)

// callsignBaseRE matches just the base callsign portion (no -SSID suffix).
// Used by the wizard which splits call + SSID into separate inputs.
var callsignBaseRE = regexp.MustCompile(`^[A-Z0-9]{1,6}$`)

// wideRE matches the legal WIDEn-N forms a station may originate per the
// APRS New-N paradigm: WIDE1-1 (fill-in, single-hop only), WIDE2-1, and
// WIDE2-2. WIDE1-0 (already-used) and WIDE1-2 (undefined — fill-in is
// single-hop by definition) are explicitly rejected to catch typos at
// save time rather than surface as mystery on-air behavior.
var wideRE = regexp.MustCompile(`^(WIDE1-1|WIDE2-[12])$`)

// validateBeaconPath checks every comma-separated hop is either a valid
// WIDE-token or a real callsign. Returns sanitized list or an error.
func validateBeaconPath(s string) ([]string, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	out := []string{}
	for _, raw := range strings.Split(s, ",") {
		tok := strings.ToUpper(strings.TrimSpace(raw))
		if tok == "" {
			continue
		}
		if wideRE.MatchString(tok) {
			out = append(out, tok)
			continue
		}
		if callsignRE.MatchString(tok) {
			out = append(out, tok)
			continue
		}
		return nil, fmt.Errorf("invalid beacon-path token %q (expected WIDE1-1, WIDE2-1, or a callsign)", raw)
	}
	if len(out) > 8 {
		return nil, fmt.Errorf("beacon path too long: %d hops (AX.25 max 8)", len(out))
	}
	return out, nil
}

// parseBeaconsForm reads the indexed beacon form fields and returns the
// resulting []state.Beacon plus any validation errors.
// Form layout: beacon_count = N, then per row beacon_<i>_{name,symbol,
// comment,messages,dest,path,every_min,enabled,remove}.
func parseBeaconsForm(r interface {
	FormValue(string) string
}) ([]state.Beacon, []string) {
	var errs []string
	n, _ := strconv.Atoi(r.FormValue("beacon_count"))
	if n < 0 || n > 32 {
		return nil, []string{"too many beacons"}
	}
	out := make([]state.Beacon, 0, n)
	seenNames := map[string]bool{}
	for i := 0; i < n; i++ {
		if r.FormValue(fmt.Sprintf("beacon_%d_remove", i)) == "1" {
			continue
		}
		name := strings.TrimSpace(r.FormValue(fmt.Sprintf("beacon_%d_name", i)))
		symbol := strings.TrimSpace(r.FormValue(fmt.Sprintf("beacon_%d_symbol", i)))
		// "custom" in the dropdown unlocks the adjacent _symbol_custom input
		// — let that override the canned value if the operator typed one.
		if symbol == "custom" {
			symbol = strings.TrimSpace(r.FormValue(fmt.Sprintf("beacon_%d_symbol_custom", i)))
		}
		comment := sanitizeBeaconComment(r.FormValue(fmt.Sprintf("beacon_%d_comment", i)))
		messages := r.FormValue(fmt.Sprintf("beacon_%d_messages", i)) == "1"
		// Tocall is the software identifier — always force the project default;
		// not an operator choice. Advanced users who really want to ship a
		// different tocall can edit state.json directly.
		dest := state.DefaultBeaconDest
		path := r.FormValue(fmt.Sprintf("beacon_%d_path", i))
		everyMin, _ := strconv.Atoi(r.FormValue(fmt.Sprintf("beacon_%d_every_min", i)))
		enabled := r.FormValue(fmt.Sprintf("beacon_%d_enabled", i)) == "1"
		ambig, _ := strconv.Atoi(r.FormValue(fmt.Sprintf("beacon_%d_ambiguity", i)))
		if ambig < 0 {
			ambig = 0
		}
		if ambig > 4 {
			ambig = 4
		}
		// Optional per-beacon callsign override — empty means "use station's"
		var beaconCall string
		if raw := strings.TrimSpace(r.FormValue(fmt.Sprintf("beacon_%d_callsign", i))); raw != "" {
			v, err := validateCallsign(raw)
			if err != nil {
				errs = append(errs, fmt.Sprintf("beacon[%d] callsign: %s", i, err.Error()))
				continue
			}
			beaconCall = v
		}
		// Symbol must be 2 chars: table/overlay + symbol code, both printable ASCII.
		if len(symbol) != 2 || symbol[0] < 0x21 || symbol[0] > 0x7e || symbol[1] < 0x21 || symbol[1] > 0x7e {
			errs = append(errs, fmt.Sprintf("beacon[%d] symbol: must be exactly 2 ASCII chars (e.g. \"I&\" for Tx-iGate)", i))
			continue
		}
		validatedPath, err := validateBeaconPath(path)
		if err != nil {
			errs = append(errs, fmt.Sprintf("beacon[%d] path: %s", i, err.Error()))
			continue
		}
		if name == "" {
			name = fmt.Sprintf("beacon%d", i)
		}
		if seenNames[name] {
			errs = append(errs, fmt.Sprintf("duplicate beacon name %q", name))
			continue
		}
		seenNames[name] = true
		// Clamp interval to 10..1440 minutes.
		if everyMin < 10 {
			everyMin = 10
		}
		if everyMin > 1440 {
			everyMin = 1440
		}
		out = append(out, state.Beacon{
			Name:           name,
			Symbol:         symbol,
			Comment:        comment,
			Messages:       messages,
			Dest:           dest,
			Path:           validatedPath,
			EveryS:         everyMin * 60,
			Enabled:        enabled,
			Callsign:       beaconCall,
			AmbiguityLevel: ambig,
		})
	}
	return out, errs
}

// parseWebhooksForm rebuilds the []state.Webhook list from the indexed form
// fields (webhook_<i>_*) posted by the Settings → Webhooks form. Mirrors
// parseBeaconsForm: a removed or fully-empty row is dropped; per-row errors
// are accumulated so the operator can fix them in one pass. Header values are
// preserved verbatim (the form round-trips them in a password input).
func parseWebhooksForm(r interface {
	FormValue(string) string
}) ([]state.Webhook, []string) {
	var errs []string
	n, _ := strconv.Atoi(r.FormValue("webhook_count"))
	if n < 0 || n > 64 {
		return nil, []string{"too many webhooks"}
	}
	out := make([]state.Webhook, 0, n)
	seen := map[string]bool{}
	for i := 0; i < n; i++ {
		if r.FormValue(fmt.Sprintf("webhook_%d_remove", i)) == "1" {
			continue
		}
		name := strings.TrimSpace(r.FormValue(fmt.Sprintf("webhook_%d_name", i)))
		urlStr := strings.TrimSpace(r.FormValue(fmt.Sprintf("webhook_%d_url", i)))
		// Empty row (operator added then abandoned it): drop silently.
		if name == "" && urlStr == "" {
			continue
		}
		if name == "" {
			errs = append(errs, fmt.Sprintf("webhook[%d]: name required", i))
			continue
		}
		key := strings.ToLower(name)
		if seen[key] {
			errs = append(errs, fmt.Sprintf("duplicate webhook name %q", name))
			continue
		}
		if err := validateWebhookURL(urlStr); err != nil {
			errs = append(errs, fmt.Sprintf("webhook %q: %s", name, err.Error()))
			continue
		}
		seen[key] = true

		source := r.FormValue(fmt.Sprintf("webhook_%d_source", i))
		switch source {
		case "rf", "is", "both":
		default:
			source = "both"
		}
		var types []string
		for _, t := range webhook.Types {
			if r.FormValue(fmt.Sprintf("webhook_%d_type_%s", i, t)) == "1" {
				types = append(types, t)
			}
		}
		matchMode := r.FormValue(fmt.Sprintf("webhook_%d_match_mode", i))
		if matchMode != "equals" {
			matchMode = "contains"
		}
		out = append(out, state.Webhook{
			Name:            name,
			URL:             urlStr,
			Enabled:         r.FormValue(fmt.Sprintf("webhook_%d_enabled", i)) == "1",
			Source:          source,
			IncludeTX:       r.FormValue(fmt.Sprintf("webhook_%d_include_tx", i)) == "1",
			Types:           types,
			Callsigns:       splitCSV(strings.ToUpper(r.FormValue(fmt.Sprintf("webhook_%d_callsigns", i)))),
			ToCallsigns:     splitCSV(strings.ToUpper(r.FormValue(fmt.Sprintf("webhook_%d_to_callsigns", i)))),
			MatchMode:       matchMode,
			MatchText:       strings.TrimSpace(r.FormValue(fmt.Sprintf("webhook_%d_match_text", i))),
			MatchCase:       r.FormValue(fmt.Sprintf("webhook_%d_match_case", i)) == "1",
			HeaderName:      strings.TrimSpace(r.FormValue(fmt.Sprintf("webhook_%d_header_name", i))),
			HeaderValue:     strings.TrimSpace(r.FormValue(fmt.Sprintf("webhook_%d_header_value", i))),
			InsecureSkipTLS: r.FormValue(fmt.Sprintf("webhook_%d_insecure", i)) == "1",
		})
	}
	return out, errs
}

// validateWebhookURL requires a syntactically valid absolute http(s) URL with
// a host. We deliberately do NOT block loopback / private addresses: the
// operator is the authenticated admin and a LAN receiver (e.g. Home Assistant
// on the same network) is the primary use case, not an SSRF threat.
func validateWebhookURL(s string) error {
	if s == "" {
		return errors.New("URL required")
	}
	u, err := url.Parse(s)
	if err != nil {
		return fmt.Errorf("invalid URL: %v", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("URL must start with http:// or https://")
	}
	if u.Host == "" {
		return errors.New("URL is missing a host")
	}
	return nil
}

// isFilterTermRE matches one APRS-IS server-filter term. The grammar is
// permissive on purpose: aprs-is.net/javaprsfilter.aspx lists ~15 letter
// codes (r, m, p, b, o, t, s, d, e, f, a, q, u, g) plus an optional `-`
// exclusion prefix. Rather than embedding the full catalog we accept any
// single letter followed by `/` then one-or-more args separated by `/`,
// each arg containing the usual safe character set (no spaces, no
// control bytes, no `|`/`~` which APRS reserves).
//
// Examples accepted:
//
//	r/30.27/-97.74/100
//	-t/pwntso
//	b/N0CALL*/W1AW
//	a/45.0/-93.0/40.0/-90.0
//	q/CR
//
// Rejected: empty terms, terms missing the letter or the slash, args
// containing spaces / control chars / reserved bytes.
var isFilterTermRE = regexp.MustCompile(`^-?[a-zA-Z]/[^|~\s/\x00-\x1F\x7F]+(?:/[^|~\s/\x00-\x1F\x7F]+)*$`)

// ValidateISFilter parses the operator-supplied APRS-IS server-side filter
// per aprs-is.net/javaprsfilter.aspx. Empty is allowed (means "no filter",
// which the IS server interprets as the bare unfiltered firehose — fine
// for a small minority of operators). Non-empty must split on whitespace
// into one or more terms, each matching isFilterTermRE.
//
// Caller responsibility on error: fall back to a known-good default like
// `r/<lat>/<lon>/<radius> -t/pwntso`, not silently accept garbage —
// APRS-IS will return the empty firehose for a malformed filter and the
// operator will think "filter does nothing."
func ValidateISFilter(s string) error {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	for _, term := range strings.Fields(s) {
		if !isFilterTermRE.MatchString(term) {
			return fmt.Errorf("invalid APRS-IS filter term %q (see aprs-is.net/javaprsfilter.aspx)", term)
		}
	}
	return nil
}

// defaultISFilter builds the known-good fallback filter used when the
// operator's typed filter fails ValidateISFilter. Mirrors the form the
// wizard constructs after the location step (`r/<lat>/<lon>/<km>` plus
// the iGate `-t/pwntso` exclusion to drop the position/weather/telemetry
// firehose). Caller passes mode-appropriate values; for ModeIS pass an
// empty excludeTypes to keep positions+weather visible on the map.
func defaultISFilter(lat, lon float64, km int, excludeTypes string) string {
	if km <= 0 {
		km = 150
	}
	base := fmt.Sprintf("r/%.2f/%.2f/%d", lat, lon, km)
	if excludeTypes == "" {
		return base
	}
	return base + " -t/" + excludeTypes
}

// validatePasscode accepts -1 (RX-only) or a decimal numeric passcode.
func validatePasscode(s string) error {
	s = strings.TrimSpace(s)
	if s == "" || s == "-1" {
		return nil
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return fmt.Errorf("passcode must be numeric (or -1 for RX-only)")
		}
	}
	return nil
}

// validateCallsign returns the uppercased callsign-SSID if valid, else error.
func validateCallsign(s string) (string, error) {
	s = strings.ToUpper(strings.TrimSpace(s))
	if !callsignRE.MatchString(s) {
		return "", errors.New("callsign must match [A-Z0-9]{1,6}(-NN)?")
	}
	return s, nil
}

// validateNext rejects open-redirect attempts. Accepts only same-origin
// absolute paths that don't start with `//` or `/\`.
func validateNext(next string) string {
	if next == "" || !strings.HasPrefix(next, "/") {
		return "/"
	}
	if strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") {
		return "/"
	}
	return next
}

// CSRF: synchronizer-token pattern bound to session cookie value via HMAC.
// Token is the base64(HMAC-SHA256(sessionKey, cookieValue)). The server signs
// with config.SessionKey so the token is unforgeable without that key.

// csrfTokenFor returns the canonical token for the given session cookie value.
func (s *Server) csrfTokenFor(cookieValue string) string {
	m := hmac.New(sha256.New, s.config.SessionKey())
	_, _ = m.Write([]byte("csrf:" + cookieValue))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// requireCSRF enforces:
//   - Method must be one of the mutating methods (caller already checked POST).
//   - Origin (or Referer) header must match the request Host.
//   - Form field `csrf_token` must equal csrfTokenFor(session cookie value).
//
// On failure, writes 403 and returns false. Caller should return without
// further side effects.
func (s *Server) requireCSRF(w http.ResponseWriter, r *http.Request) bool {
	cookie, err := r.Cookie("aprgo_session")
	if err != nil {
		http.Error(w, "no session", http.StatusForbidden)
		return false
	}
	// Origin/Referer check: must match the Host header we're being reached at.
	if !sameOriginAsHost(r) {
		http.Error(w, "csrf: bad origin", http.StatusForbidden)
		return false
	}
	want := s.csrfTokenFor(cookie.Value)
	got := r.FormValue("csrf_token")
	if got == "" {
		// HTMX prefers headers; allow either.
		got = r.Header.Get("X-CSRF-Token")
	}
	if !hmac.Equal([]byte(got), []byte(want)) {
		http.Error(w, "csrf: bad token", http.StatusForbidden)
		return false
	}
	return true
}

func sameOriginAsHost(r *http.Request) bool {
	host := r.Host
	if origin := r.Header.Get("Origin"); origin != "" {
		return originHostMatches(origin, host)
	}
	if ref := r.Header.Get("Referer"); ref != "" {
		return originHostMatches(ref, host)
	}
	// No Origin or Referer — for our trust model (LAN admin console), refuse.
	return false
}

func originHostMatches(originOrRef, host string) bool {
	// originOrRef is like "http://10.0.0.5:14439" or "http://10.0.0.5:14439/foo".
	o := originOrRef
	if i := strings.Index(o, "://"); i >= 0 {
		o = o[i+3:]
	}
	if i := strings.Index(o, "/"); i >= 0 {
		o = o[:i]
	}
	return strings.EqualFold(o, host)
}
