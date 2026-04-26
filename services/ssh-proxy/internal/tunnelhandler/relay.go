package tunnelhandler

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"time"

	"golang.org/x/crypto/ssh"

	"hybridcloud/services/ssh-proxy/internal/ticketclient"
)

// ticketPayload decodes the base64 JSON body inside a signed envelope — just
// enough to learn the tunnel endpoint we should dial and check the ticket
// hasn't aged out between issue and use. HMAC verification is the agent's
// job; we re-check expiry independently so a delayed dial fails fast at
// the proxy instead of crossing the wire and being rejected by the agent.
type ticketPayload struct {
	TunnelEndpoint string    `json:"tunnel_endpoint"`
	SessionID      string    `json:"session_id"`
	ExpiresAt      time.Time `json:"expires_at"`
}

// Relay implements the AfterTicket hook: it dials the agent's tunnel port,
// writes the signed ticket as a JSON header line, then copies bytes between
// the SSH channel and the TCP connection until either side closes.
func Relay(ctx context.Context, _ string, signed ticketclient.Signed, ch ssh.Channel) error {
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
	if p.TunnelEndpoint == "" {
		_ = ch.Close()
		return fmt.Errorf("ticket missing tunnel_endpoint")
	}
	// 1s skew tolerance so an in-flight dial doesn't trip on
	// boundary timing. Any larger gap means the ticket is genuinely
	// stale and the agent would reject it anyway.
	if !p.ExpiresAt.IsZero() && time.Now().After(p.ExpiresAt.Add(-time.Second)) {
		_ = ch.Close()
		return fmt.Errorf("ticket expired at %s", p.ExpiresAt.UTC().Format(time.RFC3339))
	}

	conn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", p.TunnelEndpoint)
	if err != nil {
		_ = ch.Close()
		return fmt.Errorf("dial agent tunnel %s: %w", p.TunnelEndpoint, err)
	}

	// Bound the header write so a hung agent endpoint cannot park us
	// indefinitely between dial and the actual byte relay.
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	header, _ := json.Marshal(signed)
	if _, err := conn.Write(append(header, '\n')); err != nil {
		_ = conn.Close()
		_ = ch.Close()
		return fmt.Errorf("write header: %w", err)
	}
	_ = conn.SetWriteDeadline(time.Time{})

	// Both halves must terminate before we return so we don't leak
	// goroutines or sockets. Closing one direction propagates EOF to the
	// io.Copy on the other side; ctx cancel forces both closures and the
	// io.Copy calls return immediately.
	done := make(chan struct{}, 2)
	closeBoth := func() {
		_ = conn.Close()
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
		_, _ = io.Copy(conn, ch)
		// Half-close: signal EOF to the agent without tearing down the
		// other direction. ssh.Channel implements CloseWrite; on plain
		// TCP we use the SetLinger/CloseWrite-equivalent CloseWrite.
		if cw, ok := conn.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		} else {
			_ = conn.Close()
		}
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(ch, conn)
		_ = ch.CloseWrite()
		done <- struct{}{}
	}()

	<-done
	// First half done — close both ends so the surviving io.Copy unblocks
	// even if its peer never sends EOF.
	closeBoth()
	<-done
	return nil
}
