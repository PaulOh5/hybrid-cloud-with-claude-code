package config

import (
	"strings"
	"testing"
)

// Phase 2 Task 0.4 — production must reject a short MAIN_API_INTERNAL_TOKEN
// before boot. Phase 1 only required "set in production"; tightening to a
// 32-byte minimum kills the dev-token-in-prod foot-gun.

func TestFromEnv_RejectsShortInternalTokenInProduction(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("MAIN_API_ADMIN_TOKEN", strings.Repeat("a", 32))
	t.Setenv("MAIN_API_AGENT_TOKEN", strings.Repeat("b", 32))
	t.Setenv("MAIN_API_INTERNAL_TOKEN", "too-short")
	t.Setenv("MAIN_API_TUNNEL_SECRET", strings.Repeat("c", 32))
	t.Setenv("MAIN_API_COOKIE_SECURE", "true")

	_, err := FromEnv()
	if err == nil {
		t.Fatalf("expected error for short internal token in production")
	}
	if !strings.Contains(err.Error(), "MAIN_API_INTERNAL_TOKEN") {
		t.Fatalf("error should mention MAIN_API_INTERNAL_TOKEN, got %q", err)
	}
}

func TestFromEnv_AcceptsLongInternalTokenInProduction(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("MAIN_API_ADMIN_TOKEN", strings.Repeat("a", 32))
	t.Setenv("MAIN_API_AGENT_TOKEN", strings.Repeat("b", 32))
	t.Setenv("MAIN_API_INTERNAL_TOKEN", strings.Repeat("d", 32))
	t.Setenv("MAIN_API_TUNNEL_SECRET", strings.Repeat("c", 32))
	t.Setenv("MAIN_API_COOKIE_SECURE", "true")

	_, err := FromEnv()
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
}

func TestFromEnv_AcceptsShortInternalTokenInDev(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("MAIN_API_ADMIN_TOKEN", "dev")
	t.Setenv("MAIN_API_AGENT_TOKEN", "dev")
	// Internal token + tunnel secret unset entirely (the existing pairing
	// check still applies); CookieSecure unset (dev).
	t.Setenv("MAIN_API_INTERNAL_TOKEN", "")
	t.Setenv("MAIN_API_TUNNEL_SECRET", "")
	t.Setenv("MAIN_API_COOKIE_SECURE", "")

	_, err := FromEnv()
	if err != nil {
		t.Fatalf("dev-mode FromEnv unexpectedly errored: %v", err)
	}
}
