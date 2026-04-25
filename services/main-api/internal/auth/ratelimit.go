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
	mu     sync.Mutex
	hits   map[string][]time.Time
	limit  int
	window time.Duration
	now    func() time.Time
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
	if len(fresh) >= r.limit {
		r.hits[key] = fresh
		return false
	}
	r.hits[key] = append(fresh, now)
	return true
}
