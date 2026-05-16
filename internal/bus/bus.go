// Package bus is an in-memory pub/sub fanout with generic typed topics.
//
// Each subscriber gets a small per-subscriber buffered channel. If a slow
// subscriber falls behind, events are dropped for that subscriber rather than
// blocking publishers. This is the right behavior here: better to drop a
// packet to a stalled consumer than stall the RF reader.
package bus

import (
	"sync"

	"aprgo/internal/aprs"
	"aprgo/internal/ax25"
)

const subBuffer = 64

// Topic fans out values of type T to N subscribers.
type Topic[T any] struct {
	mu   sync.RWMutex
	subs map[chan T]struct{}
}

func NewTopic[T any]() *Topic[T] {
	return &Topic[T]{subs: make(map[chan T]struct{})}
}

// Subscribe returns a channel that receives published values and a function
// to cancel the subscription.
func (t *Topic[T]) Subscribe() (<-chan T, func()) {
	ch := make(chan T, subBuffer)
	t.mu.Lock()
	t.subs[ch] = struct{}{}
	t.mu.Unlock()
	return ch, func() {
		t.mu.Lock()
		if _, ok := t.subs[ch]; ok {
			delete(t.subs, ch)
			close(ch)
		}
		t.mu.Unlock()
	}
}

// Publish sends to all subscribers, dropping for any whose buffer is full.
func (t *Topic[T]) Publish(v T) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	for ch := range t.subs {
		select {
		case ch <- v:
		default:
		}
	}
}

// Count returns the current subscriber count (diagnostics).
func (t *Topic[T]) Count() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.subs)
}

// Bus bundles the two canonical topics every aprgo component cares about.
type Bus struct {
	Frames  *Topic[ax25.Frame]
	Packets *Topic[aprs.Packet]
}

func New() *Bus {
	return &Bus{
		Frames:  NewTopic[ax25.Frame](),
		Packets: NewTopic[aprs.Packet](),
	}
}
