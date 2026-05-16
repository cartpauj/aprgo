package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"aprgo/internal/state"
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

// callsignRE matches AX.25 + APRS callsign-SSID format: 1-6 alphanumeric
// followed by optional -SSID where SSID is 0-15 (the AX.25 spec limit:
// the SSID byte has 4 bits for the number).
var callsignRE = regexp.MustCompile(`^[A-Z0-9]{1,6}(-([0-9]|1[0-5]))?$`)

// wideRE matches the legal WIDEn-N forms used as digipeater path tokens.
// n must be 1 or 2 and N must be 0..n per the APRS New-N paradigm.
var wideRE = regexp.MustCompile(`^WIDE([12])-([012])$`)

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

// validateBeaconText ensures the info field starts with a valid APRS data-type
// indicator. Accepted: `!` `=` `/` `@` (position with/without timestamp and
// with/without messaging), or empty (no beacon).
func validateBeaconText(s string) error {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	switch s[0] {
	case '!', '=', '/', '@':
		return nil
	}
	return fmt.Errorf("beacon info field must start with !, =, /, or @ (APRS position data-type indicator)")
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
		comment := sanitizeAPRSField(r.FormValue(fmt.Sprintf("beacon_%d_comment", i)))
		messages := r.FormValue(fmt.Sprintf("beacon_%d_messages", i)) == "1"
		// Tocall is the software identifier — always force the project default;
		// not an operator choice. Advanced users who really want to ship a
		// different tocall can edit state.json directly.
		dest := state.DefaultBeaconDest
		path := r.FormValue(fmt.Sprintf("beacon_%d_path", i))
		everyMin, _ := strconv.Atoi(r.FormValue(fmt.Sprintf("beacon_%d_every_min", i)))
		enabled := r.FormValue(fmt.Sprintf("beacon_%d_enabled", i)) == "1"
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
			Name:     name,
			Symbol:   symbol,
			Comment:  comment,
			Messages: messages,
			Dest:     dest,
			Path:     validatedPath,
			EveryS:   everyMin * 60,
			Enabled:  enabled,
			Callsign: beaconCall,
		})
	}
	return out, errs
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
// with state.SessionKey so the token is unforgeable without that key.

// csrfTokenFor returns the canonical token for the given session cookie value.
func (s *Server) csrfTokenFor(cookieValue string) string {
	m := hmac.New(sha256.New, s.state.SessionKey())
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
