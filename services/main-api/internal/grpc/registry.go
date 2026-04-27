package grpcsrv

import (
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"

	agentv1 "hybridcloud/shared/proto/agent/v1"
)

// agentSendTimeout caps how long Send waits when an agent's send buffer
// is full. A wedged or slow-reading agent should not park HTTP handlers
// indefinitely — return ErrAgentNotConnected so the caller can fail fast
// and let the next Provision attempt see fresh state.
const agentSendTimeout = 2 * time.Second

// AgentRegistry tracks active compute-agent streams so REST handlers can push
// ControlMessages (CreateInstance, DestroyInstance, Ping) to a specific node
// and look up the agent's advertised tunnel endpoint (Phase 6).
//
// Stream handlers Register(nodeID, ch, endpoint) when they start serving an
// agent and invoke the returned cleanup when the stream ends.
type AgentRegistry struct {
	mu    sync.RWMutex
	nodes map[uuid.UUID]*registryEntry
}

type registryEntry struct {
	ch             chan<- *agentv1.ControlMessage
	tunnelEndpoint string
}

// NewAgentRegistry returns an empty registry.
func NewAgentRegistry() *AgentRegistry {
	return &AgentRegistry{nodes: make(map[uuid.UUID]*registryEntry)}
}

// ErrAgentNotConnected is returned by Send when the target node has no open
// stream. The caller should report a transient failure; the agent will
// reconnect and retry on its own schedule.
var ErrAgentNotConnected = errors.New("agent: not connected")

// Register adds a channel for nodeID. If an existing entry exists it is
// replaced — the older stream is being superseded by a reconnection.
// tunnelEndpoint is the TCP address ssh-proxy should dial for this node;
// empty means the agent hasn't advertised one.
func (r *AgentRegistry) Register(nodeID uuid.UUID, ch chan<- *agentv1.ControlMessage, tunnelEndpoint string) func() {
	entry := &registryEntry{ch: ch, tunnelEndpoint: tunnelEndpoint}
	r.mu.Lock()
	r.nodes[nodeID] = entry
	r.mu.Unlock()

	return func() {
		r.mu.Lock()
		if cur, ok := r.nodes[nodeID]; ok && cur == entry {
			delete(r.nodes, nodeID)
		}
		r.mu.Unlock()
	}
}

// Send delivers msg to the registered stream. It returns ErrAgentNotConnected
// when the node is not currently connected or when the agent's send buffer
// has been blocked for longer than agentSendTimeout (indicating a slow or
// stuck reader on the agent side).
func (r *AgentRegistry) Send(nodeID uuid.UUID, msg *agentv1.ControlMessage) error {
	r.mu.RLock()
	entry, ok := r.nodes[nodeID]
	r.mu.RUnlock()
	if !ok {
		return ErrAgentNotConnected
	}
	timer := time.NewTimer(agentSendTimeout)
	defer timer.Stop()
	select {
	case entry.ch <- msg:
		return nil
	case <-timer.C:
		return ErrAgentNotConnected
	}
}

// Connected reports whether an agent is currently registered.
func (r *AgentRegistry) Connected(nodeID uuid.UUID) bool {
	r.mu.RLock()
	_, ok := r.nodes[nodeID]
	r.mu.RUnlock()
	return ok
}

// TunnelEndpoint returns the tunnel address the agent advertised at Register
// time, or ("", false) when the node is not connected or did not advertise.
//
// TODO(Phase 2.2): remove. ssh-proxy resolves node_id -> stream via its own
// muxregistry under ADR-008/012; main-api will stop carrying a tunnel address
// once Phase 2.2 lands.
func (r *AgentRegistry) TunnelEndpoint(nodeID uuid.UUID) (string, bool) {
	r.mu.RLock()
	entry, ok := r.nodes[nodeID]
	r.mu.RUnlock()
	if !ok || entry.tunnelEndpoint == "" {
		return "", false
	}
	return entry.tunnelEndpoint, true
}
