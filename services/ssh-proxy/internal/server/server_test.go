package server_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"hybridcloud/services/ssh-proxy/internal/server"
)

// newTestHostKey returns a throwaway ed25519 signer for server_test harness.
func newTestHostKey(t *testing.T) ssh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// recordingHandler stores every Connect invocation so tests can assert on
// the received prefix.
type recordingHandler struct {
	mu       sync.Mutex
	calls    []string
	closeErr error
	called   chan struct{} // optional: signals once per Connect for sync
}

func (h *recordingHandler) Connect(_ context.Context, prefix string, ch ssh.Channel) error {
	h.mu.Lock()
	h.calls = append(h.calls, prefix)
	h.mu.Unlock()
	// Immediately close the channel — Task 6.1 proves the routing signal is
	// correct; the actual byte relay lands in 6.3.
	_ = ch.Close()
	if h.called != nil {
		h.called <- struct{}{}
	}
	return h.closeErr
}

func (h *recordingHandler) prefixes() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.calls...)
}

// startServer spins up the server on a random port and returns the
// listener's address. Cleanup happens via t.Cleanup.
func startServer(t *testing.T, handler server.Handler) (string, *recordingHandler) {
	t.Helper()
	rec, _ := handler.(*recordingHandler)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	srv, err := server.New(server.Config{
		Zone:             "hybrid-cloud.com",
		HostKeys:         []ssh.Signer{newTestHostKey(t)},
		Handler:          handler,
		HandshakeTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = srv.Serve(ctx, lis) }()

	t.Cleanup(func() {
		cancel()
		_ = lis.Close()
	})
	return lis.Addr().String(), rec
}

func dialProxy(t *testing.T, addr string) *ssh.Client {
	t.Helper()
	client, err := ssh.Dial("tcp", addr, &ssh.ClientConfig{
		User:            "anyone",
		Auth:            nil, // NoClientAuth on server
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         3 * time.Second,
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return client
}

// --- tests -----------------------------------------------------------------

func TestDirectTCPIP_Accepted_InZone(t *testing.T) {
	t.Parallel()

	handler := &recordingHandler{called: make(chan struct{}, 1)}
	addr, rec := startServer(t, handler)

	client := dialProxy(t, addr)
	defer func() { _ = client.Close() }()

	// Dial a "forwarded" target. client.Dial returns as soon as the server
	// accepts the channel — the server-side handler runs concurrently, so
	// without the `called` signal `prefixes()` can race the append.
	conn, err := client.Dial("tcp", "abc12345.hybrid-cloud.com:22")
	if err != nil {
		t.Fatalf("client.Dial: %v", err)
	}
	_ = conn.Close()

	select {
	case <-handler.called:
	case <-time.After(2 * time.Second):
		t.Fatal("server handler did not run")
	}

	if got := rec.prefixes(); len(got) != 1 || got[0] != "abc12345" {
		t.Fatalf("prefixes: got %v, want [abc12345]", got)
	}
}

func TestDirectTCPIP_RejectedOutOfZone(t *testing.T) {
	t.Parallel()

	handler := &recordingHandler{}
	addr, rec := startServer(t, handler)

	client := dialProxy(t, addr)
	defer func() { _ = client.Close() }()

	_, err := client.Dial("tcp", "abc12345.evil.com:22")
	if err == nil {
		t.Fatal("expected rejection for out-of-zone target")
	}
	if !strings.Contains(err.Error(), "not in hybrid-cloud zone") {
		t.Fatalf("unexpected reject reason: %v", err)
	}
	if len(rec.prefixes()) != 0 {
		t.Fatalf("handler should not have been called; got %v", rec.prefixes())
	}
}

func TestDirectTCPIP_RejectedNonPort22(t *testing.T) {
	t.Parallel()

	handler := &recordingHandler{}
	addr, _ := startServer(t, handler)

	client := dialProxy(t, addr)
	defer func() { _ = client.Close() }()

	_, err := client.Dial("tcp", "abc12345.hybrid-cloud.com:80")
	if err == nil {
		t.Fatal("expected rejection for non-22 port")
	}
	if !strings.Contains(err.Error(), "port 22") {
		t.Fatalf("unexpected reject reason: %v", err)
	}
}

func TestSessionChannel_Rejected(t *testing.T) {
	t.Parallel()

	handler := &recordingHandler{}
	addr, _ := startServer(t, handler)

	client := dialProxy(t, addr)
	defer func() { _ = client.Close() }()

	// Session channels would give the user a shell on the proxy — refuse.
	_, _, err := client.OpenChannel("session", nil)
	if err == nil {
		t.Fatal("expected rejection for session channel")
	}
	// Channel errors from x/crypto/ssh typically wrap the reject reason.
	if !strings.Contains(err.Error(), "direct-tcpip") {
		t.Fatalf("unexpected reject reason: %v", err)
	}
}

func TestHandshakeTimeout_DropsIdleClient(t *testing.T) {
	t.Parallel()

	addr, _ := startServer(t, &recordingHandler{})

	// Open a TCP connection but don't speak SSH — server should drop us
	// after HandshakeTimeout.
	raw, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()

	_ = raw.SetReadDeadline(time.Now().Add(10 * time.Second))
	buf := make([]byte, 1)
	// The server sends a version line first; read succeeds briefly.
	// Then our silence triggers the deadline on server side, EOF lands.
	var total int
	for total < len(buf) {
		n, err := raw.Read(buf[total:])
		total += n
		if err != nil {
			if errors.Is(err, io.EOF) || strings.Contains(err.Error(), "closed") || strings.Contains(err.Error(), "reset") {
				return
			}
			return
		}
	}
}
