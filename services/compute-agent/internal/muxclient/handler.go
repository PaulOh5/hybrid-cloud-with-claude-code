package muxclient

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
)

// Phase 2.2 Task 2.3 — agent-side stream handler.
//
// The mux session itself is authenticated end-to-end (Phase 2.1 ADR-009),
// so any stream that lands here came from a ssh-proxy that already proved
// it can speak for the right node. The header on the first line conveys
// just the routing input the agent needs to dial the right VM:
//
//	{"instance_id":"...","vm_internal_ip":"192.168.122.47","vm_port":22,"expires_at":"..."}
//
// We do not HMAC-verify the header — that signature already lived on the
// ticket sent ssh-proxy-ward by main-api, and the mux session bind makes
// it redundant on this hop. Expiry is re-checked here as defense in
// depth so a long-held stream cannot route a stale ticket.

// streamHeader is the wire-format JSON ssh-proxy writes on each new
// stream. Field names must match services/ssh-proxy/internal/tunnelhandler
// streamHeader exactly.
type streamHeader struct {
	InstanceID   string    `json:"instance_id"`
	VMInternalIP string    `json:"vm_internal_ip"`
	VMPort       uint16    `json:"vm_port"`
	ExpiresAt    time.Time `json:"expires_at,omitempty"`
}

// RelayMetrics receives operational signals about per-stream relay
// outcomes. nil disables the counters.
type RelayMetrics interface {
	Failure(reason string)
	Success()
}

// Handler accepts inbound streams on a yamux session and relays each one
// to the local VM identified by the stream header.
type Handler struct {
	// DialTimeout bounds the time net.Dialer waits for the VM to accept
	// the TCP connection. Default 5s.
	DialTimeout time.Duration

	// IdleTimeout closes the stream + VM connection after the given
	// duration of no traffic. Default 30 minutes (Phase 1 parity).
	// Zero disables.
	IdleTimeout time.Duration

	// HeaderReadTimeout bounds the time spent reading the first JSON
	// line. Default 5s — anything longer signals a misbehaving peer.
	HeaderReadTimeout time.Duration

	// Dial is optional. Default uses net.Dialer with DialTimeout.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)

	// Metrics is optional. nil disables counters.
	Metrics RelayMetrics

	Log *slog.Logger
}

// AcceptLoop reads streams off sess and spawns a goroutine to relay each
// one. Returns when AcceptStream errors (session closed) or ctx fires.
// Safe to invoke as the goroutine fired from muxclient.OnAttach.
func (h *Handler) AcceptLoop(ctx context.Context, sess *yamux.Session) {
	if h.Log == nil {
		h.Log = slog.Default()
	}
	var wg sync.WaitGroup
	defer wg.Wait()

	for {
		stream, err := sess.AcceptStream()
		if err != nil {
			if !isClosedErr(err) {
				h.Log.Warn("muxclient handler: AcceptStream error", "err", err)
			}
			return
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.handleStream(ctx, stream)
		}()
	}
}

func isClosedErr(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) || errors.Is(err, yamux.ErrSessionShutdown)
}

// handleStream owns one inbound stream from accept to teardown.
func (h *Handler) handleStream(ctx context.Context, stream *yamux.Stream) {
	defer func() { _ = stream.Close() }()

	headerTimeout := h.HeaderReadTimeout
	if headerTimeout <= 0 {
		headerTimeout = 5 * time.Second
	}
	dialTimeout := h.DialTimeout
	if dialTimeout <= 0 {
		dialTimeout = 5 * time.Second
	}
	idleTimeout := h.IdleTimeout
	if idleTimeout < 0 {
		idleTimeout = 0
	} else if idleTimeout == 0 {
		idleTimeout = 30 * time.Minute
	}

	// bufio reader is reused for the stream→VM half of the relay.
	// Without that the bufio's read-ahead would swallow the first user
	// bytes that arrived in the same packet as the header.
	br := bufio.NewReaderSize(stream, 4<<10)
	hdr, err := readHeader(br, headerTimeout, stream)
	if err != nil {
		h.fail("bad_header", err, "")
		return
	}
	if hdr.VMInternalIP == "" || hdr.VMPort == 0 {
		h.fail("missing_vm", errors.New("vm_internal_ip / vm_port required"), hdr.InstanceID)
		return
	}
	// 1s skew tolerance, mirroring ssh-proxy.
	if !hdr.ExpiresAt.IsZero() && time.Now().After(hdr.ExpiresAt.Add(-time.Second)) {
		h.fail("expired_ticket", fmt.Errorf("ticket expired at %s", hdr.ExpiresAt.UTC().Format(time.RFC3339)), hdr.InstanceID)
		return
	}

	dial := h.Dial
	if dial == nil {
		dialer := &net.Dialer{Timeout: dialTimeout}
		dial = dialer.DialContext
	}

	addr := net.JoinHostPort(hdr.VMInternalIP, strconv.Itoa(int(hdr.VMPort)))
	dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	vmConn, err := dial(dialCtx, "tcp", addr)
	if err != nil {
		h.fail("vm_dial_failed", fmt.Errorf("dial %s: %w", addr, err), hdr.InstanceID)
		return
	}
	defer func() { _ = vmConn.Close() }()

	if h.Metrics != nil {
		h.Metrics.Success()
	}
	h.Log.Info("muxclient handler: relay started",
		"instance_id", hdr.InstanceID,
		"vm", addr,
	)

	relayBytes(ctx, br, stream, vmConn, idleTimeout)
}

// readHeader reads up to 4 KiB before the first newline. The size cap
// prevents a misbehaving ssh-proxy from exhausting agent memory. The
// caller passes the bufio reader (so any read-ahead carries into the
// relay) plus the underlying stream for SetReadDeadline.
func readHeader(br *bufio.Reader, timeout time.Duration, deadliner interface {
	SetReadDeadline(time.Time) error
}) (streamHeader, error) {
	_ = deadliner.SetReadDeadline(time.Now().Add(timeout))
	defer func() { _ = deadliner.SetReadDeadline(time.Time{}) }()

	line, err := br.ReadBytes('\n')
	if err != nil && err != io.EOF {
		return streamHeader{}, err
	}
	if len(line) == 0 {
		return streamHeader{}, errors.New("empty header")
	}
	var h streamHeader
	if err := json.Unmarshal(line, &h); err != nil {
		return streamHeader{}, fmt.Errorf("decode: %w", err)
	}
	return h, nil
}

// relayBytes pipes bytes between the yamux stream and the VM connection
// with deterministic teardown and an optional idle deadline. The stream
// reader is whatever the caller used to read the header — passing the
// bufio.Reader keeps any read-ahead bytes (e.g. a "hello\n" that arrived
// in the same packet as the header) in the data path instead of being
// silently swallowed by the bufio buffer.
func relayBytes(ctx context.Context, streamReader io.Reader, stream *yamux.Stream, vm net.Conn, idleTimeout time.Duration) {
	done := make(chan struct{}, 2)
	closeBoth := func() {
		_ = stream.Close()
		_ = vm.Close()
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
		_, _ = io.Copy(vm, streamReader)
		if cw, ok := vm.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		} else {
			_ = vm.Close()
		}
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(stream, vm)
		_ = stream.Close()
		done <- struct{}{}
	}()

	if idleTimeout > 0 {
		// Whole-session idle bound: if neither half closes within
		// idleTimeout, force-close. Phase 1 parity (30 min).
		go func() {
			t := time.NewTimer(idleTimeout)
			defer t.Stop()
			select {
			case <-t.C:
				closeBoth()
			case <-stopCtx:
			}
		}()
	}

	<-done
	closeBoth()
	<-done
}

// fail bumps the metrics counter (if configured) and logs the reason.
func (h *Handler) fail(reason string, err error, instanceID string) {
	if h.Metrics != nil {
		h.Metrics.Failure(reason)
	}
	h.Log.Warn("muxclient handler: relay failed",
		"reason", reason,
		"err", err,
		"instance_id", instanceID,
	)
}
