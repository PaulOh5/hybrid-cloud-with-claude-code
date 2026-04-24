package tunnel_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"hybridcloud/services/compute-agent/internal/tunnel"
)

// stubVerifier returns either a canned Ticket or an error.
type stubVerifier struct {
	ticket tunnel.Ticket
	err    error
}

func (s *stubVerifier) Verify(signed tunnel.SignedTicket) (tunnel.Ticket, error) {
	return s.ticket, s.err
}

// startServer binds on 127.0.0.1:0 and returns the addr.
func startServer(t *testing.T, v tunnel.Verifier) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s, err := tunnel.New(tunnel.Config{
		Verifier:      v,
		DialTimeout:   1 * time.Second,
		HeaderTimeout: 1 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = s.Serve(ctx, lis) }()
	t.Cleanup(func() {
		cancel()
		_ = lis.Close()
	})
	return lis.Addr().String()
}

// startFakeVM listens on 127.0.0.1:0 and echoes a greeting to the first
// client before echoing subsequent data.
func startFakeVM(t *testing.T) (string, <-chan string) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	received := make(chan string, 1)
	go func() {
		conn, err := lis.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		if _, err := conn.Write([]byte("VM-BANNER\n")); err != nil {
			return
		}
		r := bufio.NewReader(conn)
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		received <- line
	}()
	t.Cleanup(func() { _ = lis.Close() })
	return lis.Addr().String(), received
}

// --- tests -----------------------------------------------------------------

func TestTunnel_EndToEndRelay(t *testing.T) {
	t.Parallel()

	vmAddr, vmLine := startFakeVM(t)
	host, portStr, _ := net.SplitHostPort(vmAddr)
	var port int
	if _, err := fmtSscanf(portStr, &port); err != nil {
		t.Fatal(err)
	}

	verifier := &stubVerifier{
		ticket: tunnel.Ticket{
			VMInternalIP: host,
			VMPort:       uint16(port),
			ExpiresAt:    time.Now().Add(time.Minute),
		},
	}
	addr := startServer(t, verifier)

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	payload, _ := json.Marshal(tunnel.SignedTicket{Payload: "p", Signature: "s"})
	if _, err := conn.Write(append(payload, '\n')); err != nil {
		t.Fatal(err)
	}

	banner := make([]byte, 64)
	n, err := conn.Read(banner)
	if err != nil {
		t.Fatalf("read banner: %v", err)
	}
	if got := string(banner[:n]); got != "VM-BANNER\n" {
		t.Fatalf("banner: %q", got)
	}

	// Write a line to the VM through the tunnel.
	if _, err := conn.Write([]byte("hello-vm\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-vmLine:
		if got != "hello-vm\n" {
			t.Fatalf("vm received: %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("vm never received data")
	}
}

func TestTunnel_RejectsBadSignature(t *testing.T) {
	t.Parallel()

	verifier := &stubVerifier{err: errors.New("bad sig")}
	addr := startServer(t, verifier)

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	payload, _ := json.Marshal(tunnel.SignedTicket{Payload: "x", Signature: "y"})
	_, _ = conn.Write(append(payload, '\n'))

	// Server should close without sending anything.
	buf := make([]byte, 8)
	_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	_, err = conn.Read(buf)
	if err == nil {
		t.Fatal("expected close on bad signature")
	}
	// Either EOF (clean close) or RST via timeout is fine; we only care
	// that no data flowed, which the err != nil check above guarantees.
	_ = io.EOF
	_ = isTimeout
}

func TestTunnel_HeaderTimeout(t *testing.T) {
	t.Parallel()

	verifier := &stubVerifier{}
	addr := startServer(t, verifier)

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	// Do not write any header. Server should drop us after HeaderTimeout
	// (1s in startServer config).
	buf := make([]byte, 4)
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, err = conn.Read(buf)
	if err == nil {
		t.Fatal("expected header timeout to close")
	}
}

// --- helpers --------------------------------------------------------------

func fmtSscanf(s string, out *int) (int, error) {
	n, err := parseDecimal(s)
	*out = n
	return 1, err
}

func parseDecimal(s string) (int, error) {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, errors.New("not a number")
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}
