package grpcsrv

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"hybridcloud/services/main-api/internal/db/dbstore"
	"hybridcloud/services/main-api/internal/instance"
	agentv1 "hybridcloud/shared/proto/agent/v1"
)

// fakeInstances records Transition calls so registry-flow tests can assert on
// them without touching postgres.
type fakeInstances struct {
	transitions []transitionArgs
	nextErr     error
}

type transitionArgs struct {
	ID   uuid.UUID
	To   instance.State
	Opts instance.TransitionOptions
}

func (f *fakeInstances) Transition(
	_ context.Context,
	id uuid.UUID,
	to instance.State,
	opts instance.TransitionOptions,
) (dbstore.Instance, error) {
	f.transitions = append(f.transitions, transitionArgs{id, to, opts})
	if f.nextErr != nil {
		err := f.nextErr
		f.nextErr = nil
		return dbstore.Instance{}, err
	}
	return dbstore.Instance{ID: id, State: to}, nil
}

func TestRegistry_RegisterAndSend(t *testing.T) {
	t.Parallel()

	reg := NewAgentRegistry()
	nodeID := uuid.New()

	ch := make(chan *agentv1.ControlMessage, 4)
	cleanup := reg.Register(nodeID, ch, "127.0.0.1:8082")
	defer cleanup()

	if !reg.Connected(nodeID) {
		t.Fatal("expected Connected=true")
	}

	msg := &agentv1.ControlMessage{
		Payload: &agentv1.ControlMessage_Ping{
			Ping: &agentv1.Ping{},
		},
	}
	if err := reg.Send(nodeID, msg); err != nil {
		t.Fatalf("send: %v", err)
	}
	select {
	case got := <-ch:
		if got.GetPing() == nil {
			t.Fatal("expected ping")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("did not receive ping")
	}
}

func TestRegistry_SendToUnknownNode(t *testing.T) {
	t.Parallel()

	reg := NewAgentRegistry()
	if err := reg.Send(uuid.New(), &agentv1.ControlMessage{}); err != ErrAgentNotConnected {
		t.Fatalf("expected ErrAgentNotConnected, got %v", err)
	}
}

func TestRegistry_DoubleRegisterReplaces(t *testing.T) {
	t.Parallel()

	reg := NewAgentRegistry()
	id := uuid.New()

	first := make(chan *agentv1.ControlMessage, 1)
	cleanup1 := reg.Register(id, first, "")

	second := make(chan *agentv1.ControlMessage, 1)
	cleanup2 := reg.Register(id, second, "")
	defer cleanup2()

	// cleanup1 now refers to a stale entry and must not delete second.
	cleanup1()
	if !reg.Connected(id) {
		t.Fatal("stale cleanup should not have removed the replacement")
	}

	_ = reg.Send(id, &agentv1.ControlMessage{
		Payload: &agentv1.ControlMessage_Ping{Ping: &agentv1.Ping{}},
	})
	select {
	case <-second:
	default:
		t.Fatal("second registration should have received the send")
	}
	select {
	case <-first:
		t.Fatal("first (stale) channel should not have received")
	default:
	}
}

func TestStream_PushesRegistryMessagesToAgent(t *testing.T) {
	t.Parallel()

	repo := newFakeRepo()
	inst := &fakeInstances{}
	registry := NewAgentRegistry()

	svc := &AgentStreamService{
		Nodes:             repo,
		Instances:         inst,
		Registry:          registry,
		ExpectedToken:     "secret",
		DefaultZoneID:     uuid.New(),
		HeartbeatInterval: 5 * time.Second,
	}
	cli, stop := startTestServer(t, svc)
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	s, err := cli.Stream(ctx)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}

	if err := s.Send(&agentv1.AgentMessage{
		Payload: &agentv1.AgentMessage_Register{
			Register: &agentv1.Register{
				NodeName:   "node-e2e",
				AgentToken: "secret",
			},
		},
	}); err != nil {
		t.Fatalf("send register: %v", err)
	}
	ack, err := s.Recv()
	if err != nil {
		t.Fatalf("recv ack: %v", err)
	}
	nodeID, err := uuid.Parse(ack.GetRegisterAck().NodeId)
	if err != nil {
		t.Fatalf("bad node id: %v", err)
	}

	// Wait for the registry to record this stream (send goroutine starts
	// right after RegisterAck).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !registry.Connected(nodeID) {
		time.Sleep(10 * time.Millisecond)
	}
	if !registry.Connected(nodeID) {
		t.Fatal("agent did not register in AgentRegistry")
	}

	// Push a CreateInstance through the registry; the stream send-loop must
	// forward it to the client.
	instanceID := uuid.New()
	err = registry.Send(nodeID, &agentv1.ControlMessage{
		Payload: &agentv1.ControlMessage_CreateInstance{
			CreateInstance: &agentv1.CreateInstance{
				InstanceId: instanceID.String(),
				Name:       "demo",
				MemoryMb:   1024,
				Vcpus:      1,
			},
		},
	})
	if err != nil {
		t.Fatalf("registry send: %v", err)
	}

	got, err := s.Recv()
	if err != nil {
		t.Fatalf("recv forwarded: %v", err)
	}
	ci := got.GetCreateInstance()
	if ci == nil || ci.InstanceId != instanceID.String() {
		t.Fatalf("unexpected message: %+v", got.Payload)
	}

	// Client reports back InstanceStatus running. The service should
	// transition the instance.
	if err := s.Send(&agentv1.AgentMessage{
		Payload: &agentv1.AgentMessage_InstanceStatus{
			InstanceStatus: &agentv1.InstanceStatus{
				InstanceId:   instanceID.String(),
				State:        agentv1.InstanceState_INSTANCE_STATE_RUNNING,
				VmInternalIp: "10.0.0.5",
				ErrorMessage: "",
			},
		},
	}); err != nil {
		t.Fatalf("send instance status: %v", err)
	}
	_ = s.CloseSend()

	// Drain.
	for {
		if _, err := s.Recv(); err != nil {
			break
		}
	}

	// Poll for the Transition call.
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if n := len(inst.transitions); n >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(inst.transitions) == 0 {
		t.Fatal("expected Transition to be called")
	}
	tr := inst.transitions[0]
	if tr.ID != instanceID || tr.To != instance.StateRunning {
		t.Fatalf("unexpected transition: %+v", tr)
	}
	if !tr.Opts.VMInternalIP.IsValid() || tr.Opts.VMInternalIP.String() != "10.0.0.5" {
		t.Fatalf("vm_internal_ip: %v", tr.Opts.VMInternalIP)
	}

	// After the stream ends, the registry entry should be gone.
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && registry.Connected(nodeID) {
		time.Sleep(10 * time.Millisecond)
	}
	if registry.Connected(nodeID) {
		t.Fatal("registry entry should be cleared after stream ends")
	}

	// make sure the atomic-based unused import doesn't linger.
	_ = atomic.LoadInt32(new(int32))
}
