package server

import (
	"sync"
	"sync/atomic"
	"time"
)

// statsCounters tracks operational counters for the dashboard and /stats
// page. Each atomic field holds a LIFETIME value (loaded from SQLite at
// startup, flushed back periodically). The "session" view shown in the UI
// is computed as current - sessionBase, where sessionBase is a snapshot of
// the same fields taken right after the lifetime values were loaded.
//
// dropReasons is session-only (in-memory map). Lifetime drop-reason
// breakdown would require a separate table and currently has no clear ask.
type statsCounters struct {
	startedAt time.Time

	pktsRF atomic.Uint64 // RF-origin packets parsed
	pktsIS atomic.Uint64 // IS-origin packets parsed
	pktsTX atomic.Uint64 // TX-origin packets (our own outgoing — beacons + relays)

	sentIS    atomic.Uint64 // RF→IS gates we emitted
	sentRF    atomic.Uint64 // IS→RF + digipeats we emitted
	digipeats atomic.Uint64 // subset of sentRF: digipeat actions
	// igateMsgsRF counts ONLY IS→RF gated messages (the MSG_CNT metric in
	// `?IGATE?` capability replies). Dedicated counter avoids the underflow
	// race that would occur if we computed it as sentRF - digipeats from two
	// separate atomic loads — atomics give per-variable linearizability, not
	// snapshot consistency. Standard Prometheus-style "one counter per
	// quantity" pattern.
	igateMsgsRF atomic.Uint64
	beacons     atomic.Uint64 // our own beacons transmitted

	dropsTotal   atomic.Uint64 // every gate Drop action
	rateLimited  atomic.Uint64 // rate-limiter trips (first packet only)
	distDropped  atomic.Uint64 // intake distance hard-drops (>500 km)
	dupesDropped atomic.Uint64 // dupe-table hits

	mu sync.Mutex
	// dropReasons aggregates Drop action reasons → count. Session-only.
	dropReasons map[string]uint64
	// sessionBase captures each lifetime counter's value at the moment we
	// finished hydrating from SQLite, so the UI can show
	//   session = lifetime - sessionBase[name]
	// alongside the lifetime number.
	sessionBase map[string]uint64
}

func newStatsCounters() *statsCounters {
	return &statsCounters{
		startedAt:   time.Now(),
		dropReasons: make(map[string]uint64),
		sessionBase: make(map[string]uint64),
	}
}

// counterFields returns the canonical list of (name, *atomic.Uint64) pairs
// in one place so Hydrate/Snapshot stay in sync as fields are added.
func (s *statsCounters) counterFields() []struct {
	name string
	a    *atomic.Uint64
} {
	return []struct {
		name string
		a    *atomic.Uint64
	}{
		{"pkts_rf", &s.pktsRF},
		{"pkts_is", &s.pktsIS},
		{"pkts_tx", &s.pktsTX},
		{"sent_is", &s.sentIS},
		{"sent_rf", &s.sentRF},
		{"digipeats", &s.digipeats},
		{"igate_msgs_rf", &s.igateMsgsRF},
		{"beacons", &s.beacons},
		{"drops_total", &s.dropsTotal},
		{"rate_limited", &s.rateLimited},
		{"dist_dropped", &s.distDropped},
		{"dupes_dropped", &s.dupesDropped},
	}
}

// dropReasonKeyPrefix namespaces persisted drop-reason rows in the counters
// table so they don't collide with scalar counter names.
const dropReasonKeyPrefix = "drop_reason:"

// Hydrate seeds atomic counters from a persisted snapshot (loaded from
// SQLite) and captures sessionBase = the same values, so subsequent
// session-vs-lifetime math is correct. Must be called BEFORE the parseLoop
// starts incrementing — otherwise a concurrent .Add would race with .Store.
// Also restores the dropReasons aggregate so the "Top drop reasons" panel
// reflects lifetime totals across restarts.
func (s *statsCounters) Hydrate(persisted map[string]uint64) {
	for _, f := range s.counterFields() {
		v := persisted[f.name] // zero if absent — fine on first run
		f.a.Store(v)
	}
	s.mu.Lock()
	for _, f := range s.counterFields() {
		s.sessionBase[f.name] = f.a.Load()
	}
	for k, v := range persisted {
		if reason, ok := stripPrefix(k, dropReasonKeyPrefix); ok && v > 0 {
			s.dropReasons[reason] = v
		}
	}
	s.mu.Unlock()
}

// Snapshot reads every atomic counter and every drop-reason aggregate into
// a flat map suitable for SaveCounters. Drop-reason keys carry the
// "drop_reason:" namespace prefix.
func (s *statsCounters) Snapshot() map[string]uint64 {
	out := make(map[string]uint64, 12)
	for _, f := range s.counterFields() {
		out[f.name] = f.a.Load()
	}
	s.mu.Lock()
	for reason, count := range s.dropReasons {
		out[dropReasonKeyPrefix+reason] = count
	}
	s.mu.Unlock()
	return out
}

// stripPrefix returns (s without prefix, true) if s starts with prefix,
// or ("", false) otherwise. Tiny helper kept inline to avoid an import.
func stripPrefix(s, prefix string) (string, bool) {
	if len(s) < len(prefix) || s[:len(prefix)] != prefix {
		return "", false
	}
	return s[len(prefix):], true
}

// CounterView is one (session, lifetime) pair for rendering. Session is
// always ≤ lifetime; subtraction is saturating in case Hydrate ever runs
// after a counter has been touched.
type CounterView struct {
	Session  uint64
	Lifetime uint64
}

// View returns the (session, lifetime) view for a single counter name.
func (s *statsCounters) View(name string) CounterView {
	for _, f := range s.counterFields() {
		if f.name == name {
			cur := f.a.Load()
			s.mu.Lock()
			base := s.sessionBase[name]
			s.mu.Unlock()
			sess := cur
			if cur >= base {
				sess = cur - base
			} else {
				sess = 0
			}
			return CounterView{Session: sess, Lifetime: cur}
		}
	}
	return CounterView{}
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
