package aprs

import (
	"regexp"
	"strconv"
)

// Weather data lifted from a positional weather report. APRS spec §12
// (PROTOCOL.TXT, spec-wx.txt). All fields are optional; check the *Set
// bool flags to distinguish "not reported" from "reported as zero."
//
// Encoding conventions:
//   CCC/SSS    = wind direction (deg) / sustained speed (mph) — position
//                packets use this in place of course/speed when the symbol
//                code is '_' (weather station).
//   gNNN       = peak gust last 5 min, mph
//   tNNN       = temperature in °F (can be negative; APRS uses 0-prefixed
//                3-digit ASCII so "-12" arrives as "-12" or "t-12" in the
//                comment — handled below)
//   rNNN       = rain last 60 min, hundredths of inch
//   pNNN       = rain last 24 hr (sliding window), hundredths of inch
//   PNNN       = rain since local midnight, hundredths of inch
//   hNN        = humidity %; "00" means 100%
//   bNNNNN     = barometric pressure in tenths of millibar
//   lNNN/LNNN  = luminosity (W/m²), L=light, l=high-light (≥1000)
//   sNN       = snowfall, inches per 24 hr (rarely used)
type Weather struct {
	WindDirDeg     int  // 0..359
	WindDirSet     bool
	WindSpeedMPH   int  // sustained 1-min
	WindSpeedSet   bool
	WindGustMPH    int  // peak last 5 min
	WindGustSet    bool
	TempF          int  // can be negative
	TempSet        bool
	Rain1hHundIn   int  // hundredths-inch
	Rain1hSet      bool
	Rain24hHundIn  int
	Rain24hSet     bool
	RainMidHundIn  int
	RainMidSet     bool
	HumidityPct    int  // 1..100 (00 in wire format means 100)
	HumiditySet    bool
	PressureTenthMb int // tenths-of-mb (10153 == 1015.3)
	PressureSet    bool
	LuminosityWm2  int  // W/m²
	LuminositySet  bool
}

// Wind direction/speed regex: NNN/NNN at the start of the data after the
// symbol. We accept a leading "_" or "/" or absence (Mic-E weather reports
// can have CSE/SPD at the start with no leader).
var (
	// CCC/SSS at the start (or somewhere reasonable). For "_" weather
	// reports the wind tuple is the first 7 chars of the comment.
	weatherWindRE = regexp.MustCompile(`(\d{3})/(\d{3})`)
	weatherFieldRE = regexp.MustCompile(`([gtrpPhbslLs])(-?\d{2,5})`)
)

// parseWeather scans a position-comment fragment for weather fields. Returns
// (nil, "") if no weather data is detected. Returns (parsed, stripped) where
// stripped is the comment with the weather record removed so the UI doesn't
// double-display.
//
// Detection rule: we require at least 2 weather fields OR (1 weather field
// AND the wind CCC/SSS tuple) before considering it weather. This keeps a
// bare "g123" in a comment from being mistaken for a wind gust.
func parseWeather(s string) (*Weather, string) {
	w := &Weather{}
	matches := weatherFieldRE.FindAllStringSubmatchIndex(s, -1)
	count := 0
	for _, m := range matches {
		key := s[m[2]:m[3]]
		valStr := s[m[4]:m[5]]
		val, err := strconv.Atoi(valStr)
		if err != nil {
			continue
		}
		switch key {
		case "g":
			if len(valStr) == 3 {
				w.WindGustMPH = val
				w.WindGustSet = true
				count++
			}
		case "t":
			// Spec: 3-digit signed. We accept 3-digit unsigned, or sign+2-3 digits.
			if (len(valStr) == 3 && valStr[0] != '-') ||
				(len(valStr) >= 3 && valStr[0] == '-') {
				w.TempF = val
				w.TempSet = true
				count++
			}
		case "r":
			if len(valStr) == 3 {
				w.Rain1hHundIn = val
				w.Rain1hSet = true
				count++
			}
		case "p":
			if len(valStr) == 3 {
				w.Rain24hHundIn = val
				w.Rain24hSet = true
				count++
			}
		case "P":
			if len(valStr) == 3 {
				w.RainMidHundIn = val
				w.RainMidSet = true
				count++
			}
		case "h":
			if len(valStr) == 2 {
				if val == 0 {
					val = 100
				}
				w.HumidityPct = val
				w.HumiditySet = true
				count++
			}
		case "b":
			if len(valStr) == 5 {
				w.PressureTenthMb = val
				w.PressureSet = true
				count++
			}
		case "l":
			if len(valStr) == 3 {
				w.LuminosityWm2 = val
				w.LuminositySet = true
				count++
			}
		case "L":
			if len(valStr) == 3 {
				w.LuminosityWm2 = val + 1000
				w.LuminositySet = true
				count++
			}
		}
	}
	// Wind CCC/SSS is matched as a separate regex because the field key is
	// implicit (positional). Only honored when at least one other weather
	// field was present in the same string, so a bare "012/345" in a
	// comment doesn't get mis-tagged as wind.
	if count >= 1 {
		if m := weatherWindRE.FindStringSubmatchIndex(s); m != nil {
			dir, _ := strconv.Atoi(s[m[2]:m[3]])
			spd, _ := strconv.Atoi(s[m[4]:m[5]])
			if dir <= 360 && spd <= 999 {
				w.WindDirDeg = dir
				w.WindSpeedMPH = spd
				w.WindDirSet = true
				w.WindSpeedSet = true
			}
		}
	}
	if count < 2 && !(count == 1 && w.WindDirSet) {
		return nil, s
	}
	// Strip matched weather fragments from the comment. We rebuild the
	// stripped string by walking the original and skipping the matched
	// indices from BOTH the field regex and the wind tuple regex.
	type span struct{ lo, hi int }
	var spans []span
	for _, m := range matches {
		spans = append(spans, span{m[0], m[1]})
	}
	if w.WindDirSet {
		if m := weatherWindRE.FindStringSubmatchIndex(s); m != nil {
			spans = append(spans, span{m[0], m[1]})
		}
	}
	// Insertion sort by start offset; spans count is tiny (≤10).
	for i := 1; i < len(spans); i++ {
		for j := i; j > 0 && spans[j-1].lo > spans[j].lo; j-- {
			spans[j-1], spans[j] = spans[j], spans[j-1]
		}
	}
	var sb []byte
	cursor := 0
	for _, sp := range spans {
		if sp.lo < cursor {
			continue
		}
		sb = append(sb, s[cursor:sp.lo]...)
		cursor = sp.hi
	}
	sb = append(sb, s[cursor:]...)
	stripped := cleanupSpaces(string(sb))
	return w, stripped
}

// cleanupSpaces collapses multiple spaces and trims edges. Used after
// stripping matched fragments from a comment so we don't leave "  foo "
// gaps.
func cleanupSpaces(s string) string {
	// Cheap collapse: replace tabs+CR+LF with space, dedupe spaces, trim.
	out := make([]byte, 0, len(s))
	prevSpace := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\t' || c == '\r' || c == '\n' {
			c = ' '
		}
		if c == ' ' {
			if prevSpace {
				continue
			}
			prevSpace = true
		} else {
			prevSpace = false
		}
		out = append(out, c)
	}
	// Trim leading/trailing spaces.
	lo, hi := 0, len(out)
	for lo < hi && out[lo] == ' ' {
		lo++
	}
	for hi > lo && out[hi-1] == ' ' {
		hi--
	}
	return string(out[lo:hi])
}
