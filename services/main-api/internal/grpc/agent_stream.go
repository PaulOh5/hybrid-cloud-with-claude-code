// Package grpcsrv implements the main-api gRPC service that compute-agents
// connect to via a persistent bidirectional stream.
package grpcsrv

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"hybridcloud/services/main-api/internal/node"
	agentv1 "hybridcloud/shared/proto/agent/v1"
)

// AgentStreamService serves AgentService/Stream.
type AgentStreamService struct {
	agentv1.UnimplementedAgentServiceServer

	Nodes             node.Repo
	ExpectedToken     string
	DefaultZoneID     uuid.UUID
	HeartbeatInterval time.Duration
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

		if err := s.handleAgentMessage(ctx, n.ID, msg); err != nil {
			s.log().Warn("agent message handler error", "err", err, "node_id", n.ID)
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
		// Instance status is wired in Phase 3; log and drop in Phase 2.
		s.log().Debug("instance status received (phase 2 ignores)", "instance_id", p.InstanceStatus.InstanceId)
		return nil
	case *agentv1.AgentMessage_Ack:
		return nil
	case *agentv1.AgentMessage_Register:
		return status.Error(codes.InvalidArgument, "Register received after initial handshake")
	default:
		return status.Errorf(codes.InvalidArgument, "unknown AgentMessage payload %T", p)
	}
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
	// Use protobuf-JSON so future viewers can decode back to the message.
	return protojsonMarshal(t)
}
