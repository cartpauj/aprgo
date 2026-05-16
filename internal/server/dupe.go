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
	mu     sync.Mutex
	window time.Duration
	seen   map[[20]byte]time.Time
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
	// Opportunistic GC
	if len(d.seen) > 512 {
		for k, t := range d.seen {
			if now.Sub(t) > d.window {
				delete(d.seen, k)
			}
		}
	}
	return false
}
