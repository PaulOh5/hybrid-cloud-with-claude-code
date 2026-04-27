package tunnelhandler_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"

	"hybridcloud/services/ssh-proxy/internal/ticketclient"
	"hybridcloud/services/ssh-proxy/internal/tunnelhandler"
)

// fakeOpener is a StreamOpener stand-in that returns one side of a
// net.Pipe per call. The other side is captured per nodeID so tests can
// drive bytes "from the agent side" of the simulated mux stream.
type fakeOpener struct {
	mu       sync.Mutex
	streams  map[string]net.Conn // node_id -> our half (the one we hand to Relay)
	agents   map[string]net.Conn // node_id -> the agent-facing half
	openErr  error
	openCall atomic.Int64
}

func newFakeOpener() *fakeOpener {
	return &fakeOpener{
		streams: map[string]net.Conn{},
		agents:  map[string]net.Conn{},
	}
}

func (f *fakeOpener) OpenStream(_ context.Context, nodeID string) (net.Conn, error) {
	f.openCall.Add(1)
	if f.openErr != nil {
		return nil, f.openErr
	}
	a, b := net.Pipe()
	f.mu.Lock()
	f.streams[nodeID] = a
	f.agents[nodeID] = b
	f.mu.Unlock()
	return a, nil
}

func (f *fakeOpener) agent(nodeID string) net.Conn {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.agents[nodeID]
}

// pipeChannel wraps a net.Pipe so we can use one side as an ssh.Channel and
// drive the other from the test.
type pipeChannel struct {
	net.Conn
}

func (c *pipeChannel) CloseWrite() error                              { return nil }
func (c *pipeChannel) Stderr() io.ReadWriter                          { return nil }
func (c *pipeChannel) SendRequest(string, bool, []byte) (bool, error) { return false, nil }

var _ ssh.Channel = (*pipeChannel)(nil)

func makeSigned(t *testing.T, p any) ticketclient.Signed {
	t.Helper()
	body, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return ticketclient.Signed{
		Payload:   base64.StdEncoding.EncodeToString(body),
		Signature: "sig",
	}
}

func TestRelay_OpensMuxStreamByNodeID(t *testing.T) {
	t.Parallel()

	nodeID := uuid.New().String()
	opener := newFakeOpener()
	relayer := &tunnelhandler.Relayer{Opener: opener, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	signed := makeSigned(t, map[string]any{
		"session_id":     "sess-1",
		"node_id":        nodeID,
		"vm_internal_ip": "192.168.122.47",
		"vm_port":        22,
		"expires_at":     time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano),
	})

	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	ch := &pipeChannel{Conn: server}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	go func() { _ = relayer.Relay(ctx, "sess-1", signed, ch) }()

	// Wait for the opener to receive the call.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if opener.openCall.Load() == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if opener.openCall.Load() != 1 {
		t.Fatalf("opener calls: got %d, want 1", opener.openCall.Load())
	}

	// Read the agent-side header that ssh-proxy writes on the stream.
	agentSide := opener.agent(nodeID)
	if agentSide == nil {
		t.Fatalf("opener did not register stream for node %s", nodeID)
	}
	_ = agentSide.SetReadDeadline(time.Now().Add(2 * time.Second))
	br := bufio.NewReader(agentSide)
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read header: %v", err)
	}

	var hdr struct {
		InstanceID   string `json:"instance_id"`
		VMInternalIP string `json:"vm_internal_ip"`
		VMPort       uint16 `json:"vm_port"`
	}
	if err := json.Unmarshal([]byte(line), &hdr); err != nil {
		t.Fatalf("parse header: %v body=%q", err, line)
	}
	if hdr.VMInternalIP != "192.168.122.47" || hdr.VMPort != 22 {
		t.Fatalf("header fields: %+v", hdr)
	}

	// Bidirectional copy: client writes "hello\n", agent echoes back through
	// the simulated stream.
	go func() {
		_, _ = io.Copy(agentSide, agentSide)
	}()
	if _, err := client.Write([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 6)
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(client, buf); err != nil {
		t.Fatalf("echo: %v", err)
	}
	if !bytes.Equal(buf, []byte("hello\n")) {
		t.Fatalf("echo mismatch: %q", buf)
	}
}

func TestRelay_UnknownNodeReturnsError(t *testing.T) {
	t.Parallel()

	opener := newFakeOpener()
	opener.openErr = errors.New("muxregistry: no session for node")
	relayer := &tunnelhandler.Relayer{Opener: opener, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	signed := makeSigned(t, map[string]any{
		"node_id":        uuid.New().String(),
		"vm_internal_ip": "192.168.122.47",
		"vm_port":        22,
		"expires_at":     time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano),
	})

	_, server := net.Pipe()
	ch := &pipeChannel{Conn: server}

	err := relayer.Relay(context.Background(), "x", signed, ch)
	if err == nil {
		t.Fatal("expected error for unknown node")
	}
}

func TestRelay_RejectsMissingNodeID(t *testing.T) {
	t.Parallel()

	opener := newFakeOpener()
	relayer := &tunnelhandler.Relayer{Opener: opener, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	signed := makeSigned(t, map[string]any{
		"vm_internal_ip": "192.168.122.47",
		"vm_port":        22,
	})
	_, server := net.Pipe()
	ch := &pipeChannel{Conn: server}

	err := relayer.Relay(context.Background(), "x", signed, ch)
	if err == nil {
		t.Fatal("expected error for missing node_id")
	}
	if opener.openCall.Load() != 0 {
		t.Fatalf("OpenStream should not be called for invalid payload, got %d", opener.openCall.Load())
	}
}

func TestRelay_RejectsExpiredTicket(t *testing.T) {
	t.Parallel()

	opener := newFakeOpener()
	relayer := &tunnelhandler.Relayer{Opener: opener, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	signed := makeSigned(t, map[string]any{
		"node_id":        uuid.New().String(),
		"vm_internal_ip": "192.168.122.47",
		"vm_port":        22,
		"expires_at":     time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano),
	})
	_, server := net.Pipe()
	ch := &pipeChannel{Conn: server}

	err := relayer.Relay(context.Background(), "x", signed, ch)
	if err == nil {
		t.Fatal("expected error for expired ticket")
	}
	if opener.openCall.Load() != 0 {
		t.Fatalf("OpenStream should not be called for expired ticket, got %d", opener.openCall.Load())
	}
}

func TestRelay_RejectsBadPayload(t *testing.T) {
	t.Parallel()

	opener := newFakeOpener()
	relayer := &tunnelhandler.Relayer{Opener: opener, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	signed := ticketclient.Signed{
		Payload:   "%%notbase64%%",
		Signature: "s",
	}
	_, server := net.Pipe()
	ch := &pipeChannel{Conn: server}

	err := relayer.Relay(context.Background(), "x", signed, ch)
	if err == nil {
		t.Fatal("expected decode error")
	}
}
