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
// enough to learn the tunnel endpoint we should dial. HMAC verification is
// the agent's job.
type ticketPayload struct {
	TunnelEndpoint string `json:"tunnel_endpoint"`
	SessionID      string `json:"session_id"`
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

	conn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", p.TunnelEndpoint)
	if err != nil {
		_ = ch.Close()
		return fmt.Errorf("dial agent tunnel %s: %w", p.TunnelEndpoint, err)
	}

	header, _ := json.Marshal(signed)
	if _, err := conn.Write(append(header, '\n')); err != nil {
		_ = conn.Close()
		_ = ch.Close()
		return fmt.Errorf("write header: %w", err)
	}

	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(conn, ch)
		_ = conn.Close()
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(ch, conn)
		_ = ch.Close()
		done <- struct{}{}
	}()

	select {
	case <-done:
	case <-ctx.Done():
	}
	// Wait for the second half to finish so we don't leak.
	<-done
	return nil
}
