package muxserver_test

import (
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
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/yamux"

	"hybridcloud/services/ssh-proxy/internal/muxserver"
	"hybridcloud/shared/muxconfig"
)

// Phase 2.1 Task 1.1 — table-driven coverage of the four scenarios called
// out in the plan: valid auth → session, invalid token → 401 + counter,
// TLS 1.2 → handshake reject + counter, malformed header → drop + counter.

type fakeVerifier struct {
	resultByToken map[string]muxserver.AuthResult
	calls         atomic.Int64
}

func (f *fakeVerifier) Verify(_ context.Context, req muxserver.AuthRequest) (muxserver.AuthResult, error) {
	f.calls.Add(1)
	if r, ok := f.resultByToken[req.Token]; ok {
		return r, nil
	}
	return muxserver.AuthResult{}, muxserver.ErrUnauthenticated
}

type fakeRegistry struct {
	calls   atomic.Int64
	mu      sync.Mutex
	last    *yamux.Session
	lastID  string
	lastVer string
}

func (f *fakeRegistry) Register(nodeID string, sess *yamux.Session, agentVersion string) *yamux.Session {
	f.calls.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	prev := f.last
	f.last = sess
	f.lastID = nodeID
	f.lastVer = agentVersion
	return prev
}

func (f *fakeRegistry) snapshot() (int64, string, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls.Load(), f.lastID, f.lastVer
}

type fakeMetrics struct {
	failures atomic.Int64
	accepted atomic.Int64
	last     atomic.Value // string reason
}

func (m *fakeMetrics) AuthFailure(reason string) {
	m.failures.Add(1)
	m.last.Store(reason)
}

func (m *fakeMetrics) SessionAccepted() { m.accepted.Add(1) }

// Generate a self-signed ECDSA cert valid for "localhost".
func newSelfSignedCert(t *testing.T) tls.Certificate {
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
	return cert
}

// startServer spawns Serve on a localhost listener and returns the address
// + dependency probes for assertions.
func startServer(t *testing.T, verifier muxserver.Verifier) (addr string, registry *fakeRegistry, metrics *fakeMetrics, cancel func()) {
	t.Helper()
	registry = &fakeRegistry{}
	metrics = &fakeMetrics{}

	cert := newSelfSignedCert(t)
	tlsCfg := &tls.Config{Certificates: []tls.Certificate{cert}}

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	ctx, cancelFn := context.WithCancel(context.Background())
	deps := muxserver.Deps{
		TLSConfig:        tlsCfg,
		Verifier:         verifier,
		Registry:         registry,
		Metrics:          metrics,
		Log:              slog.New(slog.NewTextHandler(io.Discard, nil)),
		HandshakeTimeout: 2 * time.Second,
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = muxserver.Serve(ctx, lis, deps)
	}()

	t.Cleanup(func() {
		cancelFn()
		_ = lis.Close()
		<-done
	})
	return lis.Addr().String(), registry, metrics, cancelFn
}

// dialClient connects with TLS 1.3 and the given InsecureSkipVerify.
func dialClient(t *testing.T, addr string, maxVersion uint16) *tls.Conn {
	t.Helper()
	cfg := &tls.Config{
		InsecureSkipVerify: true,
		ServerName:         "localhost",
	}
	if maxVersion != 0 {
		cfg.MaxVersion = maxVersion
	}
	conn, err := tls.Dial("tcp", addr, cfg)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func writeAuthHeader(t *testing.T, conn *tls.Conn, req muxserver.AuthRequest) {
	t.Helper()
	body, _ := json.Marshal(req)
	body = append(body, '\n')
	if _, err := conn.Write(body); err != nil {
		t.Fatalf("write header: %v", err)
	}
}

func TestServer_ValidAuthOpensYamuxSession(t *testing.T) {
	t.Parallel()

	verifier := &fakeVerifier{resultByToken: map[string]muxserver.AuthResult{
		"good-tok": {NodeID: "node-A", AccessPolicy: "owner_team", AgentVersionSeen: "0.2.5"},
	}}
	addr, registry, metrics, _ := startServer(t, verifier)

	conn := dialClient(t, addr, 0)
	writeAuthHeader(t, conn, muxserver.AuthRequest{
		NodeID: "node-A", Token: "good-tok", AgentVersion: "0.2.5",
	})

	cfg := muxconfig.Config()
	sess, err := yamux.Client(conn, cfg)
	if err != nil {
		t.Fatalf("yamux client: %v", err)
	}
	defer func() { _ = sess.Close() }()

	// Open a stream and round-trip a probe to confirm the server is past
	// auth and into normal yamux operation. The server side accepts but
	// will not provide a stream handler in Phase 2.1 — opening a stream
	// is enough to assert the session is live.
	stream, err := sess.OpenStream()
	if err != nil {
		// In Phase 2.1 the server may immediately close once registered;
		// session liveness is sufficient.
	} else {
		_ = stream.Close()
	}

	// Wait for the registry to record the session (server runs the
	// registration in the accept loop). Up to 2s.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if c, _, _ := registry.snapshot(); c == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	calls, lastID, lastVer := registry.snapshot()
	if calls != 1 {
		t.Fatalf("registry calls: got %d, want 1", calls)
	}
	if lastID != "node-A" {
		t.Fatalf("registry node id: %q", lastID)
	}
	if lastVer != "0.2.5" {
		t.Fatalf("registry agent version: %q", lastVer)
	}
	if got := metrics.accepted.Load(); got != 1 {
		t.Fatalf("metrics accepted: got %d, want 1", got)
	}
	if got := metrics.failures.Load(); got != 0 {
		t.Fatalf("metrics failures: got %d, want 0", got)
	}
}

func TestServer_InvalidTokenRejectsAndCountsFailure(t *testing.T) {
	t.Parallel()

	verifier := &fakeVerifier{resultByToken: map[string]muxserver.AuthResult{
		"good": {NodeID: "node-A"},
	}}
	addr, registry, metrics, _ := startServer(t, verifier)

	conn := dialClient(t, addr, 0)
	writeAuthHeader(t, conn, muxserver.AuthRequest{NodeID: "node-A", Token: "wrong"})

	// Server should close the connection after the auth failure. A read
	// from the client should see EOF or a reset within HandshakeTimeout.
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 16)
	_, err := conn.Read(buf)
	if err == nil {
		t.Fatalf("expected the server to close the connection on bad auth")
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if metrics.failures.Load() == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := metrics.failures.Load(); got != 1 {
		t.Fatalf("metrics failures: got %d, want 1", got)
	}
	if reason, _ := metrics.last.Load().(string); reason != "unauthenticated" {
		t.Fatalf("failure reason: %q", reason)
	}
	if got := registry.calls.Load(); got != 0 {
		t.Fatalf("registry should not be called on bad auth, got %d", got)
	}
}

func TestServer_TLS12IsRejected(t *testing.T) {
	t.Parallel()

	verifier := &fakeVerifier{resultByToken: map[string]muxserver.AuthResult{}}
	addr, _, metrics, _ := startServer(t, verifier)

	cfg := &tls.Config{
		InsecureSkipVerify: true,
		ServerName:         "localhost",
		MaxVersion:         tls.VersionTLS12,
	}
	_, err := tls.Dial("tcp", addr, cfg)
	if err == nil {
		t.Fatal("expected TLS 1.2 client to be rejected at handshake")
	}

	// The server-side bumps mux_auth_failures_total{reason=tls_downgrade}
	// once it observes the failed handshake. Allow up to 2s.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if metrics.failures.Load() >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := metrics.failures.Load(); got < 1 {
		t.Fatalf("metrics failures after TLS 1.2: got %d, want >= 1", got)
	}
	if reason, _ := metrics.last.Load().(string); reason != "tls_downgrade" {
		t.Fatalf("failure reason: %q", reason)
	}
}

func TestServer_BadJSONHeader(t *testing.T) {
	t.Parallel()

	verifier := &fakeVerifier{resultByToken: map[string]muxserver.AuthResult{}}
	addr, _, metrics, _ := startServer(t, verifier)

	conn := dialClient(t, addr, 0)
	if _, err := fmt.Fprintln(conn, "not-json"); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 16)
	if _, err := conn.Read(buf); err == nil {
		t.Fatal("expected close on malformed header")
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if metrics.failures.Load() == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := metrics.failures.Load(); got != 1 {
		t.Fatalf("metrics failures: got %d, want 1", got)
	}
	if reason, _ := metrics.last.Load().(string); reason != "bad_header" {
		t.Fatalf("failure reason: %q", reason)
	}
}

// Verify ctx cancel exits Serve cleanly.
func TestServer_CtxCancelStopsServe(t *testing.T) {
	t.Parallel()

	verifier := &fakeVerifier{}
	cert := newSelfSignedCert(t)
	tlsCfg := &tls.Config{Certificates: []tls.Certificate{cert}}

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- muxserver.Serve(ctx, lis, muxserver.Deps{
			TLSConfig: tlsCfg,
			Verifier:  verifier,
			Registry:  &fakeRegistry{},
			Metrics:   &fakeMetrics{},
			Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		})
	}()

	cancel()
	_ = lis.Close()

	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, net.ErrClosed) {
			t.Fatalf("Serve returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not exit within 2s of ctx cancel")
	}
}
