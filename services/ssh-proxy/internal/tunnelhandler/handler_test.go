package tunnelhandler_test

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"

	"golang.org/x/crypto/ssh"

	"hybridcloud/services/ssh-proxy/internal/ticketclient"
	"hybridcloud/services/ssh-proxy/internal/tunnelhandler"
)

type fakeIssuer struct {
	lastPrefix string
	signed     ticketclient.Signed
	err        error
}

func (f *fakeIssuer) Issue(_ context.Context, prefix string) (ticketclient.Signed, error) {
	f.lastPrefix = prefix
	return f.signed, f.err
}

// nopChannel satisfies ssh.Channel for Connect's Close() call without
// requiring a real SSH session.
type nopChannel struct {
	net.Conn
	closed atomic.Bool
}

func newNopChannel() *nopChannel {
	a, _ := net.Pipe()
	return &nopChannel{Conn: a}
}

func (c *nopChannel) Close() error {
	c.closed.Store(true)
	return c.Conn.Close()
}

func (c *nopChannel) isClosed() bool { return c.closed.Load() }

func (c *nopChannel) CloseWrite() error                              { return nil }
func (c *nopChannel) Stderr() io.ReadWriter                          { return nil }
func (c *nopChannel) SendRequest(string, bool, []byte) (bool, error) { return false, nil }

// compile-time check.
var _ ssh.Channel = (*nopChannel)(nil)

// --- tests -----------------------------------------------------------------

func TestConnect_IssuesTicketAndCloses(t *testing.T) {
	t.Parallel()

	issuer := &fakeIssuer{
		signed: ticketclient.Signed{Payload: "abc", Signature: "def"},
	}
	h := &tunnelhandler.Handler{Tickets: issuer}
	ch := newNopChannel()

	if err := h.Connect(context.Background(), "abc12345", ch); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if issuer.lastPrefix != "abc12345" {
		t.Fatalf("prefix forwarded: %q", issuer.lastPrefix)
	}
	if !ch.isClosed() {
		t.Fatal("channel must be closed after ticket (Task 6.2 behavior)")
	}
}

func TestConnect_PropagatesIssuerError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("ticket server down")
	h := &tunnelhandler.Handler{Tickets: &fakeIssuer{err: wantErr}}
	ch := newNopChannel()

	err := h.Connect(context.Background(), "abc12345", ch)
	if err == nil || !strings.Contains(err.Error(), "ticket") {
		t.Fatalf("expected ticket error, got %v", err)
	}
	if !ch.isClosed() {
		t.Fatal("channel must be closed on error")
	}
}

func TestConnect_RunsAfterTicketHook(t *testing.T) {
	t.Parallel()

	called := false
	h := &tunnelhandler.Handler{
		Tickets: &fakeIssuer{signed: ticketclient.Signed{Payload: "p", Signature: "s"}},
		AfterTicket: func(_ context.Context, prefix string, s ticketclient.Signed, _ ssh.Channel) error {
			called = true
			if prefix != "abc12345" || s.Payload != "p" {
				t.Errorf("bad args: %s %+v", prefix, s)
			}
			return nil
		},
	}
	if err := h.Connect(context.Background(), "abc12345", newNopChannel()); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("AfterTicket was not invoked")
	}
}
