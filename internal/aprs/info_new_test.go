package aprs

import "testing"

// Tests for parser features added in the post-45-item second-pass audit.
// These cover: positionless WX, compressed-position cs/T altitude, Mic-E
// `xxx}` altitude, base-91 comment telemetry, NMEA `$GP*` decode, item
// name length enforcement, bulletin group detection, ambiguity surfacing,
// Mic-E fix-old indicator.

func TestPositionlessWeather(t *testing.T) {
	// Positionless WX form: `_MMDDHHMM<fields>`. Parser strips the 8-byte
	// timestamp and runs the field regex on the rest. Wind in positionless
	// form uses `cNNN/sNNN` per APRS101 §12.5 (slash form, like position+wx).
	d := Decode("_05151200180/005g010t072r000P000h54b10172", "")
	if d.Weather == nil {
		t.Fatal("expected weather parsed from positionless wx")
	}
	if !d.Weather.WindDirSet || d.Weather.WindDirDeg != 180 {
		t.Errorf("wind dir: got set=%v deg=%d, want 180", d.Weather.WindDirSet, d.Weather.WindDirDeg)
	}
	if !d.Weather.TempSet || d.Weather.TempF != 72 {
		t.Errorf("temp: got set=%v F=%d, want 72", d.Weather.TempSet, d.Weather.TempF)
	}
}

func TestItemNameLengthEnforced(t *testing.T) {
	// 2-char name "X" is too short (spec requires 3-9). Position should not parse.
	d := Decode(")X !4903.50N/07201.75W>", "")
	if d.Lat != nil {
		t.Error("item with 1-char name should be rejected; got lat parsed")
	}
	// Valid 3-char name should parse.
	d = Decode(")XYZ!4903.50N/07201.75W>", "")
	if d.Lat == nil {
		t.Error("item with 3-char name should parse")
	}
}

func TestBulletinGroupParsing(t *testing.T) {
	// BLN1ARES: numbered bulletin "1" for group "ARES"
	d := Decode(":BLN1ARES :net tonight 7pm", "")
	if d.BulletinGroup != "ARES" {
		t.Errorf("BulletinGroup = %q, want ARES", d.BulletinGroup)
	}
	// BLNAWX: announcement "A" for group "WX"
	d = Decode(":BLNAWX   :hurricane warning", "")
	if d.BulletinGroup != "WX" {
		t.Errorf("BulletinGroup = %q, want WX", d.BulletinGroup)
	}
	// BLN1: no group
	d = Decode(":BLN1     :hello world", "")
	if d.BulletinGroup != "" {
		t.Errorf("BulletinGroup = %q, want empty for groupless BLN1", d.BulletinGroup)
	}
}

func TestNMEAGPRMC(t *testing.T) {
	d := Decode("$GPRMC,123519,A,4807.038,N,01131.000,E,022.4,084.4,230394,003.1,W*6A", "")
	if d.Lat == nil || d.Lon == nil {
		t.Fatal("RMC should parse lat/lon")
	}
	if *d.Lat < 48.11 || *d.Lat > 48.13 {
		t.Errorf("lat = %f, want ~48.1173", *d.Lat)
	}
	if *d.Lon < 11.51 || *d.Lon > 11.53 {
		t.Errorf("lon = %f, want ~11.5167", *d.Lon)
	}
	if d.Course != 84 {
		t.Errorf("course = %d, want 84", d.Course)
	}
}

func TestNMEAGPGGA(t *testing.T) {
	d := Decode("$GPGGA,123519,4807.038,N,01131.000,E,1,08,0.9,545.4,M,46.9,M,,*47", "")
	if d.Lat == nil || d.Lon == nil {
		t.Fatal("GGA should parse lat/lon")
	}
	if d.Altitude < 1700 || d.Altitude > 1900 {
		t.Errorf("altitude = %d, want ~1789 ft (545.4 m)", d.Altitude)
	}
}

func TestNMEANoFix(t *testing.T) {
	// RMC with status='V' (no fix) — must NOT set lat/lon.
	d := Decode("$GPRMC,123519,V,4807.038,N,01131.000,E,0,0,230394,,*7E", "")
	if d.Lat != nil || d.Lon != nil {
		t.Error("RMC with status V (no fix) should not produce position")
	}
}

func TestLooksLikeTimestamp(t *testing.T) {
	cases := map[string]bool{
		"092345z": true,
		"092345h": true,
		"092345/": true,
		"092345a": false, // bad suffix
		"abcdef z": false, // wrong length
		"12345z":  false, // 5 digits + z = 6 chars (too short)
		"AAAAAAz": false, // non-digit
	}
	for s, want := range cases {
		if got := looksLikeTimestamp(s); got != want {
			t.Errorf("looksLikeTimestamp(%q) = %v, want %v", s, got, want)
		}
	}
}

func TestNMEAChecksumReject(t *testing.T) {
	// Same as TestNMEAGPRMC but with a corrupted checksum (last hex char
	// flipped). Should be rejected — no lat/lon set.
	d := Decode("$GPRMC,123519,A,4807.038,N,01131.000,E,022.4,084.4,230394,003.1,W*6B", "")
	if d.Lat != nil || d.Lon != nil {
		t.Error("RMC with corrupted checksum should not produce position")
	}
}

func TestNMEAChecksumMissing(t *testing.T) {
	// Per APRS101 §5 the checksum is optional; a sentence without `*XX`
	// should still parse.
	d := Decode("$GPRMC,123519,A,4807.038,N,01131.000,E,022.4,084.4,230394", "")
	if d.Lat == nil {
		t.Error("RMC without checksum should still parse")
	}
}

func TestDAOUppercase(t *testing.T) {
	// !W00! means WGS84, no extra precision (both digits = 0). Position
	// stays the same as without DAO.
	d1 := Decode("!4903.50N/07201.75W>Test", "")
	d2 := Decode("!4903.50N/07201.75W>Test !W00!", "")
	if d1.Lat == nil || d2.Lat == nil {
		t.Fatal("expected positions to decode")
	}
	if *d1.Lat != *d2.Lat || *d1.Lon != *d2.Lon {
		t.Errorf("!W00! shouldn't shift position: %f,%f vs %f,%f", *d1.Lat, *d1.Lon, *d2.Lat, *d2.Lon)
	}
	// !W55! should add 0.5/100 minute = 0.005 min to both lat and lon.
	// 0.005 min in degrees = 0.005/60 ≈ 8.33e-5°.
	d3 := Decode("!4903.50N/07201.75W>!W55!", "")
	if d3.Lat == nil {
		t.Fatal("DAO packet should still decode position")
	}
	delta := *d3.Lat - *d1.Lat
	if delta < 7e-5 || delta > 1e-4 {
		t.Errorf("!W55! lat delta = %g, want ~8.3e-5", delta)
	}
}

func TestDAOStripped(t *testing.T) {
	// !DAO! marker should be removed from the comment after parsing.
	d := Decode("!4903.50N/07201.75W>hello !W55! world", "")
	if d.Comment == "" || (d.Comment != "hello  world" && d.Comment != "hello world") {
		t.Errorf("comment after DAO strip = %q; want DAO removed", d.Comment)
	}
}

func TestLegacyGPSDTIIgnored(t *testing.T) {
	// /1<NMEA-ish data>: pre-1.0 GPS-port passthrough. Spec says
	// "reserved, do not transmit"; we accept-and-ignore.
	d := Decode("/1XGGA,123519,4807.038,N,01131.000,E", "")
	if d.Lat != nil {
		t.Error("legacy /1 DTI should not produce position")
	}
}
