package auth

import (
	"sync"
	"time"
)

// LoginRateLimit is the per-IP rate the spec allows: "login IP별 5회/분".
const LoginRateLimit = 5

// LoginRateWindow is the sliding window for the limiter.
const LoginRateWindow = time.Minute

// RateLimiter is a tiny per-key sliding-window counter. Phase 7 only needs
// it for the login path; if more endpoints become rate-limited later we can
// reach for a real library.
type RateLimiter struct {
	mu       sync.Mutex
	hits     map[string][]time.Time
	limit    int
	window   time.Duration
	now      func() time.Time
	lastSwep time.Time
}

// NewRateLimiter returns a limiter with limit/window defaults.
func NewRateLimiter(limit int, window time.Duration) *RateLimiter {
	return &RateLimiter{
		hits:   make(map[string][]time.Time),
		limit:  limit,
		window: window,
		now:    time.Now,
	}
}

// Allow returns true when key has fewer than limit hits in the trailing
// window. Records the hit when allowed.
func (r *RateLimiter) Allow(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	cutoff := now.Add(-r.window)

	// Drop hits older than cutoff.
	old := r.hits[key]
	fresh := old[:0]
	for _, t := range old {
		if t.After(cutoff) {
			fresh = append(fresh, t)
		}
	}
	allowed := len(fresh) < r.limit
	if allowed {
		fresh = append(fresh, now)
	}
	if len(fresh) == 0 {
		// Don't keep a zero-length slot; lets sweepLocked reclaim the key.
		delete(r.hits, key)
	} else {
		r.hits[key] = fresh
	}

	// Periodically reclaim entries that have aged out entirely. Without
	// this an attacker rotating source IPs (or X-Forwarded-For values)
	// would grow the map unboundedly. Sweep at most once per window so
	// the cost is amortised across many Allow calls.
	if now.Sub(r.lastSwep) >= r.window {
		r.sweepLocked(cutoff)
		r.lastSwep = now
	}
	return allowed
}

// sweepLocked drops keys whose every recorded hit is older than cutoff. The
// caller must hold r.mu.
func (r *RateLimiter) sweepLocked(cutoff time.Time) {
	for k, ts := range r.hits {
		stale := true
		for _, t := range ts {
			if t.After(cutoff) {
				stale = false
				break
			}
		}
		if stale {
			delete(r.hits, k)
		}
	}
}
