// Package grpcsrv implements the main-api gRPC service that compute-agents
// connect to via a persistent bidirectional stream.
package grpcsrv

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/netip"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"hybridcloud/services/main-api/internal/db/dbstore"
	"hybridcloud/services/main-api/internal/instance"
	"hybridcloud/services/main-api/internal/node"
	agentv1 "hybridcloud/shared/proto/agent/v1"
)

// InstanceUpdater is the narrow slice of instance.Repo the stream uses to
// react to agent-reported status changes.
type InstanceUpdater interface {
	Transition(ctx context.Context, id uuid.UUID, to instance.State, opts instance.TransitionOptions) (dbstore.Instance, error)
}

// AgentStreamService serves AgentService/Stream.
type AgentStreamService struct {
	agentv1.UnimplementedAgentServiceServer

	Nodes             node.Repo
	Instances         InstanceUpdater
	Registry          *AgentRegistry
	ExpectedToken     string
	DefaultZoneID     uuid.UUID
	HeartbeatInterval time.Duration
	SendBuffer        int
	Clock             func() time.Time
	Log               *slog.Logger
}

// Stream implements AgentService/Stream: first message must be Register,
// subsequent messages are Heartbeat / Topology / InstanceStatus / Ack.
func (s *AgentStreamService) Stream(stream agentv1.AgentService_StreamServer) error {
	ctx := stream.Context()
	now := s.clock()

	first, err := stream.Recv()
	if err != nil {
		return status.Errorf(codes.Canceled, "recv Register: %v", err)
	}
	reg := first.GetRegister()
	if reg == nil {
		return status.Error(codes.InvalidArgument, "first message must be Register")
	}
	if reg.AgentToken == "" || reg.AgentToken != s.ExpectedToken {
		return status.Error(codes.Unauthenticated, "invalid agent token")
	}

	topology, err := marshalTopology(reg.Topology)
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "topology json: %v", err)
	}

	n, err := s.Nodes.UpsertOnline(ctx, node.UpsertInput{
		ZoneID:       s.DefaultZoneID,
		NodeName:     reg.NodeName,
		Hostname:     reg.Hostname,
		AgentVersion: reg.AgentVersion,
		TopologyJSON: topology,
	})
	if err != nil {
		s.log().Error("node upsert failed", "err", err, "node_name", reg.NodeName)
		return status.Errorf(codes.Internal, "register: %v", err)
	}

	if err := stream.Send(&agentv1.ControlMessage{
		Payload: &agentv1.ControlMessage_RegisterAck{
			RegisterAck: &agentv1.RegisterAck{
				NodeId:                   n.ID.String(),
				HeartbeatIntervalSeconds: int32(s.heartbeatInterval().Seconds()),
			},
		},
	}); err != nil {
		return status.Errorf(codes.Canceled, "send RegisterAck: %v", err)
	}

	s.log().Info("agent registered",
		"node_id", n.ID,
		"node_name", n.NodeName,
		"agent_version", reg.AgentVersion,
		"elapsed_ms", time.Since(now).Milliseconds(),
	)

	// Register a send channel so the REST layer can push ControlMessages.
	var cleanupRegistry func()
	sendCh := make(chan *agentv1.ControlMessage, s.sendBuffer())
	if s.Registry != nil {
		cleanupRegistry = s.Registry.Register(n.ID, sendCh)
	} else {
		cleanupRegistry = func() {}
	}
	defer cleanupRegistry()

	// Pump outgoing ControlMessages on a goroutine so receive and send are
	// independent — a slow Send must not block Recv.
	sendErrCh := make(chan error, 1)
	go func() {
		sendErrCh <- s.sendLoop(ctx, stream, sendCh)
	}()

	recvErr := s.receiveLoop(ctx, stream, n.ID)

	// Closing the send channel signals sendLoop to exit after draining.
	close(sendCh)
	sendErr := <-sendErrCh

	if recvErr != nil {
		return recvErr
	}
	return sendErr
}

func (s *AgentStreamService) receiveLoop(
	ctx context.Context,
	stream agentv1.AgentService_StreamServer,
	nodeID uuid.UUID,
) error {
	for {
		msg, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			if status.Code(err) == codes.Canceled {
				return nil
			}
			return err
		}

		if err := s.handleAgentMessage(ctx, nodeID, msg); err != nil {
			s.log().Warn("agent message handler error", "err", err, "node_id", nodeID)
		}
	}
}

func (s *AgentStreamService) sendLoop(
	ctx context.Context,
	stream agentv1.AgentService_StreamServer,
	ch <-chan *agentv1.ControlMessage,
) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case msg, ok := <-ch:
			if !ok {
				return nil
			}
			if err := stream.Send(msg); err != nil {
				return err
			}
		}
	}
}

func (s *AgentStreamService) handleAgentMessage(ctx context.Context, nodeID uuid.UUID, msg *agentv1.AgentMessage) error {
	switch p := msg.Payload.(type) {
	case *agentv1.AgentMessage_Heartbeat:
		_ = p
		return s.Nodes.TouchHeartbeat(ctx, nodeID)
	case *agentv1.AgentMessage_Topology:
		raw, err := marshalTopology(p.Topology)
		if err != nil {
			return err
		}
		return s.Nodes.UpdateTopology(ctx, nodeID, raw)
	case *agentv1.AgentMessage_InstanceStatus:
		return s.applyInstanceStatus(ctx, p.InstanceStatus)
	case *agentv1.AgentMessage_Ack:
		return nil
	case *agentv1.AgentMessage_Register:
		return status.Error(codes.InvalidArgument, "Register received after initial handshake")
	default:
		return status.Errorf(codes.InvalidArgument, "unknown AgentMessage payload %T", p)
	}
}

// applyInstanceStatus transitions the instance row to match what the agent
// reports. When Instances is nil (tests that don't care about lifecycle) the
// status is logged and dropped.
func (s *AgentStreamService) applyInstanceStatus(ctx context.Context, st *agentv1.InstanceStatus) error {
	if s.Instances == nil {
		s.log().Debug("instance status received (no updater)", "instance_id", st.InstanceId)
		return nil
	}
	id, err := uuid.Parse(st.InstanceId)
	if err != nil {
		return err
	}
	to, err := mapInstanceState(st.State)
	if err != nil {
		return err
	}

	opts := instance.TransitionOptions{
		Reason: "agent_report",
	}
	if st.ErrorMessage != "" {
		opts.ErrorMessage = st.ErrorMessage
	}
	if st.VmInternalIp != "" {
		if addr, err := netip.ParseAddr(st.VmInternalIp); err == nil {
			opts.VMInternalIP = addr
		}
	}
	_, err = s.Instances.Transition(ctx, id, to, opts)
	if err != nil && !errors.Is(err, instance.ErrInvalidTransition) {
		// Invalid transitions happen when the agent reports an old state we
		// have already advanced past; log and swallow.
		return err
	}
	return nil
}

// StaleSweeper runs MarkStaleOffline on an interval. It exits when ctx is
// cancelled. Safe to launch as a goroutine during server startup.
func (s *AgentStreamService) StaleSweeper(ctx context.Context, interval, ttl time.Duration) error {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			cutoff := s.clock().Add(-ttl)
			affected, err := s.Nodes.MarkStaleOffline(ctx, cutoff)
			if err != nil {
				s.log().Error("stale sweep failed", "err", err)
				continue
			}
			if affected > 0 {
				s.log().Info("marked nodes offline", "count", affected, "cutoff", cutoff)
			}
		}
	}
}

// --- helpers ---------------------------------------------------------------

func (s *AgentStreamService) clock() time.Time {
	if s.Clock != nil {
		return s.Clock()
	}
	return time.Now()
}

func (s *AgentStreamService) heartbeatInterval() time.Duration {
	if s.HeartbeatInterval > 0 {
		return s.HeartbeatInterval
	}
	return 15 * time.Second
}

func (s *AgentStreamService) sendBuffer() int {
	if s.SendBuffer > 0 {
		return s.SendBuffer
	}
	return 16
}

func (s *AgentStreamService) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// marshalTopology produces a compact JSON representation suitable for storing
// in nodes.topology_json. nil topology → empty object so the column stays not-
// nullable.
func marshalTopology(t *agentv1.Topology) ([]byte, error) {
	if t == nil {
		return []byte("{}"), nil
	}
	return protojsonMarshal(t)
}

// mapInstanceState translates the proto enum to the DB enum used by the Repo.
func mapInstanceState(s agentv1.InstanceState) (instance.State, error) {
	switch s {
	case agentv1.InstanceState_INSTANCE_STATE_PROVISIONING:
		return instance.StateProvisioning, nil
	case agentv1.InstanceState_INSTANCE_STATE_RUNNING:
		return instance.StateRunning, nil
	case agentv1.InstanceState_INSTANCE_STATE_STOPPING:
		return instance.StateStopping, nil
	case agentv1.InstanceState_INSTANCE_STATE_STOPPED:
		return instance.StateStopped, nil
	case agentv1.InstanceState_INSTANCE_STATE_FAILED:
		return instance.StateFailed, nil
	default:
		return "", errors.New("unknown instance state")
	}
}
