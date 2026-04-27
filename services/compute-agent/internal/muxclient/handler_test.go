package muxclient_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/yamux"

	"hybridcloud/services/compute-agent/internal/muxclient"
	"hybridcloud/shared/muxconfig"
)

// pairYamux spins up a server+client yamux pair over a net.Pipe so tests
// can register the server side with the Handler and open streams from
// the client side as the simulated ssh-proxy.
func pairYamux(t *testing.T) (server, client *yamux.Session) {
	t.Helper()
	a, b := net.Pipe()
	cfg := muxconfig.Config()

	srvCh := make(chan *yamux.Session, 1)
	go func() {
		s, err := yamux.Server(a, cfg)
		if err != nil {
			t.Errorf("yamux.Server: %v", err)
			return
		}
		srvCh <- s
	}()
	cli, err := yamux.Client(b, muxconfig.Config())
	if err != nil {
		t.Fatalf("yamux.Client: %v", err)
	}
	srv := <-srvCh

	t.Cleanup(func() {
		_ = cli.Close()
		_ = srv.Close()
		_ = a.Close()
		_ = b.Close()
	})
	return srv, cli
}

// fakeVM accepts one connection, echoes back what it reads. Returns the
// listener address so tests inject it as vm_internal_ip:vm_port.
func fakeVM(t *testing.T) (host string, port uint16) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() {
		for {
			c, err := lis.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				_, _ = io.Copy(c, c)
			}(c)
		}
	}()
	t.Cleanup(func() { _ = lis.Close() })

	addr := lis.Addr().(*net.TCPAddr)
	return addr.IP.String(), uint16(addr.Port)
}

type fakeMetrics struct {
	mu       sync.Mutex
	failures map[string]int
	success  atomic.Int64
}

func newFakeMetrics() *fakeMetrics {
	return &fakeMetrics{failures: map[string]int{}}
}

func (f *fakeMetrics) Failure(reason string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures[reason]++
}
func (f *fakeMetrics) Success() { f.success.Add(1) }

func (f *fakeMetrics) failureCount(reason string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.failures[reason]
}

func writeHeader(t *testing.T, stream net.Conn, hdr any) {
	t.Helper()
	body, err := json.Marshal(hdr)
	if err != nil {
		t.Fatal(err)
	}
	body = append(body, '\n')
	if _, err := stream.Write(body); err != nil {
		t.Fatalf("write header: %v", err)
	}
}

func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestHandler_RelaysToVM(t *testing.T) {
	t.Parallel()

	vmIP, vmPort := fakeVM(t)
	srvSess, cliSess := pairYamux(t)

	metrics := newFakeMetrics()
	h := &muxclient.Handler{
		DialTimeout: 2 * time.Second,
		IdleTimeout: 5 * time.Second,
		Metrics:     metrics,
		Log:         quietLog(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go h.AcceptLoop(ctx, srvSess)

	stream, err := cliSess.OpenStream()
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	defer func() { _ = stream.Close() }()

	writeHeader(t, stream, map[string]any{
		"instance_id":    "inst-1",
		"vm_internal_ip": vmIP,
		"vm_port":        vmPort,
		"expires_at":     time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano),
	})

	if _, err := stream.Write([]byte("hello\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = stream.SetReadDeadline(time.Now().Add(2 * time.Second))
	br := bufio.NewReader(stream)
	got, err := br.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if !bytes.Equal(got, []byte("hello\n")) {
		t.Fatalf("echo mismatch: %q", got)
	}

	// Wait briefly for the success counter (handler bumps it after the
	// VM dial completes successfully).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if metrics.success.Load() == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := metrics.success.Load(); got != 1 {
		t.Fatalf("success counter: got %d, want 1", got)
	}
}

func TestHandler_BadHeaderClosesStream(t *testing.T) {
	t.Parallel()

	srvSess, cliSess := pairYamux(t)
	metrics := newFakeMetrics()
	h := &muxclient.Handler{Metrics: metrics, Log: quietLog()}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go h.AcceptLoop(ctx, srvSess)

	stream, err := cliSess.OpenStream()
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	if _, err := stream.Write([]byte("not-json\n")); err != nil {
		t.Fatalf("write: %v", err)
	}

	_ = stream.SetReadDeadline(time.Now().Add(1 * time.Second))
	if _, err := stream.Read(make([]byte, 4)); err == nil {
		t.Fatal("expected stream close after bad header")
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if metrics.failureCount("bad_header") >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := metrics.failureCount("bad_header"); got < 1 {
		t.Fatalf("bad_header count: got %d, want >= 1", got)
	}
}

func TestHandler_ExpiredTicketClosesStream(t *testing.T) {
	t.Parallel()

	srvSess, cliSess := pairYamux(t)
	metrics := newFakeMetrics()
	h := &muxclient.Handler{Metrics: metrics, Log: quietLog()}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go h.AcceptLoop(ctx, srvSess)

	stream, err := cliSess.OpenStream()
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	writeHeader(t, stream, map[string]any{
		"instance_id":    "inst-1",
		"vm_internal_ip": "127.0.0.1",
		"vm_port":        9999,
		"expires_at":     time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano),
	})

	_ = stream.SetReadDeadline(time.Now().Add(1 * time.Second))
	if _, err := stream.Read(make([]byte, 4)); err == nil {
		t.Fatal("expected stream close after expired ticket")
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if metrics.failureCount("expired_ticket") >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := metrics.failureCount("expired_ticket"); got < 1 {
		t.Fatalf("expired_ticket count: got %d, want >= 1", got)
	}
}

func TestHandler_MissingVMClosesStream(t *testing.T) {
	t.Parallel()

	srvSess, cliSess := pairYamux(t)
	metrics := newFakeMetrics()
	h := &muxclient.Handler{Metrics: metrics, Log: quietLog()}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go h.AcceptLoop(ctx, srvSess)

	stream, err := cliSess.OpenStream()
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	writeHeader(t, stream, map[string]any{
		"instance_id":    "inst-1",
		"vm_internal_ip": "",
		"vm_port":        0,
	})

	_ = stream.SetReadDeadline(time.Now().Add(1 * time.Second))
	if _, err := stream.Read(make([]byte, 4)); err == nil {
		t.Fatal("expected stream close on missing vm address")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if metrics.failureCount("missing_vm") >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := metrics.failureCount("missing_vm"); got < 1 {
		t.Fatalf("missing_vm count: got %d, want >= 1", got)
	}
}

func TestHandler_VMDialFailureClosesStream(t *testing.T) {
	t.Parallel()

	// Pick a port that's almost certainly not listening.
	tmp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	deadAddr := tmp.Addr().(*net.TCPAddr)
	_ = tmp.Close()

	srvSess, cliSess := pairYamux(t)
	metrics := newFakeMetrics()
	h := &muxclient.Handler{
		DialTimeout: 200 * time.Millisecond,
		Metrics:     metrics,
		Log:         quietLog(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go h.AcceptLoop(ctx, srvSess)

	stream, err := cliSess.OpenStream()
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	writeHeader(t, stream, map[string]any{
		"instance_id":    "inst-1",
		"vm_internal_ip": deadAddr.IP.String(),
		"vm_port":        uint16(deadAddr.Port),
	})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if metrics.failureCount("vm_dial_failed") >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := metrics.failureCount("vm_dial_failed"); got < 1 {
		t.Fatalf("vm_dial_failed count: got %d, want >= 1", got)
	}
}

func TestHandler_AcceptLoopExitsOnSessionClose(t *testing.T) {
	t.Parallel()

	srvSess, cliSess := pairYamux(t)
	h := &muxclient.Handler{Metrics: newFakeMetrics(), Log: quietLog()}

	done := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		h.AcceptLoop(ctx, srvSess)
		close(done)
	}()

	// Tear down the client side; AcceptStream on the server should then
	// return an error and AcceptLoop should exit.
	_ = cliSess.Close()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("AcceptLoop did not exit within 2s of session close")
	}
}

// Sanity: errors.Is is used internally for context cancellation; keep an
// import-coupling assertion so refactors don't drop it accidentally.
var _ = errors.Is
