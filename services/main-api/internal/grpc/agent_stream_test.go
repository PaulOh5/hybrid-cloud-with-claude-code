package grpcsrv

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"hybridcloud/services/main-api/internal/db/dbstore"
	"hybridcloud/services/main-api/internal/node"
	agentv1 "hybridcloud/shared/proto/agent/v1"
)

// --- fake node repo --------------------------------------------------------

type fakeRepo struct {
	mu         sync.Mutex
	nodes      map[string]*dbstore.Node // keyed by node name
	byID       map[uuid.UUID]*dbstore.Node
	heartbeats map[uuid.UUID]int
	sweeps     []time.Time
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{
		nodes:      map[string]*dbstore.Node{},
		byID:       map[uuid.UUID]*dbstore.Node{},
		heartbeats: map[uuid.UUID]int{},
	}
}

func (f *fakeRepo) UpsertOnline(_ context.Context, in node.UpsertInput) (dbstore.Node, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if existing, ok := f.nodes[in.NodeName]; ok {
		existing.Hostname = in.Hostname
		existing.AgentVersion = in.AgentVersion
		existing.Status = dbstore.NodeStatusOnline
		existing.TopologyJson = in.TopologyJSON
		return *existing, nil
	}
	n := &dbstore.Node{
		ID:           uuid.New(),
		ZoneID:       in.ZoneID,
		NodeName:     in.NodeName,
		Hostname:     in.Hostname,
		AgentVersion: in.AgentVersion,
		Status:       dbstore.NodeStatusOnline,
		TopologyJson: in.TopologyJSON,
	}
	f.nodes[n.NodeName] = n
	f.byID[n.ID] = n
	return *n, nil
}

func (f *fakeRepo) TouchHeartbeat(_ context.Context, id uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.heartbeats[id]++
	return nil
}

func (f *fakeRepo) UpdateTopology(_ context.Context, id uuid.UUID, raw []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if n, ok := f.byID[id]; ok {
		n.TopologyJson = raw
	}
	return nil
}

func (f *fakeRepo) MarkStaleOffline(_ context.Context, before time.Time) ([]uuid.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sweeps = append(f.sweeps, before)
	var ids []uuid.UUID
	for _, nd := range f.nodes {
		if nd.Status != dbstore.NodeStatusOffline {
			nd.Status = dbstore.NodeStatusOffline
			ids = append(ids, nd.ID)
		}
	}
	return ids, nil
}

func (f *fakeRepo) NonTerminalInstancesForNode(_ context.Context, _ uuid.UUID) ([]node.NodeInstance, error) {
	// No instance-tracking in this fake; the existing tests exercise the
	// node-level path. The new reaper path has its own targeted test that
	// uses a richer fake.
	return nil, nil
}

func (f *fakeRepo) List(_ context.Context) ([]dbstore.Node, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]dbstore.Node, 0, len(f.nodes))
	for _, n := range f.nodes {
		out = append(out, *n)
	}
	return out, nil
}

func (f *fakeRepo) Get(_ context.Context, id uuid.UUID) (dbstore.Node, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if n, ok := f.byID[id]; ok {
		return *n, nil
	}
	return dbstore.Node{}, status.Error(codes.NotFound, "not found")
}

func (f *fakeRepo) heartbeatCount(id uuid.UUID) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.heartbeats[id]
}

// --- test harness ----------------------------------------------------------

func startTestServer(t *testing.T, svc *AgentStreamService) (agentv1.AgentServiceClient, func()) {
	t.Helper()

	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	agentv1.RegisterAgentServiceServer(srv, svc)

	go func() {
		if err := srv.Serve(lis); err != nil {
			t.Logf("server exited: %v", err)
		}
	}()

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(_ context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(context.Background())
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	cleanup := func() {
		_ = conn.Close()
		srv.Stop()
		_ = lis.Close()
	}
	return agentv1.NewAgentServiceClient(conn), cleanup
}

// --- tests -----------------------------------------------------------------

func TestStream_RegisterAndHeartbeat(t *testing.T) {
	t.Parallel()

	repo := newFakeRepo()
	zoneID := uuid.New()
	svc := &AgentStreamService{
		Nodes:             repo,
		ExpectedToken:     "secret",
		DefaultZoneID:     zoneID,
		HeartbeatInterval: 15 * time.Second,
	}
	cli, stop := startTestServer(t, svc)
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	s, err := cli.Stream(ctx)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}

	if err := s.Send(&agentv1.AgentMessage{
		Payload: &agentv1.AgentMessage_Register{
			Register: &agentv1.Register{
				NodeName:     "node-1",
				Hostname:     "host.local",
				AgentVersion: "0.1.0",
				AgentToken:   "secret",
				Topology: &agentv1.Topology{
					IommuEnabled: true,
					Gpus: []*agentv1.Gpu{{
						Index:      0,
						PciAddress: "0000:81:00.0",
						Model:      "NVIDIA RTX A6000",
						Driver:     "nvidia",
					}},
				},
			},
		},
	}); err != nil {
		t.Fatalf("send Register: %v", err)
	}

	ack, err := s.Recv()
	if err != nil {
		t.Fatalf("recv ack: %v", err)
	}
	ra := ack.GetRegisterAck()
	if ra == nil {
		t.Fatalf("expected RegisterAck, got %T", ack.Payload)
	}
	nodeID, err := uuid.Parse(ra.NodeId)
	if err != nil {
		t.Fatalf("bad uuid in ack: %v", err)
	}

	// Send three heartbeats.
	for i := 0; i < 3; i++ {
		if err := s.Send(&agentv1.AgentMessage{
			Payload: &agentv1.AgentMessage_Heartbeat{
				Heartbeat: &agentv1.Heartbeat{NodeId: nodeID.String()},
			},
		}); err != nil {
			t.Fatalf("send heartbeat %d: %v", i, err)
		}
	}
	if err := s.CloseSend(); err != nil {
		t.Fatalf("close send: %v", err)
	}

	// Drain until EOF so the server observes all heartbeats.
	for {
		if _, err := s.Recv(); err != nil {
			break
		}
	}

	// The handler processes heartbeats asynchronously relative to the send loop;
	// poll briefly.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if repo.heartbeatCount(nodeID) == 3 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got, want := repo.heartbeatCount(nodeID), 3; got != want {
		t.Fatalf("heartbeats: got %d, want %d", got, want)
	}

	// Node should be recorded online with the right metadata.
	nodes, _ := repo.List(context.Background())
	if len(nodes) != 1 {
		t.Fatalf("expected 1 node, got %d", len(nodes))
	}
	if nodes[0].Status != dbstore.NodeStatusOnline {
		t.Fatalf("expected online, got %s", nodes[0].Status)
	}
	if nodes[0].AgentVersion != "0.1.0" {
		t.Fatalf("wrong agent version: %q", nodes[0].AgentVersion)
	}
}

func TestStream_RejectsBadToken(t *testing.T) {
	t.Parallel()

	svc := &AgentStreamService{
		Nodes:         newFakeRepo(),
		ExpectedToken: "secret",
		DefaultZoneID: uuid.New(),
	}
	cli, stop := startTestServer(t, svc)
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	s, err := cli.Stream(ctx)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}

	if err := s.Send(&agentv1.AgentMessage{
		Payload: &agentv1.AgentMessage_Register{
			Register: &agentv1.Register{
				NodeName:   "node-1",
				AgentToken: "wrong",
			},
		},
	}); err != nil {
		t.Fatalf("send Register: %v", err)
	}
	_ = s.CloseSend()
	_, err = s.Recv()
	if err == nil {
		t.Fatalf("expected error on bad token")
	}
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("expected Unauthenticated, got %v", err)
	}
}

func TestStream_RejectsMissingRegister(t *testing.T) {
	t.Parallel()

	svc := &AgentStreamService{
		Nodes:         newFakeRepo(),
		ExpectedToken: "secret",
		DefaultZoneID: uuid.New(),
	}
	cli, stop := startTestServer(t, svc)
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	s, err := cli.Stream(ctx)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}

	if err := s.Send(&agentv1.AgentMessage{
		Payload: &agentv1.AgentMessage_Heartbeat{
			Heartbeat: &agentv1.Heartbeat{},
		},
	}); err != nil {
		t.Fatalf("send: %v", err)
	}
	_ = s.CloseSend()
	_, err = s.Recv()
	if err == nil {
		t.Fatalf("expected error when Register is missing")
	}
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v", err)
	}
}

func TestStaleSweeper_TickAndCancel(t *testing.T) {
	t.Parallel()

	repo := newFakeRepo()
	// Seed one online node so the sweeper has something to flip.
	_, _ = repo.UpsertOnline(context.Background(), node.UpsertInput{
		ZoneID:   uuid.New(),
		NodeName: "n-1",
	})
	svc := &AgentStreamService{Nodes: repo}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	err := svc.StaleSweeper(ctx, 30*time.Millisecond, 60*time.Second)
	if err == nil {
		t.Fatal("sweeper should exit with a ctx error")
	}

	// At least one sweep ran.
	if len(repo.sweeps) == 0 {
		t.Fatalf("expected ≥1 sweep, got %d", len(repo.sweeps))
	}
}
