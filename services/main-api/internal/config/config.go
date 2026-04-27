// Package config centralises environment-variable parsing for main-api.
package config

import (
	"errors"
	"os"
	"strconv"
	"time"
)

// Config is the fully-resolved runtime configuration.
type Config struct {
	HTTPAddr      string
	GRPCAddr      string
	DatabaseURL   string
	AdminToken    string
	AgentToken    string
	InternalToken string // /internal/* bearer token used by ssh-proxy
	TunnelSecret  []byte // HMAC key for ssh-ticket signatures (>= 16 bytes)
	TicketTTL     time.Duration
	HeartbeatTTL  time.Duration // how long after last_heartbeat we flip to offline
	SweepInterval time.Duration

	// Phase 7: user-facing auth.
	SessionTTL       time.Duration
	CookieSecure     bool
	CookieDomain     string
	TrustedProxyHops int

	// Phase 9: billing.
	BillingTick      time.Duration // 0 disables the worker
	BillingRatesPath string        // empty disables billing entirely
}

// FromEnv reads variables with sensible Phase 1 defaults.
func FromEnv() (Config, error) {
	c := Config{
		HTTPAddr:      env("MAIN_API_HTTP_ADDR", ":8080"),
		GRPCAddr:      env("MAIN_API_GRPC_ADDR", ":8081"),
		DatabaseURL:   os.Getenv("DATABASE_URL"),
		AdminToken:    os.Getenv("MAIN_API_ADMIN_TOKEN"),
		AgentToken:    os.Getenv("MAIN_API_AGENT_TOKEN"),
		InternalToken: os.Getenv("MAIN_API_INTERNAL_TOKEN"),
		TunnelSecret:  []byte(os.Getenv("MAIN_API_TUNNEL_SECRET")),
		TicketTTL:     durationEnv("MAIN_API_TICKET_TTL", 15*time.Second),
		HeartbeatTTL:  durationEnv("MAIN_API_HEARTBEAT_TTL", 60*time.Second),
		SweepInterval: durationEnv("MAIN_API_SWEEP_INTERVAL", 10*time.Second),

		SessionTTL:       durationEnv("MAIN_API_SESSION_TTL", 7*24*time.Hour),
		CookieSecure:     boolEnv("MAIN_API_COOKIE_SECURE", false),
		CookieDomain:     os.Getenv("MAIN_API_COOKIE_DOMAIN"),
		TrustedProxyHops: intEnv("MAIN_API_TRUSTED_PROXY_HOPS", 0),

		BillingTick:      durationEnv("MAIN_API_BILLING_TICK", 30*time.Second),
		BillingRatesPath: os.Getenv("MAIN_API_BILLING_RATES_PATH"),
	}

	if c.DatabaseURL == "" {
		return c, errors.New("DATABASE_URL is required")
	}
	if c.AdminToken == "" {
		return c, errors.New("MAIN_API_ADMIN_TOKEN is required")
	}
	if c.AgentToken == "" {
		return c, errors.New("MAIN_API_AGENT_TOKEN is required")
	}
	// Internal token + tunnel secret are optional in dev (CookieSecure=false)
	// so localhost bring-up doesn't need every variable wired. Production
	// (CookieSecure=true) must have both: the ssh-ticket endpoint is the
	// only way users reach their VMs, and silently disabling it would
	// look like a healthy boot but every SSH attempt would fail at runtime.
	if (c.InternalToken == "") != (len(c.TunnelSecret) == 0) {
		return c, errors.New("MAIN_API_INTERNAL_TOKEN and MAIN_API_TUNNEL_SECRET must be set together")
	}
	if c.CookieSecure && c.InternalToken == "" {
		return c, errors.New("MAIN_API_INTERNAL_TOKEN and MAIN_API_TUNNEL_SECRET are required when MAIN_API_COOKIE_SECURE=true (production)")
	}
	if len(c.TunnelSecret) > 0 && len(c.TunnelSecret) < 16 {
		return c, errors.New("MAIN_API_TUNNEL_SECRET must be at least 16 bytes")
	}
	// Phase 2 (Task 0.4): production must reject a short internal token. The
	// dev path (CookieSecure=false) still allows short or empty tokens for
	// localhost bring-up.
	if c.CookieSecure && len(c.InternalToken) < 32 {
		return c, errors.New("MAIN_API_INTERNAL_TOKEN must be at least 32 bytes in production")
	}
	return c, nil
}

func env(k, fallback string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return fallback
}

func boolEnv(k string, fallback bool) bool {
	v := os.Getenv(k)
	if v == "" {
		return fallback
	}
	if b, err := strconv.ParseBool(v); err == nil {
		return b
	}
	return fallback
}

func intEnv(k string, fallback int) int {
	v := os.Getenv(k)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}

func durationEnv(k string, fallback time.Duration) time.Duration {
	v := os.Getenv(k)
	if v == "" {
		return fallback
	}
	if d, err := time.ParseDuration(v); err == nil {
		return d
	}
	if secs, err := strconv.Atoi(v); err == nil {
		return time.Duration(secs) * time.Second
	}
	return fallback
}
