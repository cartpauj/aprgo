package aprs

import (
	"math"
	"regexp"
	"strconv"
)

// PHG (Power-Height-Gain-Direction) is a 4- or 5-digit station capability
// code embedded in position-packet comments. Lets receivers estimate the
// transmitter's expected RF coverage radius.
//
// Format: "PHGxxxx" or "PHGxxxxR" (with explicit range), where each x is
// a single digit:
//   x[0] = Power digit:     watts = digit²            (0→0W, 5→25W, 9→81W)
//   x[1] = Height digit:    HAAT  = 10 × 2^digit ft   (0→10ft, 5→320ft, 9→5120ft)
//   x[2] = Gain digit:      dB    = digit             (0→0dB, 9→9dB)
//   x[3] = Directivity:     0=omni, 1=45°, ..., 8=360° N
//
// Range estimate (aprs.org/phg.html):
//   range_miles = √(2 × HAAT_ft × √((P/10) × (gain_linear/2)))
//
// RNG (range circle) is a simpler alternative — "RNGxxxx" explicitly
// declares the expected range in miles. Tracker-style stations use RNG
// instead of PHG when they don't have antenna data to share.
type PHG struct {
	PowerW     int     // watts
	HeightFt   int     // feet HAAT
	GainDB     int     // dB
	DirDeg     int     // 0=omni; otherwise 45..360
	Omni       bool    // true if direction digit is 0
	RangeMiles float64 // computed range
}

// RNG declares an explicit range circle in miles.
type RNG struct {
	Miles int
}

var (
	// PHG is "PHG" + 4 digits. Bob Bruninga's later PHGR variant tacks
	// a 5th byte on (digit 0-9 for beacon-rate or literal "R" depending
	// on dialect). We don't decode the rate but accept the suffix so
	// the core PHG values are still extracted from PHGxxxxR / PHGxxxxx
	// frames seen in the wild.
	phgRE = regexp.MustCompile(`\bPHG(\d{4})[\dR]?\b`)
	rngRE = regexp.MustCompile(`\bRNG(\d{4})\b`)
)

func parsePHG(s string) (*PHG, string) {
	m := phgRE.FindStringSubmatchIndex(s)
	if m == nil {
		return nil, s
	}
	digits := s[m[2]:m[3]]
	p := PHG{
		PowerW:   int(digits[0]-'0') * int(digits[0]-'0'),
		HeightFt: 10 * (1 << uint(digits[1]-'0')),
		GainDB:   int(digits[2] - '0'),
	}
	dirDigit := int(digits[3] - '0')
	switch {
	case dirDigit == 0:
		p.Omni = true
	case dirDigit >= 1 && dirDigit <= 8:
		p.DirDeg = dirDigit * 45
	}
	// Range = sqrt(2 × H × sqrt((P/10) × (gain_linear/2)))
	gainLinear := math.Pow(10.0, float64(p.GainDB)/10.0)
	inner := (float64(p.PowerW) / 10.0) * (gainLinear / 2.0)
	if inner > 0 {
		p.RangeMiles = math.Sqrt(2.0 * float64(p.HeightFt) * math.Sqrt(inner))
	}
	stripped := s[:m[0]] + s[m[1]:]
	return &p, cleanupSpaces(stripped)
}

func parseRNG(s string) (*RNG, string) {
	m := rngRE.FindStringSubmatchIndex(s)
	if m == nil {
		return nil, s
	}
	miles, err := strconv.Atoi(s[m[2]:m[3]])
	if err != nil {
		return nil, s
	}
	stripped := s[:m[0]] + s[m[1]:]
	return &RNG{Miles: miles}, cleanupSpaces(stripped)
}
