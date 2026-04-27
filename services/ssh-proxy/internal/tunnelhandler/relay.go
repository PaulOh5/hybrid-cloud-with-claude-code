package tunnelhandler

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"time"

	"golang.org/x/crypto/ssh"

	"hybridcloud/services/ssh-proxy/internal/ticketclient"
)

// StreamOpener is the narrow slice of muxregistry.Registry the relay needs:
// open a yamux stream toward the agent identified by node_id. Returning a
// net.Conn (rather than *yamux.Stream) keeps tests simple — they can hand
// back one side of a net.Pipe without spinning up a real yamux session.
type StreamOpener interface {
	OpenStream(ctx context.Context, nodeID string) (net.Conn, error)
}

// ticketPayload is the subset of the signed ticket the relay needs. NodeID
// drives the mux session lookup; VM fields are echoed onto the stream
// header so the agent knows where to dial inside the node. ExpiresAt is
// re-checked here so a delayed dial fails fast at the proxy.
type ticketPayload struct {
	SessionID    string    `json:"session_id"`
	InstanceID   string    `json:"instance_id"`
	NodeID       string    `json:"node_id"`
	VMInternalIP string    `json:"vm_internal_ip"`
	VMPort       uint16    `json:"vm_port"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// streamHeader is the one-line JSON the relay writes on the mux stream
// before piping bytes. Agent-side reads this, dials VMInternalIP:VMPort,
// and copies bytes both ways. No HMAC: the mux session itself is
// authenticated (Phase 2 ADR-009), so trusting the stream content is
// equivalent to trusting whoever holds the mux session — i.e. ssh-proxy.
type streamHeader struct {
	InstanceID   string    `json:"instance_id"`
	VMInternalIP string    `json:"vm_internal_ip"`
	VMPort       uint16    `json:"vm_port"`
	ExpiresAt    time.Time `json:"expires_at,omitempty"`
}

// Relayer holds the wiring needed to relay a user SSH channel onto a mux
// stream. Construct in cmd/main.go; pass Relayer.Relay as the
// Handler.AfterTicket function.
type Relayer struct {
	Opener StreamOpener
	Log    *slog.Logger

	// HeaderWriteTimeout bounds the time spent writing the stream header
	// before bidirectional copy begins. Default 5s.
	HeaderWriteTimeout time.Duration
}

// Relay implements Handler.AfterTicket. It decodes the signed ticket,
// opens a mux stream to the node, writes the agent-facing header, and
// pipes bytes between the SSH channel and the stream until either side
// closes (or ctx cancels).
func (r *Relayer) Relay(ctx context.Context, _ string, signed ticketclient.Signed, ch ssh.Channel) error {
	log := r.log()
	if r.Opener == nil {
		_ = ch.Close()
		return errors.New("tunnelhandler: no StreamOpener configured")
	}
	headerWriteTimeout := r.HeaderWriteTimeout
	if headerWriteTimeout <= 0 {
		headerWriteTimeout = 5 * time.Second
	}

	raw, err := base64.StdEncoding.DecodeString(signed.Payload)
	if err != nil {
		_ = ch.Close()
		return fmt.Errorf("decode ticket payload: %w", err)
	}
	var p ticketPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		_ = ch.Close()
		return fmt.Errorf("parse ticket payload: %w", err)
	}
	if p.NodeID == "" {
		_ = ch.Close()
		return errors.New("ticket missing node_id")
	}
	if p.VMInternalIP == "" || p.VMPort == 0 {
		_ = ch.Close()
		return errors.New("ticket missing vm address")
	}
	// 1s skew tolerance so an in-flight dial doesn't trip on boundary
	// timing. Any larger gap means the ticket is genuinely stale.
	if !p.ExpiresAt.IsZero() && time.Now().After(p.ExpiresAt.Add(-time.Second)) {
		_ = ch.Close()
		return fmt.Errorf("ticket expired at %s", p.ExpiresAt.UTC().Format(time.RFC3339))
	}

	stream, err := r.Opener.OpenStream(ctx, p.NodeID)
	if err != nil {
		_ = ch.Close()
		log.Warn("muxregistry: open stream failed",
			"node_id", p.NodeID,
			"session_id", p.SessionID,
			"err", err,
		)
		return fmt.Errorf("open mux stream for node %s: %w", p.NodeID, err)
	}

	// Write the agent-facing header. yamux.Stream supports SetWriteDeadline.
	header, _ := json.Marshal(streamHeader{
		InstanceID:   p.InstanceID,
		VMInternalIP: p.VMInternalIP,
		VMPort:       p.VMPort,
		ExpiresAt:    p.ExpiresAt,
	})
	header = append(header, '\n')
	_ = stream.SetWriteDeadline(time.Now().Add(headerWriteTimeout))
	if _, err := stream.Write(header); err != nil {
		_ = stream.Close()
		_ = ch.Close()
		return fmt.Errorf("write stream header: %w", err)
	}
	_ = stream.SetWriteDeadline(time.Time{})

	// Bidirectional copy with deterministic teardown. Same shape as the
	// Phase 1 net.Conn path; only the underlying transport changed.
	done := make(chan struct{}, 2)
	closeBoth := func() {
		_ = stream.Close()
		_ = ch.Close()
	}

	stopCtx := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			closeBoth()
		case <-stopCtx:
		}
	}()
	defer close(stopCtx)

	go func() {
		_, _ = io.Copy(stream, ch)
		// Half-close: signal EOF to the agent without tearing down the
		// other direction. yamux.Stream implements CloseWrite.
		if cw, ok := stream.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		} else {
			_ = stream.Close()
		}
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(ch, stream)
		_ = ch.CloseWrite()
		done <- struct{}{}
	}()

	<-done
	closeBoth()
	<-done
	return nil
}

func (r *Relayer) log() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.Default()
}
