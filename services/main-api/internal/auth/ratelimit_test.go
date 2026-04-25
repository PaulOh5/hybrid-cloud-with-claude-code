package auth

import (
	"testing"
	"time"
)

func TestRateLimiter_AllowsBelowLimit(t *testing.T) {
	t.Parallel()
	rl := NewRateLimiter(5, time.Minute)
	for i := 0; i < 5; i++ {
		if !rl.Allow("1.2.3.4") {
			t.Fatalf("hit %d should be allowed", i)
		}
	}
	if rl.Allow("1.2.3.4") {
		t.Fatal("6th hit should be denied")
	}
}

func TestRateLimiter_PerKeyIndependent(t *testing.T) {
	t.Parallel()
	rl := NewRateLimiter(2, time.Minute)
	if !rl.Allow("a") {
		t.Fatal("a hit 1")
	}
	if !rl.Allow("a") {
		t.Fatal("a hit 2")
	}
	if rl.Allow("a") {
		t.Fatal("a hit 3 should deny")
	}
	if !rl.Allow("b") {
		t.Fatal("b should be independent of a")
	}
}

func TestRateLimiter_WindowResets(t *testing.T) {
	t.Parallel()
	rl := NewRateLimiter(2, time.Minute)
	base := time.Now()
	rl.now = func() time.Time { return base }
	rl.Allow("a")
	rl.Allow("a")
	if rl.Allow("a") {
		t.Fatal("third hit denied")
	}
	rl.now = func() time.Time { return base.Add(2 * time.Minute) }
	if !rl.Allow("a") {
		t.Fatal("after window expiry should reset")
	}
}
