package tunnelhandler_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"hybridcloud/services/ssh-proxy/internal/ticketclient"
	"hybridcloud/services/ssh-proxy/internal/tunnelhandler"
)

// fakeAgentTunnel accepts one connection, reads the JSON header, and then
// echoes every byte back on the same socket — a simple stand-in for the
// real agent's tunnel server.
func fakeAgentTunnel(t *testing.T) (string, chan string) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	headerCh := make(chan string, 1)
	go func() {
		defer func() { _ = lis.Close() }()
		conn, err := lis.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		r := bufio.NewReader(conn)
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		headerCh <- line
		_, _ = io.Copy(conn, r)
	}()
	t.Cleanup(func() { _ = lis.Close() })
	return lis.Addr().String(), headerCh
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

func TestRelay_Roundtrip(t *testing.T) {
	t.Parallel()

	agentAddr, headerCh := fakeAgentTunnel(t)

	// Payload: agent learns the tunnel endpoint for verification; proxy's
	// Relay reads it to know where to dial.
	payload, _ := json.Marshal(map[string]any{
		"session_id":      "sess",
		"tunnel_endpoint": agentAddr,
	})
	signed := ticketclient.Signed{
		Payload:   base64.StdEncoding.EncodeToString(payload),
		Signature: "sig",
	}

	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	ch := &pipeChannel{Conn: server}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	go func() {
		_ = tunnelhandler.Relay(ctx, "sess", signed, ch)
	}()

	// Expect the header on the fake agent side.
	select {
	case line := <-headerCh:
		var got ticketclient.Signed
		if err := json.Unmarshal([]byte(line), &got); err != nil {
			t.Fatalf("header parse: %v", err)
		}
		if got.Signature != "sig" {
			t.Fatalf("signature: %q", got.Signature)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for header")
	}

	// Now drive bytes from proxy-client side into Relay, which should
	// forward to agent → agent echoes → Relay copies back to us.
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

func TestRelay_RejectsEmptyEndpoint(t *testing.T) {
	t.Parallel()

	payload, _ := json.Marshal(map[string]any{"session_id": "x", "tunnel_endpoint": ""})
	signed := ticketclient.Signed{
		Payload:   base64.StdEncoding.EncodeToString(payload),
		Signature: "s",
	}
	_, server := net.Pipe()
	ch := &pipeChannel{Conn: server}

	err := tunnelhandler.Relay(context.Background(), "x", signed, ch)
	if err == nil {
		t.Fatal("expected error for missing endpoint")
	}
}

func TestRelay_RejectsBadPayload(t *testing.T) {
	t.Parallel()

	signed := ticketclient.Signed{
		Payload:   "%%notbase64%%",
		Signature: "s",
	}
	_, server := net.Pipe()
	ch := &pipeChannel{Conn: server}

	err := tunnelhandler.Relay(context.Background(), "x", signed, ch)
	if err == nil {
		t.Fatal("expected decode error")
	}
}
