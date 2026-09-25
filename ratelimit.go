package assistant

import (
	"sync"
	"time"
)

// sessionRateLimiter is a small in-memory sliding-window limiter for
// per-tenant per-session chat messages. Deliberately unsophisticated:
// one process, one map, one mutex. Multi-replica deployments get a
// per-replica budget, which is acceptable for an abuse valve — a
// coordinated limiter would drag Redis into the kernel for a knob
// most tenants leave at 0 (unlimited).
type sessionRateLimiter struct {
	mu   sync.Mutex
	hits map[string][]time.Time
	now  func() time.Time
}

func newSessionRateLimiter() *sessionRateLimiter {
	return &sessionRateLimiter{
		hits: map[string][]time.Time{},
		now:  func() time.Time { return time.Now().UTC() },
	}
}

// Allow records a hit for key and reports whether it stays within
// limit hits per minute. limit <= 0 means unlimited (nothing is even
// recorded). Stale keys are pruned opportunistically so the map does
// not grow with every session ever seen.
func (l *sessionRateLimiter) Allow(key string, limit int) bool {
	if limit <= 0 {
		return true
	}
	now := l.now()
	cutoff := now.Add(-time.Minute)

	l.mu.Lock()
	defer l.mu.Unlock()

	// Opportunistic global prune: once the map is large, drop keys
	// whose newest hit is stale. Amortised across calls.
	if len(l.hits) > 4096 {
		for k, ts := range l.hits {
			if len(ts) == 0 || ts[len(ts)-1].Before(cutoff) {
				delete(l.hits, k)
			}
		}
	}

	ts := l.hits[key]
	// Drop entries outside the window.
	i := 0
	for i < len(ts) && ts[i].Before(cutoff) {
		i++
	}
	ts = ts[i:]
	if len(ts) >= limit {
		l.hits[key] = ts
		return false
	}
	l.hits[key] = append(ts, now)
	return true
}
