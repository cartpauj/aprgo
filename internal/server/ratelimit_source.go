package server

import (
	"sort"
	"sync"
	"time"
)

// sourceRateLimiter is a per-source counter that triggers a fixed timeout
// when a station exceeds the per-minute threshold. Designed to silence
// genuinely broken transmitters (stuck PTT, runaway script) rather than to
// shape polite traffic.
//
// Behavior:
//   - Each source accumulates a count within a rolling 60-second window.
//   - On packet (threshold+1) the source enters a fixed `timeout`; every
//     packet from that source is dropped until the timeout expires.
//   - When the timeout expires the source resets fully — the next packet
//     starts a fresh window with count=1.
type sourceRateLimiter struct {
	mu          sync.Mutex
	buckets     map[string]*sourceBucket
	threshold   int           // packets/minute that triggers timeout
	timeout     time.Duration // how long a source stays blocked once tripped
	lastCleanup time.Time     // time-bucketed periodic GC, drives maybeCleanup
}

type sourceBucket struct {
	count        int
	window       time.Time
	blockedUntil time.Time // zero = not blocked
}

func newSourceRateLimiter(threshold int, timeout time.Duration) *sourceRateLimiter {
	return &sourceRateLimiter{
		buckets:   make(map[string]*sourceBucket),
		threshold: threshold,
		timeout:   timeout,
	}
}

// Allow returns (ok, justBlocked). ok=false means the packet should be
// dropped. justBlocked=true is set on the single packet that crosses the
// threshold — used by the caller to emit a one-shot diagnostic entry rather
// than spam the drop ring with every subsequent blocked packet.
func (l *sourceRateLimiter) Allow(source string) (ok bool, justBlocked bool) {
	if source == "" {
		return true, false
	}
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()

	// Periodic time-bucketed cleanup: walk and evict idle buckets once per
	// minute. Without this, a workload that stops adding new sources would
	// keep the map at its peak size forever (previously cleanup only fired
	// on first-seen inserts).
	if now.Sub(l.lastCleanup) > time.Minute {
		l.maybeCleanup(now)
		l.lastCleanup = now
	}
	b, ok2 := l.buckets[source]
	if !ok2 {
		l.buckets[source] = &sourceBucket{count: 1, window: now}
		return true, false
	}
	// Currently blocked?
	if !b.blockedUntil.IsZero() {
		if now.Before(b.blockedUntil) {
			return false, false
		}
		// Timeout expired — fresh slate.
		b.count = 1
		b.window = now
		b.blockedUntil = time.Time{}
		return true, false
	}
	// Window rolled over.
	if now.Sub(b.window) >= time.Minute {
		b.count = 1
		b.window = now
		return true, false
	}
	// Within window.
	b.count++
	if b.count > l.threshold {
		b.blockedUntil = now.Add(l.timeout)
		return false, true
	}
	return true, false
}

// maybeCleanup evicts stale buckets — called periodically (once per minute)
// by Allow(), plus on every new-source insert. Drops idle buckets (no recent
// window, not currently blocked) so the map doesn't grow unboundedly when
// many unique sources have been seen. Caller must hold l.mu.
func (l *sourceRateLimiter) maybeCleanup(now time.Time) {
	for k, v := range l.buckets {
		if v.blockedUntil.IsZero() && now.Sub(v.window) > 5*time.Minute {
			delete(l.buckets, k)
		}
	}
}

// IsBlocked reports whether the given source is in a timeout right now.
// Used by the stations page to render a per-row indicator.
func (l *sourceRateLimiter) IsBlocked(source string) bool {
	if source == "" {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[source]
	if !ok {
		return false
	}
	return !b.blockedUntil.IsZero() && time.Now().Before(b.blockedUntil)
}

// BlockedEntry describes one currently-blocked source for the diagnostics
// page. Sorted by expiry (soonest first) by BlockedSources.
type BlockedEntry struct {
	Source       string
	BlockedUntil time.Time
}

// BlockedSources snapshots every source currently in timeout, sorted by the
// time their block expires (soonest first).
func (l *sourceRateLimiter) BlockedSources() []BlockedEntry {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []BlockedEntry
	for src, b := range l.buckets {
		if !b.blockedUntil.IsZero() && now.Before(b.blockedUntil) {
			out = append(out, BlockedEntry{Source: src, BlockedUntil: b.blockedUntil})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].BlockedUntil.Before(out[j].BlockedUntil)
	})
	return out
}
