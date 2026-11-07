package api

import (
	"sync"
	"time"
)

const loginWindow = 15 * time.Minute

// rateLimiter counts failures per key in a fixed window. It is in-memory, which is enough
// for a single panel process; a multi-instance panel will move this into PostgreSQL.
type rateLimiter struct {
	mu      sync.Mutex
	limit   int
	window  time.Duration
	entries map[string]*rateEntry
	lastGC  time.Time
	now     func() time.Time
}

type rateEntry struct {
	count int
	start time.Time
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	return &rateLimiter{limit: limit, window: window, entries: map[string]*rateEntry{}, now: time.Now}
}

// Blocked reports whether key has used up its failures for the current window.
func (l *rateLimiter) Blocked(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.entries[key]
	return e != nil && l.now().Sub(e.start) < l.window && e.count >= l.limit
}

func (l *rateLimiter) Fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if now.Sub(l.lastGC) > l.window {
		for k, e := range l.entries {
			if now.Sub(e.start) >= l.window {
				delete(l.entries, k)
			}
		}
		l.lastGC = now
	}
	e := l.entries[key]
	if e == nil || now.Sub(e.start) >= l.window {
		l.entries[key] = &rateEntry{count: 1, start: now}
		return
	}
	e.count++
}

func (l *rateLimiter) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, key)
}
