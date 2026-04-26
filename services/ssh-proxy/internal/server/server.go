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
	// Connect is called once per accepted direct-tcpip channel. ConnInfo
	// carries the SSH-side identity (fingerprint, remote addr) collected
	// during handshake so the handler can pass it to main-api for owner
	// scoping. Return nil on clean close, an error to surface to the
	// client.
	Connect(ctx context.Context, info ConnInfo, prefix string, ch ssh.Channel) error
}

// ConnInfo describes the authenticated SSH client for a single direct-tcpip
// channel. Fingerprint is the SHA-256 hash of the client's offered public
// key (matches `ssh-keygen -lf` SHA256 output and main-api's
// sshkeys.Fingerprint format).
type ConnInfo struct {
	Fingerprint string
	RemoteAddr  string
}

// fingerprintExtKey is the ssh.Permissions.Extensions key the
// PublicKeyCallback writes to so handleDirectTCPIP can recover it.
const fingerprintExtKey = "fingerprint"

// DenyHandler rejects every request with the supplied reason. Default for
// Task 6.1 — unit tests exercise this path.
type DenyHandler struct{ Reason string }

// Connect satisfies Handler.
func (d DenyHandler) Connect(_ context.Context, _ ConnInfo, _ string, ch ssh.Channel) error {
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
	// MaxConcurrentConns caps the number of in-flight SSH connections.
	// Excess connections are accepted then immediately closed so the OS
	// listener queue does not back up. Default 1024.
	MaxConcurrentConns int
	// IdleTimeout drops a post-handshake connection that hasn't opened a
	// single direct-tcpip channel within this window — slowloris-after-
	// handshake protection. Default 60s. Zero disables.
	IdleTimeout time.Duration
	// Log receives operational events; zero value uses slog.Default.
	Log *slog.Logger
}

// Server is the SSH listener. Run blocks until ctx is cancelled.
type Server struct {
	cfg       Config
	sshConfig *ssh.ServerConfig
	// connSlots is a buffered channel used as a counting semaphore. Each
	// in-flight connection holds one slot; new connections beyond
	// MaxConcurrentConns get rejected immediately so a slowloris flood
	// cannot exhaust file descriptors.
	connSlots chan struct{}
	// channelWG tracks in-flight direct-tcpip handlers (one per active
	// SSH session/relay) so Serve waits for byte-relay drains before
	// returning. Without this a SIGTERM cuts active SSH sessions abruptly
	// instead of letting them flush.
	channelWG sync.WaitGroup
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
	if cfg.MaxConcurrentConns <= 0 {
		cfg.MaxConcurrentConns = 1024
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = 60 * time.Second
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	sc := &ssh.ServerConfig{
		// Owner scoping happens on the main-api side: we capture the
		// client-offered key fingerprint here, forward it via the ticket
		// request, and main-api confirms the key belongs to a user that
		// owns the target instance. ssh-proxy itself has no key DB.
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			return &ssh.Permissions{
				Extensions: map[string]string{
					fingerprintExtKey: ssh.FingerprintSHA256(key),
				},
			}, nil
		},
		// Pin a small set of strong algorithms instead of taking
		// x/crypto/ssh's evolving defaults. This makes future library
		// upgrades safer (no silent revival of weak algorithms) and
		// matches the OpenSSH-modern profile most clients already use.
		Config: ssh.Config{
			KeyExchanges: []string{
				"curve25519-sha256",
				"curve25519-sha256@libssh.org",
			},
			Ciphers: []string{
				"chacha20-poly1305@openssh.com",
				"aes256-gcm@openssh.com",
				"aes128-gcm@openssh.com",
			},
			MACs: []string{
				"hmac-sha2-256-etm@openssh.com",
				"hmac-sha2-512-etm@openssh.com",
			},
		},
		ServerVersion: "SSH-2.0-hybrid-cloud-proxy",
	}
	for _, k := range cfg.HostKeys {
		sc.AddHostKey(k)
	}
	return &Server{
		cfg:       cfg,
		sshConfig: sc,
		connSlots: make(chan struct{}, cfg.MaxConcurrentConns),
	}, nil
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
				// Wait for in-flight direct-tcpip relays too. Without
				// this an SSH client connected through ProxyJump can be
				// cut mid-session on graceful shutdown even though the
				// connection-level wg returned.
				s.channelWG.Wait()
				return ctx.Err()
			}
			s.cfg.Log.Warn("accept", "err", err)
			continue
		}

		// Acquire a connection slot non-blockingly. When the cap is hit we
		// drop the new conn at TCP level; the client will see a reset and
		// can back off without us paying handshake CPU.
		select {
		case s.connSlots <- struct{}{}:
		default:
			s.cfg.Log.Warn("connection cap reached, dropping",
				"remote", conn.RemoteAddr(), "cap", s.cfg.MaxConcurrentConns)
			_ = conn.Close()
			continue
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-s.connSlots }()
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

	info := ConnInfo{
		RemoteAddr: sshConn.RemoteAddr().String(),
	}
	if sshConn.Permissions != nil {
		info.Fingerprint = sshConn.Permissions.Extensions[fingerprintExtKey]
	}

	// IdleTimeout: a client that finishes handshake but never opens a
	// direct-tcpip channel is most likely a slowloris probe. A timer fires
	// once IdleTimeout elapses with zero channels opened; closing sshConn
	// also ends `chans` below so handleConn returns naturally.
	idleTimer := time.AfterFunc(s.cfg.IdleTimeout, func() {
		s.cfg.Log.Warn("idle ssh connection dropped",
			"remote", sshConn.RemoteAddr())
		_ = sshConn.Close()
	})
	defer idleTimer.Stop()

	for newCh := range chans {
		if newCh.ChannelType() != "direct-tcpip" {
			_ = newCh.Reject(ssh.Prohibited,
				fmt.Sprintf("channel type %q not supported; use direct-tcpip (ProxyJump)", newCh.ChannelType()))
			continue
		}
		// Once the client has actually opened a tunnel channel, stop the
		// idle timer — the relay layer owns liveness from here on.
		idleTimer.Stop()
		s.channelWG.Add(1)
		go func(nc ssh.NewChannel) {
			defer s.channelWG.Done()
			s.handleDirectTCPIP(ctx, info, nc)
		}(newCh)
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

func (s *Server) handleDirectTCPIP(ctx context.Context, info ConnInfo, newCh ssh.NewChannel) {
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
		"fingerprint", info.Fingerprint,
		"origin", fmt.Sprintf("%s:%d", req.OriginHost, req.OriginPort),
	)

	if err := s.cfg.Handler.Connect(ctx, info, route.Prefix, ch); err != nil {
		s.cfg.Log.Warn("tunnel", "prefix", route.Prefix, "err", err)
	}
}
