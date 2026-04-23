package stream_test

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"hybridcloud/services/compute-agent/internal/stream"
	"hybridcloud/services/compute-agent/internal/topology"
	agentv1 "hybridcloud/shared/proto/agent/v1"
)

// --- fake server -----------------------------------------------------------

type fakeServer struct {
	agentv1.UnimplementedAgentServiceServer

	registers  int32
	heartbeats int32
	registerCh chan *agentv1.Register
}

func newFakeServer() *fakeServer {
	return &fakeServer{registerCh: make(chan *agentv1.Register, 8)}
}

func (f *fakeServer) Stream(s agentv1.AgentService_StreamServer) error {
	first, err := s.Recv()
	if err != nil {
		return err
	}
	reg := first.GetRegister()
	if reg == nil {
		return errors.New("expected Register")
	}

	atomic.AddInt32(&f.registers, 1)
	select {
	case f.registerCh <- reg:
	default:
	}

	// Ack with a short heartbeat interval so the test isn't slow.
	if err := s.Send(&agentv1.ControlMessage{
		Payload: &agentv1.ControlMessage_RegisterAck{
			RegisterAck: &agentv1.RegisterAck{
				NodeId:                   "node-abc",
				HeartbeatIntervalSeconds: 1,
			},
		},
	}); err != nil {
		return err
	}

	for {
		msg, err := s.Recv()
		if err != nil {
			return err
		}
		if msg.GetHeartbeat() != nil {
			atomic.AddInt32(&f.heartbeats, 1)
		}
	}
}

func (f *fakeServer) registerCount() int32  { return atomic.LoadInt32(&f.registers) }
func (f *fakeServer) heartbeatCount() int32 { return atomic.LoadInt32(&f.heartbeats) }

// --- test harness ----------------------------------------------------------

func startServer(t *testing.T) (*fakeServer, stream.Config) {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	fs := newFakeServer()
	agentv1.RegisterAgentServiceServer(srv, fs)

	go func() {
		if err := srv.Serve(lis); err != nil {
			t.Logf("server exited: %v", err)
		}
	}()
	t.Cleanup(func() {
		srv.Stop()
		_ = lis.Close()
	})

	dialer := func(_ context.Context, _ string) (*grpc.ClientConn, error) {
		return grpc.NewClient("passthrough:///bufnet",
			grpc.WithContextDialer(func(_ context.Context, _ string) (net.Conn, error) {
				return lis.DialContext(context.Background())
			}),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		)
	}

	return fs, stream.Config{
		Endpoint:     "bufnet",
		NodeName:     "node-1",
		Hostname:     "host.local",
		AgentVersion: "0.1.0",
		AgentToken:   "secret",
		Topology: topology.Static{T: &agentv1.Topology{
			IommuEnabled: true,
			Gpus:         []*agentv1.Gpu{{Index: 0, PciAddress: "0000:81:00.0"}},
		}},
		Dialer:         dialer,
		InitialBackoff: 10 * time.Millisecond,
		MaxBackoff:     100 * time.Millisecond,
	}
}

// --- tests -----------------------------------------------------------------

func TestClient_RegistersAndHeartbeats(t *testing.T) {
	t.Parallel()

	fs, cfg := startServer(t)

	client, err := stream.New(cfg)
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- client.Run(ctx) }()

	select {
	case reg := <-fs.registerCh:
		if reg.NodeName != "node-1" || reg.AgentToken != "secret" {
			t.Fatalf("unexpected register: %+v", reg)
		}
		if len(reg.Topology.GetGpus()) != 1 {
			t.Fatalf("expected 1 gpu reported, got %d", len(reg.Topology.GetGpus()))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for Register")
	}

	// Wait for at least two heartbeats (1s interval, allow slack).
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if fs.heartbeatCount() >= 2 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got := fs.heartbeatCount(); got < 2 {
		t.Fatalf("heartbeats: got %d, want ≥2", got)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("client did not shut down on cancel")
	}
}

func TestClient_Reconnects(t *testing.T) {
	t.Parallel()

	// Two listeners: the first one we close mid-test to force a reconnect.
	lisA := bufconn.Listen(1 << 20)
	srvA := grpc.NewServer()
	fsA := newFakeServer()
	agentv1.RegisterAgentServiceServer(srvA, fsA)
	go func() { _ = srvA.Serve(lisA) }()
	t.Cleanup(func() { srvA.Stop(); _ = lisA.Close() })

	lisB := bufconn.Listen(1 << 20)
	srvB := grpc.NewServer()
	fsB := newFakeServer()
	agentv1.RegisterAgentServiceServer(srvB, fsB)
	go func() { _ = srvB.Serve(lisB) }()
	t.Cleanup(func() { srvB.Stop(); _ = lisB.Close() })

	// Dialer chooses A until A is closed, then B.
	var closed atomic.Bool
	dialer := func(_ context.Context, _ string) (*grpc.ClientConn, error) {
		target := lisA
		if closed.Load() {
			target = lisB
		}
		return grpc.NewClient("passthrough:///bufnet",
			grpc.WithContextDialer(func(_ context.Context, _ string) (net.Conn, error) {
				return target.DialContext(context.Background())
			}),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		)
	}

	client, err := stream.New(stream.Config{
		Endpoint:       "bufnet",
		NodeName:       "node-1",
		AgentToken:     "secret",
		Topology:       topology.Empty(),
		Dialer:         dialer,
		InitialBackoff: 10 * time.Millisecond,
		MaxBackoff:     100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		_ = client.Run(ctx)
		close(done)
	}()

	// Wait for the first registration on A.
	select {
	case <-fsA.registerCh:
	case <-time.After(3 * time.Second):
		t.Fatal("no register on A")
	}

	// Kill A; client must reconnect to B.
	closed.Store(true)
	srvA.Stop()
	_ = lisA.Close()

	select {
	case <-fsB.registerCh:
	case <-time.After(5 * time.Second):
		t.Fatalf("no register on B after failover; A=%d B=%d", fsA.registerCount(), fsB.registerCount())
	}

	cancel()
	<-done
}
