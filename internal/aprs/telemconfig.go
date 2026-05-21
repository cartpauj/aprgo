package aprs

import (
	"strconv"
	"strings"
)

// TelemConfig is one of four APRS telemetry-configuration messages per
// APRS101 §13.4. The receiver applies these to subsequent T# packets
// from the same station so generic "Analog 0..4" data becomes labeled
// channels (e.g. "Battery V", "Temp F").
//
// Format:
//   PARM.P1,P2,P3,P4,P5,B1,B2,B3,B4,B5,B6,B7,B8
//   UNIT.U1,U2,U3,U4,U5,L1,L2,L3,L4,L5,L6,L7,L8
//   EQNS.A1,B1,C1,A2,B2,C2,A3,B3,C3,A4,B4,C4,A5,B5,C5
//   BITS.XXXXXXXX,Title (≤23 chars)
//
// Only the fields matching Kind are populated; the others retain zero
// values. Callers should check Kind before reading.
type TelemConfig struct {
	Kind       string       // "parm" | "unit" | "eqns" | "bits"
	ParamNames [13]string   // PARM: 5 analog + 8 digital labels
	UnitNames  [13]string   // UNIT: 5 analog units + 8 digital labels
	Coeffs     [5][3]float64 // EQNS: A, B, C per analog channel
	Sense      [8]bool      // BITS: active state per digital channel
	Title      string       // BITS: optional project title
}

// Apply transforms the 5 raw analog telemetry values into their
// engineering-unit equivalents using the EQNS coefficients (y = A*x² +
// B*x + C). A channel with all-zero coefficients is treated as identity
// (pass-through) so undefined channels still render sensibly.
func (tc *TelemConfig) Apply(raw [5]float64) [5]float64 {
	var out [5]float64
	for i := 0; i < 5; i++ {
		a, b, c := tc.Coeffs[i][0], tc.Coeffs[i][1], tc.Coeffs[i][2]
		if a == 0 && b == 0 && c == 0 {
			out[i] = raw[i]
			continue
		}
		x := raw[i]
		out[i] = a*x*x + b*x + c
	}
	return out
}

// parseTelemConfig inspects the body of a message and, if it matches one
// of the PARM./UNIT./EQNS./BITS. prefixes, returns the parsed config.
// Returns nil for regular messages — caller falls back to normal message
// decode.
func parseTelemConfig(body string) *TelemConfig {
	switch {
	case strings.HasPrefix(body, "PARM."):
		return parseTelemNames(body[5:], "parm")
	case strings.HasPrefix(body, "UNIT."):
		return parseTelemNames(body[5:], "unit")
	case strings.HasPrefix(body, "EQNS."):
		return parseTelemEQNS(body[5:])
	case strings.HasPrefix(body, "BITS."):
		return parseTelemBITS(body[5:])
	}
	return nil
}

func parseTelemNames(rest, kind string) *TelemConfig {
	rest = strings.TrimRight(rest, "\r\n")
	tc := &TelemConfig{Kind: kind}
	parts := strings.Split(rest, ",")
	// Cap at 13 (5 analog + 8 digital); excess fields are ignored per
	// the convention used by aprs.fi, which silently drops anything past
	// the spec'd field count rather than rejecting the whole message.
	for i, p := range parts {
		if i >= 13 {
			break
		}
		if kind == "unit" {
			tc.UnitNames[i] = p
		} else {
			tc.ParamNames[i] = p
		}
	}
	return tc
}

func parseTelemEQNS(rest string) *TelemConfig {
	rest = strings.TrimRight(rest, "\r\n")
	tc := &TelemConfig{Kind: "eqns"}
	parts := strings.Split(rest, ",")
	// 3 coefficients (A, B, C) × 5 analog channels = 15 values. Real
	// stations sometimes send fewer (e.g. our KK7LLM example sends 6
	// for two channels) — accept what we get, leave the rest zero.
	for i, p := range parts {
		ch := i / 3
		coef := i % 3
		if ch >= 5 {
			break
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
		if err != nil {
			// Soft-fail per-field: a malformed number leaves that
			// coefficient at zero rather than rejecting the whole
			// config message. Matches aprslib / Ham::APRS::FAP.
			continue
		}
		tc.Coeffs[ch][coef] = v
	}
	return tc
}

func parseTelemBITS(rest string) *TelemConfig {
	rest = strings.TrimRight(rest, "\r\n")
	// Form: "XXXXXXXX" or "XXXXXXXX,Title". 8 binary chars required.
	senseStr := rest
	title := ""
	if i := strings.IndexByte(rest, ','); i >= 0 {
		senseStr = rest[:i]
		title = strings.TrimSpace(rest[i+1:])
	}
	if len(senseStr) != 8 {
		return nil
	}
	tc := &TelemConfig{Kind: "bits"}
	for i := 0; i < 8; i++ {
		switch senseStr[i] {
		case '1':
			tc.Sense[i] = true
		case '0':
			tc.Sense[i] = false
		default:
			return nil
		}
	}
	// Spec caps title at 23 chars; clamp to be safe.
	if len(title) > 23 {
		title = title[:23]
	}
	tc.Title = title
	return tc
}
