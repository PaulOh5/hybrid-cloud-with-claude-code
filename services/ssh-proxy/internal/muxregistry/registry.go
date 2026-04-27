// Package muxregistry holds the in-memory mapping from node_id to live
// yamux session. Phase 2.1 Task 1.2.
//
// muxserver hands authenticated sessions to Register; Phase 2.2's
// tunnelhandler will call OpenStream to multiplex user SSH bytes.
//
// Liveness: each registered session gets a goroutine that issues
// yamux.Session.Ping on PingInterval. A failed ping (or a session that
// reports IsClosed) triggers automatic deregistration plus a degraded-
// state report to main-api so the operator dashboard reflects reality
// before the next Heartbeat lands.
//
// Concurrency: a single mutex guards the node->entry map; goroutines that
// run per-session (the health checker) coordinate via per-entry context
// cancellation so Register / Deregister / Close never block on a stuck
// ping.
package muxregistry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
)

// ErrUnknownNode signals that no session is registered for the requested
// node_id. tunnelhandler maps this to a clean SSH-side error.
var ErrUnknownNode = errors.New("muxregistry: no session for node")

// NodeStateReporter is the narrow slice of main-api the registry calls
// when a session goes dead, so the dashboard can flip the node to
// degraded ahead of the next heartbeat (ADR-010 grace machine input).
type NodeStateReporter interface {
	ReportDegraded(ctx context.Context, nodeID string) error
}

// LogReporter is a stub NodeStateReporter that only logs the degraded
// event. Used until Phase 2.4 wires the main-api state-machine endpoint
// (Task 4.1). cmd wiring should swap to the production HTTP reporter at
// that point — no plumbing changes required here.
type LogReporter struct{ Log *slog.Logger }

// ReportDegraded implements NodeStateReporter.
func (r LogReporter) ReportDegraded(_ context.Context, nodeID string) error {
	if r.Log == nil {
		return nil
	}
	r.Log.Info("muxregistry: node would be reported degraded",
		"node_id", nodeID,
		"todo", "Phase 2.4 Task 4.1 wires the main-api endpoint",
	)
	return nil
}

// Config wires the registry's dependencies. PingInterval / PingTimeout
// default to 30s / 5s when zero — production wiring overrides if Phase
// 2.5 A1 data demands a tighter cadence (Q6).
type Config struct {
	Reporter     NodeStateReporter
	PingInterval time.Duration
	PingTimeout  time.Duration
	Log          *slog.Logger
}

// Registry is goroutine-safe. Construct with New, dispose with Close.
type Registry struct {
	reporter     NodeStateReporter
	pingInterval time.Duration
	pingTimeout  time.Duration
	log          *slog.Logger

	mu       sync.Mutex
	sessions map[string]*entry
	closed   bool
}

type entry struct {
	sess         *yamux.Session
	agentVersion string
	cancel       context.CancelFunc
	done         chan struct{}
}

// New constructs a Registry. Caller must invoke Close at shutdown to stop
// the per-session health checkers.
func New(cfg Config) *Registry {
	if cfg.PingInterval <= 0 {
		cfg.PingInterval = 30 * time.Second
	}
	if cfg.PingTimeout <= 0 {
		cfg.PingTimeout = 5 * time.Second
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	return &Registry{
		reporter:     cfg.Reporter,
		pingInterval: cfg.PingInterval,
		pingTimeout:  cfg.PingTimeout,
		log:          cfg.Log,
		sessions:     make(map[string]*entry),
	}
}

// Register attaches sess to nodeID. If a session was already registered
// for nodeID it is closed before the new entry replaces it (ghost session
// prevention) and returned to the caller for logging — Registry itself
// owns the close so the muxserver does not need to repeat it.
func (r *Registry) Register(nodeID string, sess *yamux.Session, agentVersion string) *yamux.Session {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		_ = sess.Close()
		return nil
	}
	prev := r.sessions[nodeID]
	ctx, cancel := context.WithCancel(context.Background())
	e := &entry{
		sess:         sess,
		agentVersion: agentVersion,
		cancel:       cancel,
		done:         make(chan struct{}),
	}
	r.sessions[nodeID] = e
	r.mu.Unlock()

	if prev != nil {
		// Stop the old health checker first; closing the session signals
		// it implicitly via IsClosed but cancel() makes the exit
		// deterministic for callers that observe done.
		prev.cancel()
		_ = prev.sess.Close()
		<-prev.done
	}

	go r.watchHealth(ctx, nodeID, e)

	if prev != nil {
		return prev.sess
	}
	return nil
}

// OpenStream opens a new yamux stream toward the agent registered as
// nodeID. ErrUnknownNode is returned for missing or recently-closed
// sessions; underlying yamux errors propagate verbatim so callers can
// distinguish "node was healthy but the new stream failed" cases.
func (r *Registry) OpenStream(ctx context.Context, nodeID string) (*yamux.Stream, error) {
	r.mu.Lock()
	e, ok := r.sessions[nodeID]
	r.mu.Unlock()
	if !ok {
		return nil, ErrUnknownNode
	}
	if e.sess.IsClosed() {
		return nil, fmt.Errorf("%w: session is closed", ErrUnknownNode)
	}
	stream, err := e.sess.OpenStream()
	if err != nil {
		return nil, fmt.Errorf("muxregistry: open stream for %s: %w", nodeID, err)
	}
	// Honour caller cancellation by closing the stream when ctx is done.
	// yamux.OpenStream itself doesn't take a context; this wraps that
	// gap. The watcher exits when the parent ctx fires (closing the
	// stream) or when the registry's own context cancels via the entry
	// — that path is covered by Close()/Deregister() which already
	// close the session, taking the stream with it.
	if ctx != nil && ctx.Done() != nil {
		go func() {
			<-ctx.Done()
			_ = stream.Close()
		}()
	}
	return stream, nil
}

// Deregister removes nodeID from the registry without reporting degraded
// state — used by callers that already know the session is dead.
func (r *Registry) Deregister(nodeID string) {
	r.mu.Lock()
	e, ok := r.sessions[nodeID]
	if ok {
		delete(r.sessions, nodeID)
	}
	r.mu.Unlock()
	if ok {
		e.cancel()
		_ = e.sess.Close()
	}
}

// Close stops all health checkers and closes every live session.
func (r *Registry) Close() {
	r.mu.Lock()
	r.closed = true
	entries := make([]*entry, 0, len(r.sessions))
	for _, e := range r.sessions {
		entries = append(entries, e)
	}
	r.sessions = make(map[string]*entry)
	r.mu.Unlock()

	for _, e := range entries {
		e.cancel()
		_ = e.sess.Close()
	}
	for _, e := range entries {
		<-e.done
	}
}

// watchHealth pings the session on PingInterval cadence. A failed ping
// (or a session that reports closed) triggers deregistration + a
// degraded-state report to main-api.
func (r *Registry) watchHealth(ctx context.Context, nodeID string, e *entry) {
	defer close(e.done)
	t := time.NewTicker(r.pingInterval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if e.sess.IsClosed() {
				r.handleDeath(nodeID, e, "session closed")
				return
			}
			pingDone := make(chan error, 1)
			go func() {
				_, err := e.sess.Ping()
				pingDone <- err
			}()
			select {
			case err := <-pingDone:
				if err != nil {
					r.handleDeath(nodeID, e, "ping error: "+err.Error())
					return
				}
			case <-time.After(r.pingTimeout):
				r.handleDeath(nodeID, e, "ping timeout")
				return
			case <-ctx.Done():
				return
			}
		}
	}
}

// handleDeath removes the entry from the map (if still present) and
// reports the node as degraded. Idempotent — multiple callers may race.
func (r *Registry) handleDeath(nodeID string, e *entry, reason string) {
	r.mu.Lock()
	cur, ok := r.sessions[nodeID]
	if ok && cur == e {
		delete(r.sessions, nodeID)
	}
	r.mu.Unlock()

	_ = e.sess.Close()

	r.log.Warn("muxregistry: session unhealthy, deregistering",
		"node_id", nodeID,
		"reason", reason,
	)

	if r.reporter != nil {
		// Use a fresh context with a short timeout — the registry
		// shouldn't block on main-api unavailability.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := r.reporter.ReportDegraded(ctx, nodeID); err != nil {
			r.log.Warn("muxregistry: degraded report failed",
				"node_id", nodeID,
				"err", err,
			)
		}
	}
}
