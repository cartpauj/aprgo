package gate

import (
	"testing"
	"time"
)

func TestMessagedRecipientTrackerBasic(t *testing.T) {
	tr := NewMessagedRecipientTracker(time.Minute)
	if tr.Consume("N0CALL") {
		t.Fatal("Consume on empty tracker should return false")
	}
	tr.Record("N0CALL")
	if !tr.Consume("N0CALL") {
		t.Fatal("Consume after Record should return true")
	}
	if tr.Consume("N0CALL") {
		t.Fatal("Second Consume should return false (single-use)")
	}
}

func TestMessagedRecipientTrackerCaseInsensitive(t *testing.T) {
	tr := NewMessagedRecipientTracker(time.Minute)
	tr.Record("n0call-10")
	if !tr.Consume("N0CALL-10") {
		t.Fatal("Consume should match recorded entry case-insensitively")
	}
}

func TestMessagedRecipientTrackerExpiry(t *testing.T) {
	tr := NewMessagedRecipientTracker(10 * time.Millisecond)
	tr.Record("N0CALL")
	time.Sleep(20 * time.Millisecond)
	if tr.Consume("N0CALL") {
		t.Fatal("Consume of expired entry should return false")
	}
}

func TestMessagedRecipientTrackerLRUCap(t *testing.T) {
	tr := NewMessagedRecipientTracker(time.Hour)
	// Populate well above trackerCap.
	for i := 0; i < trackerCap+100; i++ {
		tr.Record(string(rune('A'+i%26)) + string(rune('A'+(i/26)%26)) +
			string(rune('A'+(i/676)%26)) + string(rune('0'+(i%10))))
	}
	tr.mu.Lock()
	size := len(tr.m)
	tr.mu.Unlock()
	if size > trackerCap {
		t.Fatalf("LRU cap should bound map at %d, got %d", trackerCap, size)
	}
}
