package aprs

import "testing"

// Mic-E test data taken from real RF traffic captured by an iGate on
// 2026-05-21. Bytes are spelled as hex string literals because real-world
// Mic-E bodies contain non-printable bytes (0x1C / 0x1F unit-separator-style
// characters used in speed/course encoding) that would otherwise mangle in
// editors. The known-good lat/lon were independently verified at capture
// time, so these double as a regression snapshot — any future Mic-E refactor
// must continue to produce the same coordinates for these inputs.

func TestMicE_KK7LLM(t *testing.T) {
	// Source KK7LLM-13, dest T0QQQU. Known: 40.18583, -111.60817
	info := "\x60\x27\x40\x4d\x6d\x2a\x1f\x6b\x2f\x27\x22\x43\x36\x7d\x4b\x4b"
	d := Decode(info, "T0QQQU")
	if d.Lat == nil || d.Lon == nil {
		t.Fatalf("expected position decoded, got lat=%v lon=%v", d.Lat, d.Lon)
	}
	if !approx(*d.Lat, 40.18583, 1e-3) {
		t.Errorf("lat: got %v want ~40.18583", *d.Lat)
	}
	if !approx(*d.Lon, -111.60817, 1e-3) {
		t.Errorf("lon: got %v want ~-111.60817", *d.Lon)
	}
}

func TestMicE_K7UHP(t *testing.T) {
	// Source K7UHP-2, dest 4QPRRX. Known: 41.038, -111.98633
	info := "\x60\x27\x57\x2e\x6c\x20\x1c\x2d\x2f\x5d\x22\x42\x29\x7d\x34\x34"
	d := Decode(info, "4QPRRX")
	if d.Lat == nil || d.Lon == nil {
		t.Fatalf("expected position decoded")
	}
	if !approx(*d.Lat, 41.038, 1e-3) {
		t.Errorf("lat: got %v want ~41.038", *d.Lat)
	}
	if !approx(*d.Lon, -111.98633, 1e-3) {
		t.Errorf("lon: got %v want ~-111.98633", *d.Lon)
	}
}

func TestMicE_K6KRU(t *testing.T) {
	// Source K6KRU-9, dest T0UXSS. Known: 40.97217, -111.92683
	info := "\x60\x27\x53\x59\x6c\x52\x6e\x6b\x2f\x60\x22\x42\x28\x7d\x31\x34"
	d := Decode(info, "T0UXSS")
	if d.Lat == nil || d.Lon == nil {
		t.Fatalf("expected position decoded")
	}
	if !approx(*d.Lat, 40.97217, 1e-3) {
		t.Errorf("lat: got %v want ~40.97217", *d.Lat)
	}
	if !approx(*d.Lon, -111.92683, 1e-3) {
		t.Errorf("lon: got %v want ~-111.92683", *d.Lon)
	}
}

func TestMicE_SymbolExtracted(t *testing.T) {
	// Mic-E body encodes symbol table at body[7] and symbol code at body[8]
	// per APRS101 §10.1.5. Confirm both are populated to non-empty.
	info := "\x60\x27\x40\x4d\x6d\x2a\x1f\x6b\x2f\x27\x22\x43\x36\x7d"
	d := Decode(info, "T0QQQU")
	if d.Symbol == "" {
		t.Errorf("expected non-empty Symbol on Mic-E packet")
	}
	if len(d.Symbol) != 2 {
		t.Errorf("Symbol should be 2 chars (table + code), got %q (len=%d)", d.Symbol, len(d.Symbol))
	}
}

func TestMicE_OldDTIBacktickPrime(t *testing.T) {
	// The "old" Mic-E DTI is `'` (apostrophe) — same decoder, alternate
	// type indicator. Same bytes after the DTI should decode the same.
	// Substitute the leading 0x60 with 0x27.
	info := "\x27\x27\x40\x4d\x6d\x2a\x1f\x6b\x2f\x27\x22\x43\x36\x7d"
	d := Decode(info, "T0QQQU")
	if d.Lat == nil {
		t.Fatalf("expected position decoded for ' DTI variant")
	}
}

func TestMicE_RejectsShortBody(t *testing.T) {
	// Body needs ~9 bytes after DTI for the position field. Anything
	// shorter must not panic and must not produce a position.
	d := Decode("\x60abc", "T0QQQU")
	if d.Lat != nil {
		t.Errorf("expected nil lat on short Mic-E body")
	}
}

func TestMicE_RejectsShortDest(t *testing.T) {
	// Destination call must be at least 6 chars to encode the latitude.
	// Anything shorter must fail-soft, not panic.
	info := "\x60\x27\x40\x4d\x6d\x2a\x1f\x6b\x2f\x27\x22\x43\x36\x7d"
	d := Decode(info, "FOO")
	if d.Lat != nil {
		t.Errorf("expected nil lat on short dest")
	}
}

func TestMicE_RejectsInvalidLonMin(t *testing.T) {
	// Per mic-e-types.txt, body[1]-28 values 60-69 are reserved/invalid.
	// body[1] = 0x58 ('X') → 88-28 = 60 (invalid).
	info := "\x60\x58\x20\x6c\x20\x1c\x2d\x2f\x5d\x22"
	d := Decode(info, "T0QQQU")
	if d.Lat != nil {
		t.Errorf("expected rejection of lonMin>=60, got lat=%v", *d.Lat)
	}
}

// ---------- Messages (:) ----------

func TestMessageBasic(t *testing.T) {
	d := Decode(":N0CALL   :Hello, World!", "APRS")
	if !d.IsMessage {
		t.Fatalf("expected IsMessage=true")
	}
	if d.MsgTo != "N0CALL" {
		t.Errorf("MsgTo: got %q want %q", d.MsgTo, "N0CALL")
	}
	if d.MsgBody != "Hello, World!" {
		t.Errorf("MsgBody: got %q want %q", d.MsgBody, "Hello, World!")
	}
}

func TestMessageWithID(t *testing.T) {
	// Body terminator `{NN` is the outbound message ID per APRS101 §14.
	d := Decode(":N0CALL   :Hello{42", "APRS")
	if d.MsgBody != "Hello" {
		t.Errorf("body without ID: got %q want %q", d.MsgBody, "Hello")
	}
	if d.MsgID != "42" {
		t.Errorf("MsgID: got %q want %q", d.MsgID, "42")
	}
}

func TestMessageReplyAckForm(t *testing.T) {
	// APRS 1.1 reply-ack: `body{MM}AA` — own msgID MM plus piggyback ack
	// for the peer's earlier msgID AA. `}` is the separator.
	d := Decode(":N0CALL   :Reply{AB}CD", "APRS")
	if d.MsgBody != "Reply" {
		t.Errorf("MsgBody: got %q want %q", d.MsgBody, "Reply")
	}
	if d.MsgID != "AB" {
		t.Errorf("MsgID: got %q want %q", d.MsgID, "AB")
	}
	if d.ReplyAckID != "CD" {
		t.Errorf("ReplyAckID: got %q want %q", d.ReplyAckID, "CD")
	}
}

func TestMessageAck(t *testing.T) {
	d := Decode(":N0CALL   :ack42", "APRS")
	if !d.IsAck {
		t.Fatalf("expected IsAck=true")
	}
	if d.IsRej {
		t.Errorf("ack should not also be rej")
	}
	if d.AckedID != "42" {
		t.Errorf("AckedID: got %q want %q", d.AckedID, "42")
	}
}

func TestMessageRej(t *testing.T) {
	d := Decode(":N0CALL   :rejVE", "APRS")
	if !d.IsRej {
		t.Fatalf("expected IsRej=true")
	}
	if d.IsAck {
		t.Errorf("rej should not also be ack")
	}
	if d.AckedID != "VE" {
		t.Errorf("AckedID: got %q want %q", d.AckedID, "VE")
	}
}

func TestMessageAckReplyAckForm(t *testing.T) {
	// Some clients emit `ackMM}AA` where AA is a piggyback ack.
	// Split: AckedID=MM, ReplyAckID=AA.
	d := Decode(":N0CALL   :ackAB}CD", "APRS")
	if !d.IsAck {
		t.Fatalf("expected IsAck=true")
	}
	if d.AckedID != "AB" {
		t.Errorf("AckedID: got %q want %q", d.AckedID, "AB")
	}
	if d.ReplyAckID != "CD" {
		t.Errorf("ReplyAckID: got %q want %q", d.ReplyAckID, "CD")
	}
}

func TestMessageAckSanitizesGarbageBytes(t *testing.T) {
	// Real-world broken transmitters sometimes emit non-printable bytes
	// in the msgID field. Decoder must strip them (else UI renders �).
	d := Decode(":N0CALL   :ack\xffVE", "APRS")
	if !d.IsAck {
		t.Fatalf("expected IsAck=true")
	}
	if d.AckedID != "VE" {
		t.Errorf("AckedID should have stripped \\xff: got %q want %q", d.AckedID, "VE")
	}
}

func TestMessageAddresseeTrimmed(t *testing.T) {
	// The 9-char addressee field is space-padded; trim trailing spaces
	// but not leading (callsigns have no leading whitespace).
	d := Decode(":N0CALL-9 :body", "APRS")
	if d.MsgTo != "N0CALL-9" {
		t.Errorf("MsgTo (with SSID): got %q want %q", d.MsgTo, "N0CALL-9")
	}
}

func TestMessageRejectsBadAddresseeLength(t *testing.T) {
	// Addressee field must be exactly 9 chars followed by ':'. If the
	// 10th byte isn't ':', this isn't a message — bail out.
	d := Decode(":N0CALL   Xbody", "APRS")
	if d.IsMessage {
		t.Errorf("should not recognize as message when 10th byte isn't ':'")
	}
}
