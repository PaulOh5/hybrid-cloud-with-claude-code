// Package server implements the ssh-proxy SSH listener. It accepts anonymous
// SSH kex (auth happens VM-side) and only handles direct-tcpip channel
// requests — the ProxyJump path a client takes when ~/.ssh/config has
//
//	Host *.hybrid-cloud.com
//	    ProxyJump proxy.hybrid-cloud.com
//
// Task 6.1 scope: accept the connection, extract the target subdomain, and
// reject with a structured error. Task 6.2/6.3 wire the ticket lookup +
// agent tunnel on top of Handler.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"hybridcloud/services/ssh-proxy/internal/router"
)

// Handler decides what to do with a direct-tcpip request after the router
// validates the target is in-zone. Task 6.1 ships a static Deny handler;
// Task 6.2/6.3 wire the real tunnel.
type Handler interface {
	// Connect is called once per accepted direct-tcpip channel. It receives
	// the resolved subdomain prefix and is responsible for opening whatever
	// transport to the VM and shuttling bytes. Return nil on clean close,
	// an error to surface to the client.
	Connect(ctx context.Context, prefix string, ch ssh.Channel) error
}

// DenyHandler rejects every request with the supplied reason. Default for
// Task 6.1 — unit tests exercise this path.
type DenyHandler struct{ Reason string }

// Connect satisfies Handler.
func (d DenyHandler) Connect(_ context.Context, _ string, ch ssh.Channel) error {
	_ = ch.Close()
	if d.Reason == "" {
		return errors.New("proxy: tunnel not implemented")
	}
	return errors.New("proxy: " + d.Reason)
}

// Config is the server boot configuration.
type Config struct {
	// Zone is the DNS suffix we accept direct-tcpip requests for, e.g.
	// "hybrid-cloud.com". Anything else is refused.
	Zone string
	// HostKeys signs the proxy's SSH identity; at least one required.
	HostKeys []ssh.Signer
	// Handler is invoked per accepted target. Zero value denies everything.
	Handler Handler
	// HandshakeTimeout bounds how long a client has to finish kex + open
	// the first channel before we drop the connection. Default 15s.
	HandshakeTimeout time.Duration
	// Log receives operational events; zero value uses slog.Default.
	Log *slog.Logger
}

// Server is the SSH listener. Run blocks until ctx is cancelled.
type Server struct {
	cfg       Config
	sshConfig *ssh.ServerConfig
}

// New validates cfg and returns a ready Server.
func New(cfg Config) (*Server, error) {
	if cfg.Zone == "" {
		return nil, errors.New("server: Zone required")
	}
	if len(cfg.HostKeys) == 0 {
		return nil, errors.New("server: at least one host key required")
	}
	if cfg.Handler == nil {
		cfg.Handler = DenyHandler{Reason: "tunnel not configured"}
	}
	if cfg.HandshakeTimeout <= 0 {
		cfg.HandshakeTimeout = 15 * time.Second
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	sc := &ssh.ServerConfig{
		NoClientAuth:  true,
		ServerVersion: "SSH-2.0-hybrid-cloud-proxy",
	}
	for _, k := range cfg.HostKeys {
		sc.AddHostKey(k)
	}
	return &Server{cfg: cfg, sshConfig: sc}, nil
}

// Serve accepts connections on lis until ctx is cancelled. It never returns
// an error from Accept; per-connection failures are logged.
func (s *Server) Serve(ctx context.Context, lis net.Listener) error {
	var wg sync.WaitGroup
	go func() {
		<-ctx.Done()
		_ = lis.Close()
	}()
	for {
		conn, err := lis.Accept()
		if err != nil {
			if ctx.Err() != nil {
				wg.Wait()
				return ctx.Err()
			}
			s.cfg.Log.Warn("accept", "err", err)
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.handleConn(ctx, conn)
		}()
	}
}

func (s *Server) handleConn(ctx context.Context, raw net.Conn) {
	defer func() { _ = raw.Close() }()

	deadline := time.Now().Add(s.cfg.HandshakeTimeout)
	_ = raw.SetDeadline(deadline)

	sshConn, chans, reqs, err := ssh.NewServerConn(raw, s.sshConfig)
	if err != nil {
		s.cfg.Log.Warn("ssh handshake", "remote", raw.RemoteAddr(), "err", err)
		return
	}
	defer func() { _ = sshConn.Close() }()
	_ = raw.SetDeadline(time.Time{})

	// Drop any global requests — we don't implement them.
	go ssh.DiscardRequests(reqs)

	s.cfg.Log.Info("ssh connection",
		"remote", sshConn.RemoteAddr(),
		"client", string(sshConn.ClientVersion()),
	)

	for newCh := range chans {
		if newCh.ChannelType() != "direct-tcpip" {
			_ = newCh.Reject(ssh.Prohibited,
				fmt.Sprintf("channel type %q not supported; use direct-tcpip (ProxyJump)", newCh.ChannelType()))
			continue
		}
		go s.handleDirectTCPIP(ctx, newCh)
	}
}

// directTCPIPReq is the RFC 4254 §7.2 payload of a direct-tcpip channel
// open request.
type directTCPIPReq struct {
	Host       string
	Port       uint32
	OriginHost string
	OriginPort uint32
}

func (s *Server) handleDirectTCPIP(ctx context.Context, newCh ssh.NewChannel) {
	var req directTCPIPReq
	if err := ssh.Unmarshal(newCh.ExtraData(), &req); err != nil {
		_ = newCh.Reject(ssh.ConnectionFailed, "malformed direct-tcpip payload")
		return
	}

	route, err := router.ExtractRoute(req.Host, s.cfg.Zone)
	if err != nil {
		s.cfg.Log.Warn("reject direct-tcpip",
			"host", req.Host, "port", req.Port, "err", err)
		_ = newCh.Reject(ssh.Prohibited, "target not in hybrid-cloud zone")
		return
	}
	if req.Port != 22 {
		_ = newCh.Reject(ssh.Prohibited, "only port 22 is forwarded")
		return
	}

	ch, requests, err := newCh.Accept()
	if err != nil {
		s.cfg.Log.Warn("accept channel", "err", err)
		return
	}
	go ssh.DiscardRequests(requests)

	s.cfg.Log.Info("direct-tcpip accepted",
		"prefix", route.Prefix,
		"origin", fmt.Sprintf("%s:%d", req.OriginHost, req.OriginPort),
	)

	if err := s.cfg.Handler.Connect(ctx, route.Prefix, ch); err != nil {
		s.cfg.Log.Warn("tunnel", "prefix", route.Prefix, "err", err)
	}
}
