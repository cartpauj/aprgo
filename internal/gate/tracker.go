package gate

import (
	"strings"
	"sync"
	"time"
)

// MessagedRecipientTracker implements MessagedTracker with a single-use TTL
// map. When we IS→RF gate a message originated by SRC, we Record(SRC). The
// next time we see a position packet from SRC on IS we Consume(SRC) and pass
// it to RF once, then forget them so we don't keep flooding local airspace
// with a remote station's beacon stream.
// trackerCap is the upper bound on map size. Under attack with many unique
// callsigns inside ttl, gc-only eviction would let the map grow without
// limit; cap + oldest-entry eviction keeps it bounded.
const trackerCap = 4096

type MessagedRecipientTracker struct {
	mu  sync.Mutex
	m   map[string]time.Time
	ttl time.Duration
}

// NewMessagedRecipientTracker builds a tracker that forgets entries after
// ttl (recommended ~30 minutes per APRS-IS convention).
func NewMessagedRecipientTracker(ttl time.Duration) *MessagedRecipientTracker {
	return &MessagedRecipientTracker{m: make(map[string]time.Time), ttl: ttl}
}

func (t *MessagedRecipientTracker) Record(call string) {
	if call == "" {
		return
	}
	key := strings.ToUpper(call)
	t.mu.Lock()
	defer t.mu.Unlock()
	t.m[key] = time.Now().Add(t.ttl)
	t.gc()
	// Hard cap: if gc didn't reclaim enough (lots of fresh entries), drop
	// the oldest one. Single-pass O(n) but n is bounded by trackerCap.
	for len(t.m) > trackerCap {
		var oldestKey string
		var oldestExp time.Time
		for k, exp := range t.m {
			if oldestKey == "" || exp.Before(oldestExp) {
				oldestKey, oldestExp = k, exp
			}
		}
		delete(t.m, oldestKey)
	}
}

func (t *MessagedRecipientTracker) Consume(call string) bool {
	if call == "" {
		return false
	}
	key := strings.ToUpper(call)
	t.mu.Lock()
	defer t.mu.Unlock()
	exp, ok := t.m[key]
	if !ok {
		return false
	}
	delete(t.m, key)
	return time.Now().Before(exp)
}

// gc drops expired entries. Caller holds t.mu.
func (t *MessagedRecipientTracker) gc() {
	now := time.Now()
	for k, exp := range t.m {
		if now.After(exp) {
			delete(t.m, k)
		}
	}
}
