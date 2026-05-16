package server

import (
	"sync"
	"sync/atomic"
	"time"
)

// Stats tracks in-memory counters for the operator dashboard. Counts are
// session-only (reset on restart); historical numbers come from the SQLite
// store. The two together let the /stats page answer both "what's happening
// now" and "what has this iGate done overall."
type statsCounters struct {
	startedAt time.Time

	pktsRF atomic.Uint64 // RF-origin packets parsed
	pktsIS atomic.Uint64 // IS-origin packets parsed
	pktsTX atomic.Uint64 // TX-origin packets (our own outgoing — beacons + relays)

	sentIS    atomic.Uint64 // RF→IS gates we emitted
	sentRF    atomic.Uint64 // IS→RF + digipeats we emitted
	digipeats atomic.Uint64 // subset of sentRF: digipeat actions
	beacons   atomic.Uint64 // our own beacons transmitted

	dropsTotal atomic.Uint64 // every gate Drop action
	rateLimited atomic.Uint64 // rate-limiter trips (first packet only)
	distDropped atomic.Uint64 // intake distance hard-drops (>500 km)
	dupesDropped atomic.Uint64 // dupe-table hits

	mu sync.Mutex
	// dropReasons aggregates Drop action reasons → count.
	dropReasons map[string]uint64
}

func newStatsCounters() *statsCounters {
	return &statsCounters{
		startedAt:   time.Now(),
		dropReasons: make(map[string]uint64),
	}
}

func (s *statsCounters) recordDropReason(reason string) {
	s.dropsTotal.Add(1)
	s.mu.Lock()
	s.dropReasons[reason]++
	s.mu.Unlock()
}

// DropReasonSnapshot is one (reason, count) pair for rendering.
type DropReasonSnapshot struct {
	Reason string
	Count  uint64
}

// TopDropReasons returns the top N drop reasons by count, descending.
func (s *statsCounters) TopDropReasons(n int) []DropReasonSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]DropReasonSnapshot, 0, len(s.dropReasons))
	for r, c := range s.dropReasons {
		out = append(out, DropReasonSnapshot{Reason: r, Count: c})
	}
	// Insertion sort by count desc, fine for tiny N.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Count > out[j-1].Count; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	if n > 0 && n < len(out) {
		out = out[:n]
	}
	return out
}
