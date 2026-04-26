// Package tunnelhandler implements server.Handler by fetching a ticket from
// main-api and (Task 6.3) dialling the agent's tunnel listener to relay
// bytes. Task 6.2 stops after the ticket — the channel is closed without a
// byte tunnel.
package tunnelhandler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"golang.org/x/crypto/ssh"

	"hybridcloud/services/ssh-proxy/internal/server"
	"hybridcloud/services/ssh-proxy/internal/ticketclient"
)

// TicketIssuer is the ticketclient.Client subset the handler needs. An
// interface so tests can stub without a running HTTP server.
type TicketIssuer interface {
	Issue(ctx context.Context, prefix, fingerprint string) (ticketclient.Signed, error)
}

// Handler wires the server.Handler signature; callers set AfterTicket to
// complete the tunnel in Task 6.3. When AfterTicket is nil, the channel is
// closed right after a successful ticket lookup (Task 6.2 behaviour).
type Handler struct {
	Tickets     TicketIssuer
	Log         *slog.Logger
	AfterTicket func(ctx context.Context, prefix string, ticket ticketclient.Signed, ch ssh.Channel) error
}

// Connect fetches a ticket for the subdomain prefix scoped to the
// authenticated client fingerprint. See package doc for the Task-6.2 vs 6.3
// behaviour switch.
func (h *Handler) Connect(ctx context.Context, info server.ConnInfo, prefix string, ch ssh.Channel) error {
	log := h.log()
	if h.Tickets == nil {
		_ = ch.Close()
		return errors.New("tunnelhandler: no ticket issuer configured")
	}
	if info.Fingerprint == "" {
		// Defence in depth: PublicKeyCallback should always populate this,
		// but if a future change ever loosens auth we want the failure
		// loud and local rather than letting main-api silently 404.
		_ = ch.Close()
		return errors.New("tunnelhandler: missing client fingerprint")
	}

	signed, err := h.Tickets.Issue(ctx, prefix, info.Fingerprint)
	if err != nil {
		_ = ch.Close()
		log.Warn("ticket issue", "prefix", prefix, "fingerprint", info.Fingerprint, "err", err)
		return fmt.Errorf("ticket: %w", err)
	}
	log.Info("ticket issued",
		"prefix", prefix,
		"fingerprint", info.Fingerprint,
		"payload_len", len(signed.Payload),
	)

	if h.AfterTicket == nil {
		_ = ch.Close()
		return nil
	}
	return h.AfterTicket(ctx, prefix, signed, ch)
}

func (h *Handler) log() *slog.Logger {
	if h.Log != nil {
		return h.Log
	}
	return slog.Default()
}
