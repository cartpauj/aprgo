package server

// Gate-drop reason ring buffer + /diagnostics page.
//
// Every packet the gate package decides about returns one or more Actions;
// when an Action.Kind is gate.Drop it carries a human-readable Reason
// (e.g. "RF→IS skipped: own callsign", "IS→RF: bulletin", "IS→RF: recipient not heard
// on RF"). The dispatcher previously discarded those reasons. Now we
// capture the last `dropRingCap` of them in a mutex-guarded ring so the
// operator can answer "why wasn't that gated?" from a UI page without
// reading source or grepping the journal.
//
// In-memory only; lost on restart (correct — these are operational
// diagnostics, not historical records).

import (
	"strings"
	"sync"
	"time"

	"aprgo/internal/ax25"
)

const dropRingCap = 500

type dropEntry struct {
	Time   time.Time
	Origin string // "RF" | "IS" | "TX"
	Source string
	Dest   string
	Info   string // truncated, control-char-stripped
	Reason string
}

type dropRing struct {
	mu      sync.Mutex
	entries []dropEntry
}

func newDropRing() *dropRing {
	return &dropRing{entries: make([]dropEntry, 0, dropRingCap)}
}

// add appends a new drop entry, evicting the oldest if the ring is full.
func (r *dropRing) add(e dropEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.entries) == dropRingCap {
		r.entries = r.entries[1:]
	}
	r.entries = append(r.entries, e)
}

// snapshot returns a copy of the ring, newest-first, for rendering.
func (r *dropRing) snapshot() []dropEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]dropEntry, len(r.entries))
	for i, e := range r.entries {
		out[len(r.entries)-1-i] = e // reverse: newest first
	}
	return out
}

// originLabel maps an ax25.Source to a short string for display.
func originLabel(o ax25.Source) string {
	switch o {
	case ax25.SrcRF:
		return "RF"
	case ax25.SrcIS:
		return "IS"
	case ax25.SrcTX:
		return "TX"
	}
	return "??"
}

// sanitizeInfo trims an info field for human display in the diagnostics
// table: strip control characters, collapse whitespace, cap length.
const infoDisplayMax = 80

func sanitizeInfo(info []byte) string {
	if len(info) == 0 {
		return ""
	}
	var b strings.Builder
	b.Grow(len(info))
	for _, c := range info {
		if c < 0x20 || c == 0x7f {
			b.WriteByte(' ')
		} else {
			b.WriteByte(c)
		}
	}
	s := strings.TrimSpace(b.String())
	if len(s) > infoDisplayMax {
		s = s[:infoDisplayMax-1] + "…"
	}
	return s
}
