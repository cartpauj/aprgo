package server

// Viscous-delay TX queue for fill-in WIDE1-1 digipeats.
//
// The gate marks fill-in digipeats with Action.Viscous=true. Instead of
// firing them immediately we hold them for a randomized 3–5 s window. If
// during that window we hear the SAME content (same src+dest+info) on RF
// — meaning some other digi already handled it — we cancel the queued TX.
//
// Why randomized: a fixed hold means every fill-in digi in earshot would
// fire at exactly the same moment after the hold expires, defeating the
// whole point. Random staggering means the shortest-random digi fires
// first; all others see it during their longer hold and cancel.
//
// Lookup key is the same content hash the regular dupe table uses:
// `<src>:<dest>:<info>`. This way a retransmit from any other station
// with the same content matches our queued entry regardless of path.

import (
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"aprgo/internal/gate"
)

const (
	viscousMinHold = 3000 * time.Millisecond
	viscousMaxHold = 5000 * time.Millisecond
)

type viscousEntry struct {
	timer  *time.Timer
	action gate.Action
}

type viscousQueue struct {
	mu      sync.Mutex
	entries map[string]*viscousEntry
}

func newViscousQueue() *viscousQueue {
	return &viscousQueue{entries: make(map[string]*viscousEntry)}
}

// enqueue stores `a` keyed by `contentHash` and schedules `fire` to run
// after a random 3–5 s hold. If the same hash is already queued, the new
// entry is dropped (we're already holding this content; don't double-up).
func (q *viscousQueue) enqueue(contentHash string, a gate.Action, fire func(gate.Action)) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if _, exists := q.entries[contentHash]; exists {
		return false
	}
	holdMs := int(viscousMinHold/time.Millisecond) +
		rand.Intn(int((viscousMaxHold-viscousMinHold)/time.Millisecond)+1)
	hold := time.Duration(holdMs) * time.Millisecond
	entry := &viscousEntry{action: a}
	entry.timer = time.AfterFunc(hold, func() {
		q.mu.Lock()
		stored, ok := q.entries[contentHash]
		if ok && stored == entry {
			delete(q.entries, contentHash)
		}
		q.mu.Unlock()
		if ok && stored == entry {
			fire(a)
		}
	})
	q.entries[contentHash] = entry
	return true
}

// cancelIfQueued stops a pending viscous TX for `contentHash` if one is
// queued. Returns true iff something was cancelled — used to bump a
// "suppressed by viscous" counter.
func (q *viscousQueue) cancelIfQueued(contentHash string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	entry, ok := q.entries[contentHash]
	if !ok {
		return false
	}
	entry.timer.Stop()
	delete(q.entries, contentHash)
	return true
}

// suppressedByViscous is a process-lifetime counter. The dispatcher
// increments it on each cancelIfQueued hit so the operator can see how
// much RF noise their fill-in isn't generating.
var suppressedByViscous atomic.Uint64
