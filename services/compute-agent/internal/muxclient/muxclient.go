// Package muxclient maintains the compute-agent side of the Phase 2
// data plane: an outbound yamux/TLS session against ssh-proxy's mux
// endpoint. The session is opened once main-api has handed back a
// node_id (so we know which identity to claim in the auth header) and
// is held for the agent's lifetime, reconnecting with exponential
// backoff on any failure.
//
// Auth header format (one JSON line, terminated by \n):
//
//	{"node_id":"<uuid>","token":"<plain>","agent_version":"<ver>"}
//
// On successful yamux session, OnAttach is invoked synchronously on
// the run goroutine. Phase 2.2 will plug a stream handler in via that
// callback. Until then OnAttach typically just logs.
//
// Reconnect policy: any error (dial, TLS, auth-line write, yamux init,
// session death) drops the current session and triggers a backoff. The
// previous session is closed before the new one starts so in-flight
// streams are explicitly terminated — spec §7 Always.
package muxclient

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"time"

	"github.com/hashicorp/yamux"

	"hybridcloud/shared/muxconfig"
)

// Config wires Run's dependencies and parameters.
type Config struct {
	// Endpoint is host:port of ssh-proxy's mux listener
	// (e.g. "mux.qlaud.net:443").
	Endpoint string
	// ServerName is the SNI sent in the TLS ClientHello. Must match
	// the cert SAN ssh-proxy serves.
	ServerName string
	// NodeID is the persisted UUID main-api assigned at Register.
	NodeID string
	// AgentToken is the plaintext token operator-issued via the admin
	// CLI (Phase 2 Task 3.2). Stored only in memory.
	AgentToken string
	// AgentVersion is the running agent's version string. Repeated in
	// every auth header so ssh-proxy logs / dashboards can see drift.
	AgentVersion string

	// KeepaliveInterval overrides the muxconfig default for this
	// session if set. Zero falls back to muxconfig.Config().
	KeepaliveInterval time.Duration

	// InitialBackoff / MaxBackoff bound the reconnect delay (1s / 60s
	// per plan). Tests inject smaller values.
	InitialBackoff time.Duration
	MaxBackoff     time.Duration

	// HandshakeTimeout caps TLS + auth header write per attempt.
	HandshakeTimeout time.Duration

	// OnAttach is invoked synchronously on the run loop after each
	// successful session attach. Phase 2.2 wires the stream handler
	// here. Required (Run errors if nil).
	OnAttach func(*yamux.Session)

	// RootCAs verifies the ssh-proxy server cert. Production wires the
	// system CA pool (or a pinned CA). When nil with
	// InsecureSkipVerify=false, TLS uses the host's default verifier.
	RootCAs            *x509.CertPool
	InsecureSkipVerify bool

	// Dial is optional. Default uses net.Dialer with the configured
	// HandshakeTimeout for the underlying TCP connect.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)

	Log *slog.Logger
}

// Run blocks until ctx is cancelled, perpetually attaching a fresh yamux
// session to the configured endpoint. Returns ctx.Err() (or nil) on
// shutdown — never returns a non-context error during steady-state
// operation, since failures are absorbed by the reconnect loop.
func Run(ctx context.Context, cfg Config) error {
	if err := validate(&cfg); err != nil {
		return err
	}

	backoff := cfg.InitialBackoff
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		sess, err := dialAndAuth(ctx, cfg)
		if err != nil {
			cfg.Log.Warn("muxclient: connect failed",
				"err", err,
				"endpoint", cfg.Endpoint,
				"next_backoff", backoff,
			)
			if !sleep(ctx, jitter(backoff)) {
				return ctx.Err()
			}
			backoff = nextBackoff(backoff, cfg.MaxBackoff)
			continue
		}

		// Reset backoff after each successful attach so a healthy
		// node that briefly drops doesn't ramp into a 60s wait.
		backoff = cfg.InitialBackoff
		cfg.Log.Info("muxclient: attached",
			"endpoint", cfg.Endpoint,
			"node_id", cfg.NodeID,
		)
		cfg.OnAttach(sess)

		// Block until the session dies or ctx cancels. Either path
		// closes the session and triggers the reconnect.
		select {
		case <-sess.CloseChan():
		case <-ctx.Done():
		}
		_ = sess.Close()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		cfg.Log.Warn("muxclient: session ended, reconnecting",
			"endpoint", cfg.Endpoint,
		)
	}
}

func validate(cfg *Config) error {
	if cfg.Endpoint == "" {
		return errors.New("muxclient: Endpoint required")
	}
	if cfg.NodeID == "" {
		return errors.New("muxclient: NodeID required")
	}
	if cfg.AgentToken == "" {
		return errors.New("muxclient: AgentToken required")
	}
	if cfg.OnAttach == nil {
		return errors.New("muxclient: OnAttach required")
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.InitialBackoff <= 0 {
		cfg.InitialBackoff = time.Second
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = 60 * time.Second
	}
	if cfg.HandshakeTimeout <= 0 {
		cfg.HandshakeTimeout = 10 * time.Second
	}
	if cfg.ServerName == "" {
		// Default to the hostname portion of Endpoint. Helps the
		// common case where Endpoint and SNI match.
		host, _, err := net.SplitHostPort(cfg.Endpoint)
		if err == nil {
			cfg.ServerName = host
		}
	}
	if cfg.Dial == nil {
		dialer := &net.Dialer{Timeout: cfg.HandshakeTimeout}
		cfg.Dial = dialer.DialContext
	}
	return nil
}

// dialAndAuth runs one connect attempt: TCP dial, TLS handshake, send
// auth header, init yamux Client. Any failure closes intermediate
// resources and returns a wrapped error so the run loop can log a
// stable reason.
func dialAndAuth(ctx context.Context, cfg Config) (*yamux.Session, error) {
	dialCtx, cancel := context.WithTimeout(ctx, cfg.HandshakeTimeout)
	defer cancel()

	raw, err := cfg.Dial(dialCtx, "tcp", cfg.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", cfg.Endpoint, err)
	}

	tlsConn := tls.Client(raw, &tls.Config{
		ServerName:         cfg.ServerName,
		MinVersion:         tls.VersionTLS13,
		RootCAs:            cfg.RootCAs,
		InsecureSkipVerify: cfg.InsecureSkipVerify, //nolint:gosec // dev/test only when set
	})
	if err := tlsConn.HandshakeContext(dialCtx); err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("tls handshake: %w", err)
	}

	if err := writeAuthHeader(tlsConn, cfg); err != nil {
		_ = tlsConn.Close()
		return nil, fmt.Errorf("write auth header: %w", err)
	}

	yamuxCfg := muxconfig.Config()
	if cfg.KeepaliveInterval > 0 {
		yamuxCfg.KeepAliveInterval = cfg.KeepaliveInterval
	}
	sess, err := yamux.Client(tlsConn, yamuxCfg)
	if err != nil {
		_ = tlsConn.Close()
		return nil, fmt.Errorf("yamux client: %w", err)
	}
	return sess, nil
}

func writeAuthHeader(conn net.Conn, cfg Config) error {
	header, err := json.Marshal(struct {
		NodeID       string `json:"node_id"`
		Token        string `json:"token"`
		AgentVersion string `json:"agent_version"`
	}{
		NodeID:       cfg.NodeID,
		Token:        cfg.AgentToken,
		AgentVersion: cfg.AgentVersion,
	})
	if err != nil {
		return err
	}
	header = append(header, '\n')
	_ = conn.SetWriteDeadline(time.Now().Add(cfg.HandshakeTimeout))
	if _, err := conn.Write(header); err != nil {
		return err
	}
	_ = conn.SetWriteDeadline(time.Time{})
	return nil
}

func nextBackoff(cur, max time.Duration) time.Duration {
	n := cur * 2
	if n > max {
		return max
	}
	return n
}

// jitter spreads reconnect attempts. math/rand is fine here — no crypto.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	half := int64(d / 2)
	return time.Duration(int64(d) + rand.Int64N(half+1)) //nolint:gosec
}

// sleep waits for d or returns false if ctx fires first.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
