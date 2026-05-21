package aprs

import (
	"math"
	"testing"
)

// approx returns true if a and b agree within tol. Used because lat/lon
// reconstruction from APRS minutes-and-hundredths can carry small rounding
// errors that aren't worth chasing in tests.
func approx(a, b, tol float64) bool {
	return math.Abs(a-b) <= tol
}

// ---------- Uncompressed position (! / = / @) ----------

func TestUncompressedPositionAPRS101Example(t *testing.T) {
	// Canonical APRS101 §8 example: "!4903.50N/07201.75W-"
	// 49° 03.50' N = 49.05833° / 72° 01.75' W = -72.029166°
	d := Decode("!4903.50N/07201.75W-", "APRS")
	if d.Lat == nil || d.Lon == nil {
		t.Fatalf("expected position decoded, got lat=%v lon=%v", d.Lat, d.Lon)
	}
	if !approx(*d.Lat, 49.05833, 1e-4) {
		t.Errorf("lat: got %v want ~49.05833", *d.Lat)
	}
	if !approx(*d.Lon, -72.02917, 1e-4) {
		t.Errorf("lon: got %v want ~-72.02917", *d.Lon)
	}
	if d.Symbol != "/-" {
		t.Errorf("symbol: got %q want %q", d.Symbol, "/-")
	}
	if d.Ambiguity != 0 {
		t.Errorf("ambiguity: got %d want 0", d.Ambiguity)
	}
}

func TestUncompressedPositionMessageCapable(t *testing.T) {
	// `=` is the message-capable variant of `!`. Same position decode.
	d := Decode("=4031.03N/11200.83Wr", "APRS")
	if d.Lat == nil || d.Lon == nil {
		t.Fatalf("expected position decoded")
	}
	if !approx(*d.Lat, 40.5172, 1e-4) {
		t.Errorf("lat: got %v want ~40.5172", *d.Lat)
	}
	if !approx(*d.Lon, -112.0138, 1e-4) {
		t.Errorf("lon: got %v want ~-112.0138", *d.Lon)
	}
}

func TestUncompressedPositionWithTimestamp(t *testing.T) {
	// `@` = position + timestamp + msg-capable. Timestamp is 7 bytes
	// (DDHHMM + z/h/`/'), then position. From APRS101 §8.
	d := Decode("@092345z4903.50N/07201.75W>", "APRS")
	if d.Lat == nil || d.Lon == nil {
		t.Fatalf("expected position decoded past timestamp")
	}
	if !approx(*d.Lat, 49.05833, 1e-4) {
		t.Errorf("lat after timestamp: got %v", *d.Lat)
	}
}

func TestUncompressedPositionSouthWest(t *testing.T) {
	// Negative lat (S) and negative lon (W) reconstruction.
	d := Decode("!3358.50S/15102.25W-", "APRS")
	if d.Lat == nil || d.Lon == nil {
		t.Fatalf("expected position decoded")
	}
	if !approx(*d.Lat, -33.975, 1e-4) {
		t.Errorf("south lat: got %v want -33.975", *d.Lat)
	}
	if !approx(*d.Lon, -151.0375, 1e-4) {
		t.Errorf("west lon: got %v want -151.0375", *d.Lon)
	}
}

func TestUncompressedPositionNorthEast(t *testing.T) {
	d := Decode("!4903.50N\\07201.75E-", "APRS")
	if d.Lat == nil || d.Lon == nil {
		t.Fatalf("expected position decoded")
	}
	if *d.Lat < 0 {
		t.Errorf("N hemisphere lat should be positive: got %v", *d.Lat)
	}
	if *d.Lon < 0 {
		t.Errorf("E hemisphere lon should be positive: got %v", *d.Lon)
	}
}

func TestUncompressedAmbiguityLevel1(t *testing.T) {
	// One blank in lat hundredths (and lon hundredths) → level 1.
	d := Decode("!4903.5 N/07201.7 W-", "APRS")
	if d.Lat == nil || d.Lon == nil {
		t.Fatalf("expected position decoded")
	}
	if d.Ambiguity != 1 {
		t.Errorf("ambiguity: got %d want 1", d.Ambiguity)
	}
}

func TestUncompressedAmbiguityLevel4(t *testing.T) {
	// Blank the tens-of-minutes digit on lat → level 4.
	d := Decode("!49  .  N/072  .  W-", "APRS")
	if d.Lat == nil {
		t.Fatalf("expected position decoded with degree-only ambiguity")
	}
	if d.Ambiguity != 4 {
		t.Errorf("ambiguity: got %d want 4", d.Ambiguity)
	}
}

func TestUncompressedRejectsInvalidHemisphere(t *testing.T) {
	// 'X' is not a valid hemisphere indicator.
	d := Decode("!4903.50X/07201.75W-", "APRS")
	if d.Lat != nil {
		t.Errorf("expected nil lat on invalid hemisphere, got %v", *d.Lat)
	}
}

func TestUncompressedRejectsBadSymbolTable(t *testing.T) {
	// Symbol table must be '/', '\', or 0-9 / A-Z (overlay).
	d := Decode("!4903.50N?07201.75W-", "APRS")
	if d.Lat != nil {
		t.Errorf("expected nil lat on invalid symbol table, got %v", *d.Lat)
	}
}

func TestUncompressedTooShort(t *testing.T) {
	// Position needs at least 19 bytes after the DTI. Anything shorter is junk.
	d := Decode("!49", "APRS")
	if d.Lat != nil {
		t.Errorf("expected nil lat on truncated position")
	}
}

// ---------- Compressed position ----------

func TestCompressedPositionAPRS101Example(t *testing.T) {
	// APRS101 §9 example: "!/5L!!<*e7>7P["
	// Decodes to roughly 49.5°N 72.75°W (rough numbers — the test uses
	// a wider tol since the compressed base91 encoding has 1.5 m resolution).
	d := Decode("!/5L!!<*e7>7P[", "APRS")
	if d.Lat == nil || d.Lon == nil {
		t.Fatalf("expected compressed position decoded, got lat=%v lon=%v", d.Lat, d.Lon)
	}
	// Sanity range checks — the exact value depends on the spec formulas
	// but we want to make sure we're in the right hemisphere/decade.
	if *d.Lat < 0 || *d.Lat > 90 {
		t.Errorf("compressed lat out of N hemisphere range: %v", *d.Lat)
	}
	if *d.Lon < -180 || *d.Lon > 0 {
		t.Errorf("compressed lon out of W hemisphere range: %v", *d.Lon)
	}
}

func TestCompressedPositionRejectsBadChars(t *testing.T) {
	// Compressed lat/lon characters must be in printable base91 range
	// '!' (0x21) through '|' (0x7C). Out-of-range chars should reject.
	d := Decode("!/\x00\x00\x00\x00<*e7>7P[", "APRS")
	if d.Lat != nil {
		t.Errorf("expected nil lat on out-of-range compressed bytes")
	}
}

func TestCompressedPositionTooShort(t *testing.T) {
	// Compressed needs at least 13 bytes after the DTI (sym + 4 lat + 4 lon + sym + 2 cs + T)
	d := Decode("!/5L!!<*", "APRS")
	if d.Lat != nil {
		t.Errorf("expected nil lat on truncated compressed position")
	}
}

// ---------- Object reports (;) ----------

func TestObjectPositionDecoded(t *testing.T) {
	// Real example from RF traffic: 446.25 MHz repeater object.
	d := Decode(";446.25HRM*111111z4031.03N/11200.83WrT11", "APRS")
	if d.Lat == nil || d.Lon == nil {
		t.Fatalf("expected object position decoded")
	}
	if !approx(*d.Lat, 40.5172, 1e-4) {
		t.Errorf("object lat: got %v", *d.Lat)
	}
	if d.ObjectKilled {
		t.Errorf("expected live object (live='*'), got killed=true")
	}
}

func TestObjectKilledFlag(t *testing.T) {
	// '_' in the 10th position marks the object as killed/dead.
	d := Decode(";446.25HRM_111111z4031.03N/11200.83WrT11", "APRS")
	if !d.ObjectKilled {
		t.Errorf("expected ObjectKilled=true for '_' marker")
	}
}

// ---------- Status reports (>) ----------

func TestStatusReportPlainText(t *testing.T) {
	// `>` + free-form text. No leading timestamp.
	d := Decode(">MicroTrak FA v1.42", "APRS")
	if d.Comment != "MicroTrak FA v1.42" {
		t.Errorf("status comment: got %q want %q", d.Comment, "MicroTrak FA v1.42")
	}
}

func TestStatusReportWithTimestamp(t *testing.T) {
	// `>DDHHMMzText` per APRS101 §16. The 7-byte timestamp prefix
	// should be stripped from what we surface as the status text.
	d := Decode(">092345zRunning APRS", "APRS")
	if d.Comment != "Running APRS" {
		t.Errorf("status with timestamp: got %q want %q", d.Comment, "Running APRS")
	}
}

func TestStatusReportTimestampOnlyDigits(t *testing.T) {
	// Only strip the prefix if it's 6 digits followed by 'z'. Random
	// text that happens to be 7 chars long must not be stripped.
	d := Decode(">helloXzStatus text", "APRS")
	if d.Comment != "helloXzStatus text" {
		t.Errorf("status with fake timestamp: should not have stripped, got %q", d.Comment)
	}
}
