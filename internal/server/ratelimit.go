package server

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// loginLimiter is a per-IP failed-attempt counter with a sliding window.
// A successful login resets the count for that IP.
type loginLimiter struct {
	mu      sync.Mutex
	entries map[string]*limiterEntry
}

type limiterEntry struct {
	fails    int
	firstAt  time.Time
	lockedAt time.Time
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{entries: make(map[string]*limiterEntry)}
}

const (
	maxFailsPerWindow = 5
	failWindow        = 1 * time.Minute
	lockoutDuration   = 10 * time.Minute
)

// Allow returns true if the IP may attempt a login right now.
func (l *loginLimiter) Allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.entries[ip]
	if !ok {
		return true
	}
	if !e.lockedAt.IsZero() && time.Since(e.lockedAt) < lockoutDuration {
		return false
	}
	// Expired lockout — reset.
	if !e.lockedAt.IsZero() {
		delete(l.entries, ip)
	}
	return true
}

// Fail records a failed attempt and locks the IP if it crosses the threshold.
func (l *loginLimiter) Fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	e, ok := l.entries[ip]
	if !ok || now.Sub(e.firstAt) > failWindow {
		l.entries[ip] = &limiterEntry{fails: 1, firstAt: now}
		return
	}
	e.fails++
	if e.fails >= maxFailsPerWindow {
		e.lockedAt = now
	}
}

// Success clears the counter for an IP after a successful login.
func (l *loginLimiter) Success(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, ip)
}

// clientIP extracts the remote IP, honoring X-Forwarded-For if and only if the
// server is reached on loopback (i.e. behind a trusted local reverse proxy).
// Otherwise uses RemoteAddr directly.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
