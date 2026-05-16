package server

import (
	"sync"
	"time"
)

// sourceRateLimiter is a per-source token bucket used to throttle how often
// any one station's frames flow through our gate or digipeater. Caps default
// to APRS community norms (aprx defaults): 6/min average, 60/min burst.
//
// A misbehaving station beaconing every second is detected and dropped here
// rather than amplified onto IS or RF.
type sourceRateLimiter struct {
	mu       sync.Mutex
	buckets  map[string]*sourceBucket
	perMin   int // sustained rate in events per minute
	burstMax int // ceiling within any 60-second window
}

type sourceBucket struct {
	count  int
	window time.Time // start of current 60s window
}

func newSourceRateLimiter(perMin, burstMax int) *sourceRateLimiter {
	return &sourceRateLimiter{
		buckets:  make(map[string]*sourceBucket),
		perMin:   perMin,
		burstMax: burstMax,
	}
}

// Allow returns true if `source` may proceed; false if it's been over-limit
// in the last 60 seconds.
func (l *sourceRateLimiter) Allow(source string) bool {
	if source == "" {
		return true
	}
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[source]
	if !ok || now.Sub(b.window) >= time.Minute {
		l.buckets[source] = &sourceBucket{count: 1, window: now}
		// Opportunistic cleanup of stale buckets.
		if len(l.buckets) > 1024 {
			for k, v := range l.buckets {
				if now.Sub(v.window) > 5*time.Minute {
					delete(l.buckets, k)
				}
			}
		}
		return true
	}
	b.count++
	return b.count <= l.burstMax
}
