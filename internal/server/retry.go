package server

// Outbound APRS message retry queue.
//
// Standard APRS messaging: the sender keeps retransmitting a message (same
// msg-id) until the recipient acks or N attempts have failed. Recipients
// auto-ack on receipt; the retry loop on the sender stops as soon as that
// ack lands. Without this, anything we send via /messages/send would be
// fire-and-forget — the recipient's radio could miss the first frame on
// the air and we'd never know.
//
// Design notes:
//   - In-memory only. We don't persist the pending queue: if aprgo restarts
//     mid-window, we drop the retries on the floor rather than fire a 2-day-
//     old message after the operator's gone to bed. This matches the spirit
//     of APRS messaging — retries are a "right now" thing.
//   - One entry per message ID (the DB row id). Looked up on ack to dequeue.
//   - One sweep goroutine ticks every retrySweepInterval and fires any due
//     attempts. Single goroutine = no contention; the worker holds the
//     queue mutex only long enough to copy out pending items.

import (
	"context"
	"log"
	"sync"
	"time"

	"aprgo/internal/aprs"
	"aprgo/internal/state"
	"aprgo/internal/store"
)

const (
	retryMaxAttempts     = 5                      // 1 original + 4 retries
	retryInterval        = 30 * time.Second       // between attempts
	retrySweepInterval   = 5 * time.Second        // how often the worker scans
)

// retryEntry is one outbound message awaiting ack.
type retryEntry struct {
	ID           int64
	Source, Dest string
	MsgID, Body  string
	ViaRF, ViaIS bool
	Attempts     int       // count of TX attempts so far (1 after the original send)
	NextRetry    time.Time // when the next retry should fire
}

// retryQueue is the server's outbound message retry pool.
type retryQueue struct {
	mu      sync.Mutex
	entries map[int64]*retryEntry
}

func newRetryQueue() *retryQueue {
	return &retryQueue{entries: make(map[int64]*retryEntry)}
}

// Add enrolls an outbound message in the retry queue. Attempts is set to 1
// because the caller has already TX'd the original send; we count retries
// from there.
func (q *retryQueue) Add(e *retryEntry) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.entries[e.ID] = e
}

// Remove drops a message from the queue. Called on ack (success), cancel
// (user-pressed-×), or exhaustion (no ack after retryMaxAttempts).
func (q *retryQueue) Remove(id int64) *retryEntry {
	q.mu.Lock()
	defer q.mu.Unlock()
	e := q.entries[id]
	delete(q.entries, id)
	return e
}

// due copies pointers to entries whose NextRetry is in the past. Returns a
// new slice so the caller can iterate without holding the queue mutex.
func (q *retryQueue) due(now time.Time) []*retryEntry {
	q.mu.Lock()
	defer q.mu.Unlock()
	var out []*retryEntry
	for _, e := range q.entries {
		if !now.Before(e.NextRetry) {
			out = append(out, e)
		}
	}
	return out
}

// Has reports whether a given message ID is still in the queue.
func (q *retryQueue) Has(id int64) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	_, ok := q.entries[id]
	return ok
}

// runRetryWorker is the server's outbound retry sweep loop.
func (s *Server) runRetryWorker(ctx context.Context) {
	tick := time.NewTicker(retrySweepInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			for _, e := range s.retries.due(now) {
				s.fireRetry(e)
			}
		}
	}
}

// fireRetry executes a single retry attempt, updates state in both the queue
// and the DB, and either schedules the next attempt or marks the message
// failed if we've exhausted retryMaxAttempts.
func (s *Server) fireRetry(e *retryEntry) {
	// Encode + TX on whichever medium the original used. Route through the
	// txRF/txIS helpers so the dashboard sees each retry as a TX frame
	// (otherwise retries TX silently and the operator never sees them).
	info := []byte(aprs.MessageInfo(e.Dest, e.Body, e.MsgID))
	dstCall := state.DefaultBeaconDest
	if e.ViaRF {
		_ = s.txRF(e.Source, dstCall, []string{"WIDE1-1"}, info)
	}
	if e.ViaIS {
		_ = s.txIS(e.Source, dstCall, info)
	}

	e.Attempts++
	if e.Attempts >= retryMaxAttempts {
		// All attempts spent. Mark failed in DB and drop from queue.
		_ = s.store.SetMessageState(e.ID, "failed", e.Attempts)
		s.retries.Remove(e.ID)
		log.Printf("retry: msg %d to %s exhausted (%d attempts, no ack)", e.ID, e.Dest, e.Attempts)
		return
	}
	// Schedule next attempt. Persist current attempt count so the chat UI
	// shows "retry N/5" without needing the in-memory state.
	e.NextRetry = time.Now().Add(retryInterval)
	_ = s.store.SetMessageState(e.ID, "pending", e.Attempts)
}

// enqueueRetry is the entry-point used by handleMessageSend right after the
// initial TX: registers the just-sent message in the retry queue with
// attempts=1 (we count the original send) and a 30s window before the
// first retry would fire.
func (s *Server) enqueueRetry(m store.Message) {
	s.retries.Add(&retryEntry{
		ID:        m.ID,
		Source:    m.Source,
		Dest:      m.Dest,
		MsgID:     m.MsgID,
		Body:      m.Body,
		ViaRF:     m.ViaRF,
		ViaIS:     m.ViaIS,
		Attempts:  1,
		NextRetry: time.Now().Add(retryInterval),
	})
}
