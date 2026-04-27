package muxserver

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/yamux"

	"hybridcloud/shared/muxconfig"
)

// Registry is the per-node mux session registry the muxserver hands
// authenticated sessions to. Phase 2.1 Task 1.2 implements the production
// type; Task 1.1 only consumes the interface.
//
// Register is called with the agent-reported version (so dashboards can
// see drift before the next Heartbeat). The previous session for the same
// node is returned so the caller can close it after registering the new
// one — Task 1.2 specifies that ghost-session cleanup happens inside
// Register, but exposing the previous session keeps muxserver's contract
// honest if a future Registry implementation chooses to defer the close.
type Registry interface {
	Register(nodeID string, session *yamux.Session, agentVersion string) *yamux.Session
}

// Metrics receives operational signals. nil disables them.
type Metrics interface {
	AuthFailure(reason string)
	SessionAccepted()
}

// Deps wires the dynamic dependencies for Serve.
type Deps struct {
	// TLSConfig must carry server certificates. MinVersion is force-bumped
	// to TLS 1.3 inside Serve (S1) — explicit lower values are a config
	// bug and overridden silently to keep production safe.
	TLSConfig *tls.Config
	// Verifier authenticates each agent's auth header.
	Verifier Verifier
	// Registry receives accepted yamux sessions. Required.
	Registry Registry
	// Metrics is optional. nil disables operational signals.
	Metrics Metrics
	// Log is required.
	Log *slog.Logger
	// HandshakeTimeout caps the time per connection from accept to
	// authenticated yamux session. Default 30s.
	HandshakeTimeout time.Duration
}

// authReasonTLSDowngrade is the metrics label for handshakes refused by
// the TLS 1.3 enforcement (S1). Tests assert on this exact string.
const (
	authReasonTLSDowngrade   = "tls_downgrade"
	authReasonBadHeader      = "bad_header"
	authReasonUnauthed       = "unauthenticated"
	authReasonAuthCallFailed = "auth_call_failed"
)

// Serve accepts connections on lis, performs the TLS 1.3 + auth-header
// handshake, hands the resulting yamux server session to deps.Registry,
// and runs each agent in its own goroutine. Returns when ctx is cancelled
// or lis returns a permanent error.
func Serve(ctx context.Context, lis net.Listener, deps Deps) error {
	if err := validateDeps(deps); err != nil {
		return err
	}
	tlsCfg := deps.TLSConfig.Clone()
	if tlsCfg.MinVersion < tls.VersionTLS13 {
		tlsCfg.MinVersion = tls.VersionTLS13
	}
	handshakeTimeout := deps.HandshakeTimeout
	if handshakeTimeout <= 0 {
		handshakeTimeout = 30 * time.Second
	}

	// Close the listener when ctx is cancelled so Accept returns. Without
	// this Serve would block forever even after shutdown is requested.
	go func() {
		<-ctx.Done()
		_ = lis.Close()
	}()

	var wg sync.WaitGroup
	defer wg.Wait()

	for {
		conn, err := lis.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			deps.Log.Warn("muxserver: accept error", "err", err)
			// Transient — keep accepting. tcp listeners report syscall
			// errors here; without backoff we'd hot-loop on EMFILE.
			time.Sleep(50 * time.Millisecond)
			continue
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			handleConn(ctx, conn, tlsCfg, handshakeTimeout, deps)
		}()
	}
}

func validateDeps(deps Deps) error {
	if deps.TLSConfig == nil || len(deps.TLSConfig.Certificates) == 0 && deps.TLSConfig.GetCertificate == nil {
		return errors.New("muxserver: TLSConfig with certificates required")
	}
	if deps.Verifier == nil {
		return errors.New("muxserver: Verifier required")
	}
	if deps.Registry == nil {
		return errors.New("muxserver: Registry required")
	}
	if deps.Log == nil {
		return errors.New("muxserver: Log required")
	}
	return nil
}

// handleConn drives one accepted TCP connection through TLS handshake,
// auth header read, Verifier call, yamux Server init, and Registry
// handoff. Any failure closes the connection and bumps a metric reason.
func handleConn(ctx context.Context, raw net.Conn, tlsCfg *tls.Config, handshakeTimeout time.Duration, deps Deps) {
	deadline := time.Now().Add(handshakeTimeout)
	_ = raw.SetDeadline(deadline)

	tlsConn := tls.Server(raw, tlsCfg)
	hsCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	if err := tlsConn.HandshakeContext(hsCtx); err != nil {
		_ = tlsConn.Close()
		reason := classifyHandshakeError(err)
		failure(deps, reason, "tls handshake failed", err, "")
		return
	}

	req, err := readAuthHeader(tlsConn)
	if err != nil {
		_ = tlsConn.Close()
		failure(deps, authReasonBadHeader, "auth header read failed", err, "")
		return
	}

	authCtx, authCancel := context.WithDeadline(ctx, deadline)
	defer authCancel()
	result, err := deps.Verifier.Verify(authCtx, req)
	if err != nil {
		_ = tlsConn.Close()
		if errors.Is(err, ErrUnauthenticated) {
			failure(deps, authReasonUnauthed, "agent rejected", err, req.NodeID)
			return
		}
		failure(deps, authReasonAuthCallFailed, "auth upstream failed", err, req.NodeID)
		return
	}

	// Clear deadline before handing off to yamux — long-lived session.
	_ = raw.SetDeadline(time.Time{})

	sess, err := yamux.Server(tlsConn, muxconfig.Config())
	if err != nil {
		_ = tlsConn.Close()
		failure(deps, "yamux_init_failed", "yamux init failed", err, result.NodeID)
		return
	}

	prev := deps.Registry.Register(result.NodeID, sess, result.AgentVersionSeen)
	if prev != nil {
		_ = prev.Close()
		deps.Log.Info("muxserver: replaced ghost session", "node_id", result.NodeID)
	}
	if deps.Metrics != nil {
		deps.Metrics.SessionAccepted()
	}
	deps.Log.Info("muxserver: agent attached",
		"node_id", result.NodeID,
		"access_policy", result.AccessPolicy,
		"agent_version", result.AgentVersionSeen,
	)
}

// readAuthHeader reads one JSON line from the TLS connection. The line is
// bounded to 4 KiB so a misbehaving agent cannot exhaust memory.
func readAuthHeader(conn net.Conn) (AuthRequest, error) {
	br := bufio.NewReaderSize(conn, 4<<10)
	line, err := br.ReadBytes('\n')
	if err != nil && err != io.EOF {
		return AuthRequest{}, err
	}
	if len(line) == 0 {
		return AuthRequest{}, errors.New("empty header")
	}
	var req AuthRequest
	if err := json.Unmarshal(line, &req); err != nil {
		return AuthRequest{}, err
	}
	if req.NodeID == "" || req.Token == "" {
		return AuthRequest{}, errors.New("missing node_id or token")
	}
	return req, nil
}

// classifyHandshakeError maps TLS handshake failures to a metrics reason.
// TLS 1.2/1.0/1.1 clients hit "no compatible versions" — that's the
// tls_downgrade signal called out in plan §S1.
func classifyHandshakeError(err error) string {
	msg := err.Error()
	if strings.Contains(msg, "tls: client offered only unsupported versions") ||
		strings.Contains(msg, "tls: no application protocol") ||
		strings.Contains(msg, "protocol version not supported") {
		return authReasonTLSDowngrade
	}
	if strings.Contains(msg, "EOF") {
		// Older TLS client libraries close abruptly when the server
		// won't downgrade. Report as tls_downgrade so the dashboard
		// signal is unified.
		return authReasonTLSDowngrade
	}
	return "tls_handshake_failed"
}

func failure(deps Deps, reason, msg string, err error, nodeID string) {
	if deps.Metrics != nil {
		deps.Metrics.AuthFailure(reason)
	}
	deps.Log.Warn("muxserver: "+msg, "reason", reason, "err", err, "node_id", nodeID)
}
