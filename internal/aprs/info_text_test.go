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
