// Package stream runs the compute-agent side of the AgentService bidi stream.
//
// The run loop dials main-api, sends Register, and then pumps heartbeats
// until the context is cancelled or the stream dies. On error it sleeps with
// exponential backoff and reconnects.
package stream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/timestamppb"

	"hybridcloud/services/compute-agent/internal/topology"
	agentv1 "hybridcloud/shared/proto/agent/v1"
)

// Config controls a single agent run loop.
type Config struct {
	Endpoint     string
	NodeName     string
	Hostname     string
	AgentVersion string
	AgentToken   string
	// TunnelEndpoint is the host:port ssh-proxy should dial to open raw SSH
	// tunnels to VMs on this node. Advertised in Register so main-api can
	// embed it in tickets. Empty means the node opts out of SSH routing.
	//
	// TODO(Phase 2.2): remove. Replaced by muxclient (yamux/TLS outbound to
	// ssh-proxy) under ADR-008/012. Phase 2.0 leaves the field in place to
	// keep Phase 1 compile and runtime intact while data plane is migrated.
	TunnelEndpoint string

	// Topology is consulted on each Register — hot-swap support is Phase 4+.
	Topology topology.Collector

	// Dialer builds a gRPC client connection to Endpoint. If nil, a default
	// insecure dialer is used (fine for Phase 2 in-cluster traffic).
	Dialer func(ctx context.Context, endpoint string) (*grpc.ClientConn, error)

	// InitialBackoff and MaxBackoff bound the reconnect delay.
	InitialBackoff time.Duration
	MaxBackoff     time.Duration

	// OnControl is invoked for each ControlMessage that arrives during a live
	// session. Handlers may enqueue responses through send, which serialises
	// writes back to main-api via the session's single send goroutine.
	OnControl func(ctx context.Context, msg *agentv1.ControlMessage, send func(*agentv1.AgentMessage))

	Log *slog.Logger
}

// Client runs the connect-register-heartbeat loop until its Run context is
// cancelled or the parent returns.
type Client struct {
	cfg Config
}

// New validates cfg and returns a Client ready to Run.
func New(cfg Config) (*Client, error) {
	if cfg.Endpoint == "" {
		return nil, errors.New("stream.New: Endpoint required")
	}
	if cfg.NodeName == "" {
		return nil, errors.New("stream.New: NodeName required")
	}
	if cfg.AgentToken == "" {
		return nil, errors.New("stream.New: AgentToken required")
	}
	if cfg.Topology == nil {
		cfg.Topology = topology.Empty()
	}
	if cfg.InitialBackoff <= 0 {
		cfg.InitialBackoff = time.Second
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = 60 * time.Second
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Dialer == nil {
		cfg.Dialer = defaultDialer
	}
	if cfg.OnControl == nil {
		cfg.OnControl = func(context.Context, *agentv1.ControlMessage, func(*agentv1.AgentMessage)) {}
	}
	return &Client{cfg: cfg}, nil
}

// Run blocks until ctx is done. Each iteration dials, registers, and pumps
// heartbeats; on any error it waits with backoff and retries. A clean
// server-side EOF (runOnce returns nil with ctx still alive) is treated
// the same as any other session end: short backoff, then reconnect.
// Without this an EOF would either terminate the agent or thunder-herd
// reconnect immediately.
func (c *Client) Run(ctx context.Context) error {
	backoff := c.cfg.InitialBackoff

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		err := c.runOnce(ctx)
		if errors.Is(err, context.Canceled) || ctx.Err() != nil {
			return ctx.Err()
		}

		c.cfg.Log.Warn("agent session ended", "err", err, "next_backoff", backoff)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(jitter(backoff)):
		}
		backoff = nextBackoff(backoff, c.cfg.MaxBackoff)
	}
}

func (c *Client) runOnce(ctx context.Context) error {
	conn, err := c.cfg.Dialer(ctx, c.cfg.Endpoint)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer func() { _ = conn.Close() }()

	cli := agentv1.NewAgentServiceClient(conn)
	stream, err := cli.Stream(ctx)
	if err != nil {
		return fmt.Errorf("open stream: %w", err)
	}

	top, err := c.cfg.Topology.Collect(ctx)
	if err != nil {
		return fmt.Errorf("collect topology: %w", err)
	}

	if err := stream.Send(&agentv1.AgentMessage{
		Payload: &agentv1.AgentMessage_Register{
			Register: &agentv1.Register{
				NodeName:            c.cfg.NodeName,
				Hostname:            c.cfg.Hostname,
				AgentVersion:        c.cfg.AgentVersion,
				AgentToken:          c.cfg.AgentToken,
				Topology:            top,
				AgentTunnelEndpoint: c.cfg.TunnelEndpoint,
			},
		},
	}); err != nil {
		return fmt.Errorf("send Register: %w", err)
	}

	ack, err := stream.Recv()
	if err != nil {
		return fmt.Errorf("recv RegisterAck: %w", err)
	}
	ra := ack.GetRegisterAck()
	if ra == nil {
		return fmt.Errorf("expected RegisterAck, got %T", ack.Payload)
	}

	c.cfg.Log.Info("registered",
		"node_id", ra.NodeId,
		"heartbeat_seconds", ra.HeartbeatIntervalSeconds,
	)

	interval := time.Duration(ra.HeartbeatIntervalSeconds) * time.Second
	if interval <= 0 {
		interval = 15 * time.Second
	}

	// Serialise all writes to the stream through outCh so OnControl handlers
	// and the heartbeat ticker never call stream.Send concurrently.
	outCh := make(chan *agentv1.AgentMessage, 16)
	sessionCtx, cancelSession := context.WithCancel(ctx)
	defer cancelSession()

	sendFn := func(m *agentv1.AgentMessage) {
		select {
		case outCh <- m:
		case <-sessionCtx.Done():
		}
	}

	errCh := make(chan error, 3)
	sendDone := make(chan struct{})
	go func() {
		defer close(sendDone)
		errCh <- c.sendLoop(sessionCtx, stream, outCh)
	}()
	go func() { errCh <- c.heartbeatTicker(sessionCtx, ra.NodeId, interval, sendFn) }()
	go func() { errCh <- c.readControl(sessionCtx, stream, sendFn) }()

	err = <-errCh
	cancelSession()
	// sendLoop owns stream.Send; we must wait for it to exit before
	// calling CloseSend. gRPC client streams are not safe for concurrent
	// Send / CloseSend.
	<-sendDone
	_ = stream.CloseSend()
	return err
}

// sendLoop fan-ins messages from both the heartbeat ticker and OnControl
// handlers onto stream.Send. Exits on ctx cancellation or stream error.
func (c *Client) sendLoop(
	ctx context.Context,
	stream agentv1.AgentService_StreamClient,
	out <-chan *agentv1.AgentMessage,
) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case msg, ok := <-out:
			if !ok {
				return nil
			}
			if err := stream.Send(msg); err != nil {
				return fmt.Errorf("send: %w", err)
			}
		}
	}
}

func (c *Client) heartbeatTicker(
	ctx context.Context,
	nodeID string,
	interval time.Duration,
	send func(*agentv1.AgentMessage),
) error {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			send(&agentv1.AgentMessage{
				Payload: &agentv1.AgentMessage_Heartbeat{
					Heartbeat: &agentv1.Heartbeat{
						NodeId: nodeID,
						SentAt: timestamppb.Now(),
					},
				},
			})
		}
	}
}

func (c *Client) readControl(
	ctx context.Context,
	stream agentv1.AgentService_StreamClient,
	send func(*agentv1.AgentMessage),
) error {
	for {
		msg, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		c.cfg.OnControl(ctx, msg, send)
	}
}

// --- helpers ---------------------------------------------------------------

func defaultDialer(ctx context.Context, endpoint string) (*grpc.ClientConn, error) {
	return grpc.NewClient(endpoint,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
}

func nextBackoff(cur, max time.Duration) time.Duration {
	n := cur * 2
	if n > max {
		return max
	}
	return n
}

// jitter spreads reconnect attempts so a fleet does not thunder the api after
// a brief outage. Uses math/rand/v2 — no security requirement on this jitter.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	half := int64(d / 2)
	return time.Duration(int64(d) + rand.Int64N(half+1)) //nolint:gosec // jitter, not crypto
}
