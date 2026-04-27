package muxregistry_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/yamux"

	"hybridcloud/services/ssh-proxy/internal/muxregistry"
	"hybridcloud/shared/muxconfig"
)

// pairSession spins up a client+server yamux session over a net.Pipe so
// tests can register the server side and exercise OpenStream. Returning
// closeFn lets the test simulate connection death.
func pairSession(t *testing.T) (server *yamux.Session, client *yamux.Session, closeFn func()) {
	t.Helper()
	a, b := net.Pipe()

	cfg := muxconfig.Config()

	type result struct {
		s   *yamux.Session
		err error
	}
	srvCh := make(chan result, 1)
	go func() {
		s, err := yamux.Server(a, cfg)
		srvCh <- result{s, err}
	}()

	cli, err := yamux.Client(b, muxconfig.Config())
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	srv := <-srvCh
	if srv.err != nil {
		t.Fatalf("server: %v", srv.err)
	}

	closeFn = func() {
		_ = cli.Close()
		_ = srv.s.Close()
		_ = a.Close()
		_ = b.Close()
	}
	t.Cleanup(closeFn)
	return srv.s, cli, closeFn
}

type fakeReporter struct {
	mu      sync.Mutex
	calls   []string
	calls64 atomic.Int64
}

func (f *fakeReporter) ReportDegraded(_ context.Context, nodeID string) error {
	f.calls64.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, nodeID)
	return nil
}

func (f *fakeReporter) snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.calls))
	copy(out, f.calls)
	return out
}

func newRegistry(t *testing.T, reporter muxregistry.NodeStateReporter) *muxregistry.Registry {
	t.Helper()
	if reporter == nil {
		reporter = &fakeReporter{}
	}
	r := muxregistry.New(muxregistry.Config{
		Reporter:     reporter,
		PingInterval: 100 * time.Millisecond,
		PingTimeout:  500 * time.Millisecond,
		Log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	t.Cleanup(func() { r.Close() })
	return r
}

func TestRegistry_RegisterAndOpenStream(t *testing.T) {
	t.Parallel()

	srvSess, _, _ := pairSession(t)
	r := newRegistry(t, nil)

	prev := r.Register("node-A", srvSess, "0.2.0")
	if prev != nil {
		t.Fatalf("first register returned prev=%v, want nil", prev)
	}

	stream, err := r.OpenStream(context.Background(), "node-A")
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	defer func() { _ = stream.Close() }()
}

func TestRegistry_ReregisterClosesPrev(t *testing.T) {
	t.Parallel()

	srv1, _, _ := pairSession(t)
	srv2, _, _ := pairSession(t)
	r := newRegistry(t, nil)

	r.Register("node-A", srv1, "0.2.0")
	prev := r.Register("node-A", srv2, "0.2.1")

	if prev != srv1 {
		t.Fatalf("expected previous session to be returned for cleanup logging")
	}
	// Registry must close prev internally.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if srv1.IsClosed() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !srv1.IsClosed() {
		t.Fatalf("previous session should be closed by Registry on re-register")
	}
	if srv2.IsClosed() {
		t.Fatalf("new session must not be closed")
	}
}

func TestRegistry_OpenStreamUnknownNode(t *testing.T) {
	t.Parallel()

	r := newRegistry(t, nil)
	_, err := r.OpenStream(context.Background(), "nope")
	if err == nil {
		t.Fatal("OpenStream should error for unknown node")
	}
	if !errors.Is(err, muxregistry.ErrUnknownNode) {
		t.Fatalf("expected ErrUnknownNode, got %v", err)
	}
}

func TestRegistry_OpenStreamDeadSession(t *testing.T) {
	t.Parallel()

	srvSess, _, closeFn := pairSession(t)
	r := newRegistry(t, nil)

	r.Register("node-A", srvSess, "0.2.0")
	closeFn() // sever the underlying pipe

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if srvSess.IsClosed() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	_, err := r.OpenStream(context.Background(), "node-A")
	if err == nil {
		t.Fatal("OpenStream should error after session close")
	}
}

func TestRegistry_PingFailureAutoDeregister(t *testing.T) {
	t.Parallel()

	srvSess, _, closeFn := pairSession(t)
	reporter := &fakeReporter{}
	r := newRegistry(t, reporter)

	r.Register("node-A", srvSess, "0.2.0")
	closeFn() // kill the session — pings will fail next tick

	// Wait for the health checker to notice and deregister.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if reporter.calls64.Load() == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	calls := reporter.snapshot()
	if len(calls) != 1 || calls[0] != "node-A" {
		t.Fatalf("ReportDegraded calls: %v", calls)
	}

	// OpenStream should now fail with ErrUnknownNode (deregistered).
	if _, err := r.OpenStream(context.Background(), "node-A"); !errors.Is(err, muxregistry.ErrUnknownNode) {
		t.Fatalf("expected ErrUnknownNode after auto-deregister, got %v", err)
	}
}

func TestRegistry_ConcurrentReregisterNoGhost(t *testing.T) {
	t.Parallel()

	const N = 50
	r := newRegistry(t, nil)

	// Pre-create N sessions to register one after another. Track them
	// so the assertion at the end can check all but the last are closed.
	sessions := make([]*yamux.Session, N)
	for i := 0; i < N; i++ {
		s, _, _ := pairSession(t)
		sessions[i] = s
	}

	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			r.Register("node-A", sessions[idx], "v")
		}(i)
	}
	wg.Wait()

	// At most one of the sessions remains open (the one Register
	// processed last). Every other session must be closed by the
	// registry's re-register cleanup.
	open := 0
	for _, s := range sessions {
		if !s.IsClosed() {
			open++
		}
	}
	if open != 1 {
		t.Fatalf("ghost sessions detected: %d sessions still open after concurrent register, want 1", open)
	}
}
