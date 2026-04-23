package grpcsrv

import (
	"errors"
	"sync"

	"github.com/google/uuid"

	agentv1 "hybridcloud/shared/proto/agent/v1"
)

// AgentRegistry tracks active compute-agent streams so REST handlers can push
// ControlMessages (CreateInstance, DestroyInstance, Ping) to a specific node.
//
// Stream handlers Register(nodeID, ch) when they start serving an agent and
// invoke the returned cleanup when the stream ends. The channel feeds the
// stream's send goroutine.
type AgentRegistry struct {
	mu     sync.RWMutex
	byNode map[uuid.UUID]chan<- *agentv1.ControlMessage
}

// NewAgentRegistry returns an empty registry.
func NewAgentRegistry() *AgentRegistry {
	return &AgentRegistry{byNode: make(map[uuid.UUID]chan<- *agentv1.ControlMessage)}
}

// ErrAgentNotConnected is returned by Send when the target node has no open
// stream. The caller should report a transient failure; the agent will
// reconnect and retry on its own schedule.
var ErrAgentNotConnected = errors.New("agent: not connected")

// Register adds a channel for nodeID. If an existing channel is registered it
// is replaced — the older stream is being superseded by a reconnection.
// Returns a cleanup closure the caller MUST invoke when the stream ends.
func (r *AgentRegistry) Register(nodeID uuid.UUID, ch chan<- *agentv1.ControlMessage) func() {
	r.mu.Lock()
	r.byNode[nodeID] = ch
	r.mu.Unlock()

	return func() {
		r.mu.Lock()
		if cur, ok := r.byNode[nodeID]; ok && cur == ch {
			delete(r.byNode, nodeID)
		}
		r.mu.Unlock()
	}
}

// Send delivers msg to the registered stream. It returns ErrAgentNotConnected
// when the node is not currently connected. The channel is buffered (set by
// the caller) so a blocked receiver produces a blocking Send here — callers
// who need a timeout should select around the result.
func (r *AgentRegistry) Send(nodeID uuid.UUID, msg *agentv1.ControlMessage) error {
	r.mu.RLock()
	ch, ok := r.byNode[nodeID]
	r.mu.RUnlock()
	if !ok {
		return ErrAgentNotConnected
	}
	ch <- msg
	return nil
}

// Connected reports whether an agent is currently registered.
func (r *AgentRegistry) Connected(nodeID uuid.UUID) bool {
	r.mu.RLock()
	_, ok := r.byNode[nodeID]
	r.mu.RUnlock()
	return ok
}
