package agentv1_test

import (
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "hybridcloud/shared/proto/agent/v1"
)

func TestAgentMessageRoundTrip(t *testing.T) {
	t.Parallel()

	orig := &agentv1.AgentMessage{
		Payload: &agentv1.AgentMessage_Register{
			Register: &agentv1.Register{
				NodeName:     "dev-node-01",
				Hostname:     "host.local",
				AgentVersion: "0.1.0",
				AgentToken:   "dev-token",
				Topology: &agentv1.Topology{
					IommuEnabled: true,
					Gpus: []*agentv1.Gpu{{
						Index:       0,
						PciAddress:  "0000:81:00.0",
						Model:       "NVIDIA RTX A6000",
						MemoryBytes: 48 * 1024 * 1024 * 1024,
						IommuGroup:  "23",
						Driver:      "vfio-pci",
					}},
					NvlinkPairs: []*agentv1.NvlinkPair{{GpuAIndex: 0, GpuBIndex: 1}},
				},
			},
		},
	}

	raw, err := proto.Marshal(orig)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	got := &agentv1.AgentMessage{}
	if err := proto.Unmarshal(raw, got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if !proto.Equal(orig, got) {
		t.Fatalf("roundtrip mismatch:\n got  %v\n want %v", got, orig)
	}
}

func TestControlMessage_CreateInstance(t *testing.T) {
	t.Parallel()

	msg := &agentv1.ControlMessage{
		Payload: &agentv1.ControlMessage_CreateInstance{
			CreateInstance: &agentv1.CreateInstance{
				InstanceId:  "11111111-1111-1111-1111-111111111111",
				Name:        "demo",
				MemoryMb:    4096,
				Vcpus:       2,
				SshPubkeys:  []string{"ssh-ed25519 AAAA..."},
				SlotIndices: []int32{0, 1},
				ImageRef:    "ubuntu-24.04",
			},
		},
	}

	ci := msg.GetCreateInstance()
	if ci == nil {
		t.Fatal("expected CreateInstance payload")
	}
	if ci.MemoryMb != 4096 || ci.Vcpus != 2 {
		t.Fatalf("bad fields: %+v", ci)
	}
	if got, want := len(ci.SlotIndices), 2; got != want {
		t.Fatalf("slot count: got %d, want %d", got, want)
	}
}

// Phase 2.0 Task 0.2 — see ADR-012. Register carries the agent-reported mux
// session id (advertised back from ssh-proxy after data-plane attach) and
// Heartbeat carries the running agent version for operational visibility.
func TestPhase2RegisterMuxSessionID(t *testing.T) {
	t.Parallel()

	orig := &agentv1.Register{
		NodeName:     "dev-node-01",
		AgentVersion: "0.2.0",
		AgentToken:   "dev-token",
		MuxSessionId: "mux-7ab3",
	}
	raw, err := proto.Marshal(orig)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := &agentv1.Register{}
	if err := proto.Unmarshal(raw, got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.MuxSessionId != "mux-7ab3" {
		t.Fatalf("MuxSessionId: got %q, want %q", got.MuxSessionId, "mux-7ab3")
	}
}

func TestPhase2HeartbeatAgentVersion(t *testing.T) {
	t.Parallel()

	orig := &agentv1.Heartbeat{
		NodeId:       "11111111-1111-1111-1111-111111111111",
		AgentVersion: "0.2.0",
	}
	raw, err := proto.Marshal(orig)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := &agentv1.Heartbeat{}
	if err := proto.Unmarshal(raw, got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.AgentVersion != "0.2.0" {
		t.Fatalf("AgentVersion: got %q, want %q", got.AgentVersion, "0.2.0")
	}
}

func TestInstanceStatusTimestamp(t *testing.T) {
	t.Parallel()

	observed := time.Date(2026, 4, 23, 12, 0, 0, 0, time.UTC)
	st := &agentv1.InstanceStatus{
		InstanceId: "x",
		State:      agentv1.InstanceState_INSTANCE_STATE_RUNNING,
		ObservedAt: timestamppb.New(observed),
	}
	if !st.ObservedAt.AsTime().Equal(observed) {
		t.Fatalf("timestamp roundtrip mismatch: %v vs %v", st.ObservedAt.AsTime(), observed)
	}
}
