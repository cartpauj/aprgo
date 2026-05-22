package aprs

import "testing"

// Real-world APRS clients freely emit 8-bit characters in comment / status
// / message bodies using Latin-1 (ISO-8859-1) encoding. The web UI renders
// as UTF-8, so any byte > 0x7F would otherwise show as the U+FFFD
// replacement char. Our parser converts Latin-1 → UTF-8 at the display-
// field boundary so these chars render correctly.

func TestStatusReportPreservesDegreeSign(t *testing.T) {
	// Real status report seen on RF: contains 0xB0 (°) in Latin-1.
	info := ">DN40ow/yDX: KF6RAL-10 71.2mi 319\xB0 22:01"
	d := Decode(info, "APRS")
	// In UTF-8, ° = 0xC2 0xB0. Should be exactly that, not 0xB0 alone
	// (which would render as the replacement char in a UTF-8 browser).
	if d.Comment == "" {
		t.Fatalf("expected non-empty comment")
	}
	if !contains(d.Comment, "319\xC2\xB0") {
		t.Errorf("expected UTF-8 ° preserved, got %q", d.Comment)
	}
}

func TestStatusReportDropsControlChars(t *testing.T) {
	info := ">Hello\x01\x02world\x7F"
	d := Decode(info, "APRS")
	if d.Comment != "Helloworld" {
		t.Errorf("control chars not stripped: got %q", d.Comment)
	}
}

func TestCommentPreservesLatin1Mid(t *testing.T) {
	// Position with a Latin-1 char in the comment.
	info := "!4903.50N/07201.75W-Caf\xE9 stop"
	d := Decode(info, "APRS")
	// é = U+00E9 = UTF-8 0xC3 0xA9
	if !contains(d.Comment, "Caf\xC3\xA9") {
		t.Errorf("expected UTF-8 é preserved, got %q", d.Comment)
	}
}

func TestMessageBodyPreservesLatin1(t *testing.T) {
	info := ":N0CALL   :Temperature: 20\xB0C"
	d := Decode(info, "APRS")
	if !contains(d.MsgBody, "20\xC2\xB0C") {
		t.Errorf("expected UTF-8 ° in message body, got %q", d.MsgBody)
	}
}

func TestDropsLatin1ControlsC1Range(t *testing.T) {
	// Bytes 0x80-0x9F are Latin-1 C1 controls (or codepage glyphs that
	// differ across systems). Drop them rather than convert.
	info := ">Test\x88\x95mid"
	d := Decode(info, "APRS")
	if d.Comment != "Testmid" {
		t.Errorf("C1 controls should be stripped: got %q", d.Comment)
	}
}

func TestCommentPreservesValidUTF8(t *testing.T) {
	// Modern clients (APRSdroid etc.) emit UTF-8 directly. Some upstream
	// iGates also re-encode Latin-1 to UTF-8 before we see the bytes.
	// Either way, valid UTF-8 sequences must pass through unchanged — not
	// get byte-by-byte Latin-1-mangled into mojibake.
	//
	// "Türkiye" — ü = U+00FC = UTF-8 0xC3 0xBC.
	info := "!4903.50N/07201.75W-T\xC3\xBCrkiye"
	d := Decode(info, "APRS")
	if !contains(d.Comment, "T\xC3\xBCrkiye") {
		t.Errorf("valid UTF-8 should pass through unchanged, got %q", d.Comment)
	}
}

func TestCourseSpeedStripsWithoutSpace(t *testing.T) {
	// Per APRS101 §7, CSE/SPD is a fixed-length 7-byte field with no
	// required trailing delimiter. Comment text may follow immediately.
	info := "!4903.50N/07201.75W>174/000looking for menudo!"
	d := Decode(info, "APRS")
	if d.Course != 174 {
		t.Errorf("course: got %d want 174", d.Course)
	}
	// 0 knots → 0 mph after rounding.
	if d.Speed != 0 {
		t.Errorf("speed: got %d want 0", d.Speed)
	}
	if d.Comment != "looking for menudo!" {
		t.Errorf("comment: got %q want %q", d.Comment, "looking for menudo!")
	}
}

func TestNonStandardTimestampSuffix(t *testing.T) {
	// Real packet from Microsat WX3in1 Mini: @DDHHMM<letter> instead of
	// the spec's {z,h,/}. Position should still decode.
	info := "@220649I4143.09N/11149.22W#River Heights"
	d := Decode(info, "APRS")
	if d.Lat == nil || d.Lon == nil {
		t.Fatalf("expected position to decode, got lat=%v lon=%v", d.Lat, d.Lon)
	}
}

func TestLowercasePHG(t *testing.T) {
	// Real packet: Kantronics KPC-3 emits "phg6630" in lowercase.
	info := "!4145.27N/11143.06W#phg6630/W2,UTn"
	d := Decode(info, "APRS")
	if d.PHG == nil {
		t.Fatalf("expected lowercase phg to decode")
	}
}

// contains is a tiny strings.Contains substitute so we don't depend on
// strings package in test (and the byte literals are clearer this way).
func contains(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
