package server

import (
	"crypto/sha1"
	"sync"
	"time"
)

// dupeTable is a small bounded LRU-ish set of recently-seen packet hashes
// used to suppress digipeat reflections. Anything seen within the window is
// considered a duplicate.
type dupeTable struct {
	mu         sync.Mutex
	window     time.Duration
	seen       map[[20]byte]time.Time
	lastGCSize int // size at last GC; only re-walk when it doubles
}

func newDupeTable(window time.Duration) *dupeTable {
	return &dupeTable{window: window, seen: make(map[[20]byte]time.Time, 256)}
}

// CheckAndMark returns true if key was already seen within the window;
// otherwise records it and returns false.
func (d *dupeTable) CheckAndMark(key []byte) bool {
	h := sha1.Sum(key)
	now := time.Now()
	d.mu.Lock()
	defer d.mu.Unlock()
	if t, ok := d.seen[h]; ok && now.Sub(t) < d.window {
		return true
	}
	d.seen[h] = now
	// Opportunistic GC: walk the map only when it has grown substantially
	// since the last sweep (default trigger 512, then doubles each time).
	// Without this guard a busy parse loop would re-walk the map every
	// packet once size > 512, hammering the contended dupe mutex.
	trigger := d.lastGCSize * 2
	if trigger < 512 {
		trigger = 512
	}
	if len(d.seen) > trigger {
		for k, t := range d.seen {
			if now.Sub(t) > d.window {
				delete(d.seen, k)
			}
		}
		d.lastGCSize = len(d.seen)
	}
	return false
}
