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
	// Internal token + tunnel secret are optional; missing means the ssh
	// ticket endpoint is disabled. Enforced together so an operator can't
	// accidentally ship a signer without auth.
	if (c.InternalToken == "") != (len(c.TunnelSecret) == 0) {
		return c, errors.New("MAIN_API_INTERNAL_TOKEN and MAIN_API_TUNNEL_SECRET must be set together")
	}
	if len(c.TunnelSecret) > 0 && len(c.TunnelSecret) < 16 {
		return c, errors.New("MAIN_API_TUNNEL_SECRET must be at least 16 bytes")
	}
	return c, nil
}

func env(k, fallback string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return fallback
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
