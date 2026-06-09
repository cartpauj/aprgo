package gps

import (
	"math"
	"testing"
)

func TestValidChecksum(t *testing.T) {
	cases := []struct {
		line string
		want bool
	}{
		// Real sentences captured from a u-blox 7 on /dev/ttyACM0.
		{"$GPTXT,01,01,02,u-blox ag - www.u-blox.com*50", true},
		{"$GPRMC,174141.00,V,,,,,,,090626,,,N*70", true},
		{"$GPGGA,174141.00,,,,,0,04,4.67,,,,,,*51", true},
		{"$GPGSA,A,1,23,25,15,20,,,,,,,,,11.17,4.67,10.15*06", true},
		// Multi-GNSS talker — must still validate.
		{"$GNRMC,123519.00,A,4807.038,N,01131.000,E,0.06,31.66,230394,,,A*6F", false}, // deliberately wrong cksum
		// Corrupted checksum.
		{"$GPGGA,174141.00,,,,,0,04,4.67,,,,,,*52", false},
		// Garbage / serial noise.
		{"random serial noise", false},
		{"", false},
		{"$", false},
	}
	for _, c := range cases {
		if got := validChecksum(c.line); got != c.want {
			t.Errorf("validChecksum(%q) = %v, want %v", c.line, got, c.want)
		}
	}
}

func TestSentenceTypeTalkerAgnostic(t *testing.T) {
	cases := map[string]string{
		"$GPRMC,...":  "RMC",
		"$GNRMC,...":  "RMC",
		"$GLGSV,...":  "GSV",
		"$GAGGA,...":  "GGA",
		"$GBGGA,...":  "GGA",
		"$GPTXT,...":  "TXT",
		"$PMTK001*..": "", // proprietary, ignored
		"garbage":     "",
	}
	for line, want := range cases {
		if got := sentenceType(line); got != want {
			t.Errorf("sentenceType(%q) = %q, want %q", line, got, want)
		}
	}
}

func TestParseCoord(t *testing.T) {
	cases := []struct {
		val, hemi string
		deg       int
		want      float64
	}{
		{"4404.13993", "N", 2, 44.068999},
		{"12118.86023", "W", 3, -121.314337},
		{"4807.038", "N", 2, 48.1173},
		{"01131.000", "E", 3, 11.516667},
	}
	for _, c := range cases {
		got, ok := parseCoord(c.val, c.hemi, c.deg)
		if !ok {
			t.Errorf("parseCoord(%q,%q) failed", c.val, c.hemi)
			continue
		}
		if math.Abs(got-c.want) > 1e-5 {
			t.Errorf("parseCoord(%q,%q) = %.6f, want %.6f", c.val, c.hemi, got, c.want)
		}
	}
}

// TestParseSentenceRealFixtures uses fixtures with verified checksums and
// asserts the fix-detection semantics: a checksum-valid sentence with no lock
// must parse but report no position / invalid status.
func TestParseSentenceRealFixtures(t *testing.T) {
	// No-fix RMC (status V) — parses, but valid=false and no position.
	d, ok := parseSentence("$GPRMC,174141.00,V,,,,,,,090626,,,N*70")
	if !ok {
		t.Fatal("RMC no-fix should parse (valid checksum + known type)")
	}
	if d.typ != "RMC" || !d.haveStatus || d.valid || d.havePos {
		t.Errorf("RMC no-fix: got %+v, want valid=false havePos=false", d)
	}

	// No-fix GGA (quality 0) — parses, quality 0, sats present, no position.
	d, ok = parseSentence("$GPGGA,174141.00,,,,,0,04,4.67,,,,,,*51")
	if !ok {
		t.Fatal("GGA no-fix should parse")
	}
	if !d.haveQuality || d.quality != 0 || d.havePos {
		t.Errorf("GGA no-fix: got %+v, want quality=0 havePos=false", d)
	}
	if !d.haveSats || d.sats != 4 {
		t.Errorf("GGA sats = %d, want 4", d.sats)
	}
	if !d.haveHDOP || math.Abs(d.hdop-4.67) > 1e-6 {
		t.Errorf("GGA hdop = %v, want 4.67", d.hdop)
	}

	// TXT banner — recognised (counts as GPS evidence) but carries no fields.
	if _, ok := parseSentence("$GPTXT,01,01,02,u-blox ag - www.u-blox.com*50"); !ok {
		t.Error("GPTXT banner should be recognised for detection")
	}

	// Wrong checksum — rejected entirely.
	if _, ok := parseSentence("$GPGGA,174141.00,,,,,0,04,4.67,,,,,,*52"); ok {
		t.Error("bad-checksum line must be rejected")
	}
}

func TestParseGGAWithFix(t *testing.T) {
	// Build a GGA with a real lock and a correct checksum.
	line := withChecksum("GPGGA,123519,4807.038,N,01131.000,E,1,08,0.9,545.4,M,46.9,M,,")
	d, ok := parseSentence(line)
	if !ok {
		t.Fatalf("fixture %q failed to parse", line)
	}
	if !d.havePos {
		t.Fatal("GGA with lock should have position")
	}
	if math.Abs(d.lat-48.1173) > 1e-4 || math.Abs(d.lon-11.51667) > 1e-4 {
		t.Errorf("pos = %.5f,%.5f want 48.1173,11.51667", d.lat, d.lon)
	}
	if d.quality != 1 || d.sats != 8 {
		t.Errorf("quality/sats = %d/%d want 1/8", d.quality, d.sats)
	}
}

func TestParseRMCWithFix(t *testing.T) {
	line := withChecksum("GNRMC,123519,A,4807.038,N,01131.000,W,22.4,84.4,230394,,")
	d, ok := parseSentence(line)
	if !ok {
		t.Fatalf("fixture %q failed to parse", line)
	}
	if !d.valid || !d.havePos {
		t.Fatal("RMC with lock should be valid + have position")
	}
	if d.lon > 0 {
		t.Errorf("W longitude should be negative, got %.5f", d.lon)
	}
	if !d.haveSpeed || math.Abs(d.speedKnots-22.4) > 1e-6 {
		t.Errorf("speed = %v want 22.4 kn", d.speedKnots)
	}
}

// withChecksum appends the correct *HH checksum to a bare NMEA body (the part
// between '$' and '*') so test fixtures don't need hand-computed checksums.
func withChecksum(body string) string {
	var cs byte
	for i := 0; i < len(body); i++ {
		cs ^= body[i]
	}
	const hex = "0123456789ABCDEF"
	return "$" + body + "*" + string([]byte{hex[cs>>4], hex[cs&0xF]})
}
