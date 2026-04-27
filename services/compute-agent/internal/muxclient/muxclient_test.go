package muxclient_test

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/yamux"

	"hybridcloud/services/compute-agent/internal/muxclient"
	"hybridcloud/shared/muxconfig"
)

// fakeMuxServer is a minimal stand-in for ssh-proxy's muxserver. It accepts
// TLS, reads one JSON auth header, and either runs yamux.Server (accept)
// or closes the connection (reject). The behavior is configurable per
// test so we can exercise auth failure, reject loops, and successful
// attach without depending on the real muxserver package.
type fakeMuxServer struct {
	t        *testing.T
	listener net.Listener
	addr     string
	rootCAs  *x509.CertPool

	// behavior switches
	mu          sync.Mutex
	rejectAll   bool
	acceptCalls atomic.Int64
	rejectCalls atomic.Int64
}

func startFakeServer(t *testing.T) *fakeMuxServer {
	t.Helper()
	cert, pool := newSelfSignedCert(t)
	tlsCfg := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13}

	lis, err := tls.Listen("tcp", "127.0.0.1:0", tlsCfg)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &fakeMuxServer{t: t, listener: lis, addr: lis.Addr().String(), rootCAs: pool}

	go srv.acceptLoop()
	t.Cleanup(func() { _ = lis.Close() })
	return srv
}

func (s *fakeMuxServer) setReject(v bool) {
	s.mu.Lock()
	s.rejectAll = v
	s.mu.Unlock()
}

func (s *fakeMuxServer) acceptLoop() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

func (s *fakeMuxServer) handle(conn net.Conn) {
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	br := bufio.NewReader(conn)
	line, err := br.ReadBytes('\n')
	if err != nil {
		s.rejectCalls.Add(1)
		_ = conn.Close()
		return
	}
	var req map[string]string
	if err := json.Unmarshal(line, &req); err != nil {
		s.rejectCalls.Add(1)
		_ = conn.Close()
		return
	}

	s.mu.Lock()
	reject := s.rejectAll
	s.mu.Unlock()
	if reject {
		s.rejectCalls.Add(1)
		_ = conn.Close()
		return
	}

	_ = conn.SetDeadline(time.Time{})
	cfg := muxconfig.Config()
	sess, err := yamux.Server(conn, cfg)
	if err != nil {
		s.rejectCalls.Add(1)
		_ = conn.Close()
		return
	}
	s.acceptCalls.Add(1)
	// Hold the session until either the session itself closes (client
	// disconnect / reject test flips) or AcceptStream errors. Closing
	// the underlying conn is yamux's job once Close is called.
	for {
		stream, err := sess.AcceptStream()
		if err != nil {
			_ = sess.Close()
			return
		}
		_ = stream.Close()
	}
}

func newSelfSignedCert(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("keypair: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(certPEM)
	return cert, pool
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestMuxclient_AttachesAndCallsOnAttach(t *testing.T) {
	t.Parallel()

	srv := startFakeServer(t)

	attachCount := atomic.Int64{}
	attached := make(chan struct{}, 1)
	cfg := muxclient.Config{
		Endpoint:           srv.addr,
		ServerName:         "localhost",
		NodeID:             "node-A",
		AgentToken:         "tok",
		AgentVersion:       "0.2.5",
		InsecureSkipVerify: true,
		InitialBackoff:     50 * time.Millisecond,
		MaxBackoff:         200 * time.Millisecond,
		Log:                quietLogger(),
		OnAttach: func(*yamux.Session) {
			attachCount.Add(1)
			select {
			case attached <- struct{}{}:
			default:
			}
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- muxclient.Run(ctx, cfg) }()

	select {
	case <-attached:
	case <-time.After(3 * time.Second):
		t.Fatal("OnAttach was not called within 3s")
	}

	// yamux.Client / yamux.Server return without exchanging bytes; the
	// server-side acceptCalls counter is bumped on its own goroutine
	// timeline. Poll briefly so the assertion is not racy.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if srv.acceptCalls.Load() == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := srv.acceptCalls.Load(); got != 1 {
		t.Fatalf("server accept calls: got %d, want 1", got)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("Run returned %v, want context.Canceled or nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit within 2s of ctx cancel")
	}
}

func TestMuxclient_RetriesOnConnectFailure(t *testing.T) {
	t.Parallel()

	// Start a listener then immediately stop it so dials get refused; we
	// need an address that is *not* listening to drive the retry path.
	tmpLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := tmpLis.Addr().String()
	_ = tmpLis.Close()

	cfg := muxclient.Config{
		Endpoint:           addr,
		ServerName:         "localhost",
		NodeID:             "node-A",
		AgentToken:         "tok",
		AgentVersion:       "0.2.0",
		InsecureSkipVerify: true,
		InitialBackoff:     20 * time.Millisecond,
		MaxBackoff:         100 * time.Millisecond,
		Log:                quietLogger(),
		OnAttach:           func(*yamux.Session) {},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	err = muxclient.Run(ctx, cfg)
	if err != nil && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Fatalf("Run returned %v; want DeadlineExceeded after retries", err)
	}
}

func TestMuxclient_AuthRejectionThenRecovery(t *testing.T) {
	t.Parallel()

	srv := startFakeServer(t)
	srv.setReject(true)

	attached := make(chan struct{}, 1)
	cfg := muxclient.Config{
		Endpoint:           srv.addr,
		ServerName:         "localhost",
		NodeID:             "node-A",
		AgentToken:         "tok",
		AgentVersion:       "0.2.0",
		InsecureSkipVerify: true,
		InitialBackoff:     20 * time.Millisecond,
		MaxBackoff:         100 * time.Millisecond,
		Log:                quietLogger(),
		OnAttach: func(*yamux.Session) {
			select {
			case attached <- struct{}{}:
			default:
			}
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- muxclient.Run(ctx, cfg) }()

	// Wait for at least 2 reject attempts (proves retry).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if srv.rejectCalls.Load() >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if srv.rejectCalls.Load() < 2 {
		t.Fatalf("expected >= 2 reject calls, got %d", srv.rejectCalls.Load())
	}

	// Flip server to accept, expect attach within a few backoff cycles.
	srv.setReject(false)
	select {
	case <-attached:
	case <-time.After(2 * time.Second):
		t.Fatal("did not attach after server flipped to accept")
	}

	cancel()
	<-done
}

func TestMuxclient_CtxCancelDuringDialExits(t *testing.T) {
	t.Parallel()

	tmpLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := tmpLis.Addr().String()
	_ = tmpLis.Close()

	cfg := muxclient.Config{
		Endpoint:           addr,
		ServerName:         "localhost",
		NodeID:             "node-A",
		AgentToken:         "tok",
		AgentVersion:       "0.2.0",
		InsecureSkipVerify: true,
		InitialBackoff:     2 * time.Second,
		MaxBackoff:         10 * time.Second,
		Log:                quietLogger(),
		OnAttach:           func(*yamux.Session) {},
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- muxclient.Run(ctx, cfg) }()

	// Cancel before the first backoff completes — Run should observe
	// ctx.Done in time.After / dial and return promptly.
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("Run did not exit within 1s of cancel during dial backoff")
	}
}
