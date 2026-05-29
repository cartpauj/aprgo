package store

import (
	"path/filepath"
	"testing"
	"time"
)

// TestMessageCallsignCaseFolding verifies that callsigns are normalized to
// uppercase at write time, so a conversation isn't split when one side's call
// arrives in a different case (e.g. a lowercase third-party MsgOrigSrc vs an
// operator-typed uppercase reply), and lookups are case-insensitive.
func TestMessageCallsignCaseFolding(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "t.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	// Inbound logged with a lowercase originator; outbound reply uppercase.
	if _, err := db.LogMessage(Message{Time: time.Unix(1, 0), Direction: "in", Source: "wb2osz-5", Dest: "KG7OKR-10", Body: "hi"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.LogMessage(Message{Time: time.Unix(2, 0), Direction: "out", Source: "KG7OKR-10", Dest: "WB2OSZ-5", Body: "yo"}); err != nil {
		t.Fatal(err)
	}

	// Caller passes its own call in mixed case — must still fold to ONE peer.
	convs, err := db.Conversations("kg7okr-10")
	if err != nil {
		t.Fatal(err)
	}
	if len(convs) != 1 {
		t.Fatalf("expected 1 conversation (case-folded), got %d: %+v", len(convs), convs)
	}
	if convs[0].Peer != "WB2OSZ-5" {
		t.Errorf("peer = %q, want WB2OSZ-5", convs[0].Peer)
	}
	if convs[0].Count != 2 {
		t.Errorf("conversation message count = %d, want 2", convs[0].Count)
	}

	// Thread lookup with a lowercase peer arg returns both messages.
	msgs, err := db.MessagesWithPeer("KG7OKR-10", "wb2osz-5", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Errorf("expected 2 messages in thread, got %d", len(msgs))
	}
}

// TestMarkAckCaseInsensitive confirms an ack clears the matching outbound row
// regardless of callsign case.
func TestMarkAckCaseInsensitive(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "t.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	id, err := db.LogMessage(Message{Time: time.Unix(1, 0), Direction: "out", Source: "KG7OKR-10", Dest: "WB2OSZ-5", Body: "ping", MsgID: "7", State: "pending"})
	if err != nil {
		t.Fatal(err)
	}
	// Ack arrives from wb2osz-5 (lowercase), addressed back to kg7okr-10.
	got, err := db.MarkAck("kg7okr-10", "wb2osz-5", "7")
	if err != nil {
		t.Fatal(err)
	}
	if got != id {
		t.Fatalf("MarkAck returned id %d, want %d (the pending outbound row)", got, id)
	}
}
