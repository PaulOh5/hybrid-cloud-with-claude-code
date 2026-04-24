// Package tunnel runs the compute-agent's TCP listener that ssh-proxy dials
// to relay raw SSH bytes to a running VM.
//
// Wire protocol (framed header followed by raw bytes):
//
//	<one JSON line: {"payload": "<base64>", "signature": "<base64>"}>\n
//	<raw SSH bytes both directions>
//
// The agent verifies the HMAC signature against its shared secret, parses
// the ticket, dials the VM's internal sshd, and shuttles bytes until either
// side closes.
package tunnel

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"
)

// SignedTicket is the wire envelope. Duplicated from main-api's sshticket
// package so compute-agent does not pull a main-api dependency.
type SignedTicket struct {
	Payload   string `json:"payload"`
	Signature string `json:"signature"`
}

// Ticket is the decoded body. The agent only reads VMInternalIP + VMPort +
// ExpiresAt; the other fields are for audit logs.
type Ticket struct {
	SessionID      string    `json:"session_id"`
	InstanceID     string    `json:"instance_id"`
	NodeID         string    `json:"node_id"`
	VMInternalIP   string    `json:"vm_internal_ip"`
	VMPort         uint16    `json:"vm_port"`
	TunnelEndpoint string    `json:"tunnel_endpoint"`
	ExpiresAt      time.Time `json:"expires_at"`
}

// Verifier validates SignedTickets. Decoupled from main-api's Verifier so
// tests can swap in a stub.
type Verifier interface {
	Verify(SignedTicket) (Ticket, error)
}

// Config controls Serve.
type Config struct {
	// Verifier authenticates incoming tickets.
	Verifier Verifier
	// DialTimeout bounds how long we wait for the VM sshd TCP connect.
	// Default 5s.
	DialTimeout time.Duration
	// HeaderTimeout bounds how long we wait for the JSON header line.
	// Default 5s.
	HeaderTimeout time.Duration
	Log           *slog.Logger
}

// Server is the agent's TCP tunnel listener. Bind via net.Listen and feed
// into Server.Serve; the server handles each connection in its own
// goroutine.
type Server struct {
	cfg Config
}

// New returns a Server; validates required fields.
func New(cfg Config) (*Server, error) {
	if cfg.Verifier == nil {
		return nil, errors.New("tunnel: Verifier required")
	}
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = 5 * time.Second
	}
	if cfg.HeaderTimeout <= 0 {
		cfg.HeaderTimeout = 5 * time.Second
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	return &Server{cfg: cfg}, nil
}

// Serve accepts connections on lis until ctx is cancelled.
func (s *Server) Serve(ctx context.Context, lis net.Listener) error {
	go func() {
		<-ctx.Done()
		_ = lis.Close()
	}()
	var wg sync.WaitGroup
	for {
		conn, err := lis.Accept()
		if err != nil {
			if ctx.Err() != nil {
				wg.Wait()
				return ctx.Err()
			}
			s.cfg.Log.Warn("accept", "err", err)
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.handle(ctx, conn)
		}()
	}
}

func (s *Server) handle(ctx context.Context, conn net.Conn) {
	defer func() { _ = conn.Close() }()

	_ = conn.SetReadDeadline(time.Now().Add(s.cfg.HeaderTimeout))
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		s.cfg.Log.Warn("read header", "remote", conn.RemoteAddr(), "err", err)
		return
	}
	_ = conn.SetReadDeadline(time.Time{})

	var signed SignedTicket
	if err := json.Unmarshal(line, &signed); err != nil {
		s.cfg.Log.Warn("parse header", "err", err)
		return
	}
	ticket, err := s.cfg.Verifier.Verify(signed)
	if err != nil {
		s.cfg.Log.Warn("verify ticket", "err", err)
		return
	}

	target := net.JoinHostPort(ticket.VMInternalIP, fmt.Sprintf("%d", ticket.VMPort))
	upstream, err := net.DialTimeout("tcp", target, s.cfg.DialTimeout)
	if err != nil {
		s.cfg.Log.Warn("dial vm", "target", target, "err", err)
		return
	}
	defer func() { _ = upstream.Close() }()

	s.cfg.Log.Info("tunnel established",
		"session", ticket.SessionID,
		"instance", ticket.InstanceID,
		"target", target,
	)
	relay(ctx, conn, upstream)
	s.cfg.Log.Debug("tunnel closed", "session", ticket.SessionID)
}

// relay shuttles bytes in both directions and returns when either side
// closes. Context cancellation breaks the copy loop on both halves.
func relay(ctx context.Context, a, b net.Conn) {
	done := make(chan struct{}, 2)
	go copyHalf(a, b, done)
	go copyHalf(b, a, done)
	select {
	case <-done:
	case <-ctx.Done():
	}
	_ = a.Close()
	_ = b.Close()
	<-done
}

func copyHalf(dst, src net.Conn, done chan<- struct{}) {
	_, _ = io.Copy(dst, src)
	done <- struct{}{}
}
