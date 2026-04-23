// Package topology describes the GPU layout of a compute-agent host.
//
// Phase 2 uses a static source: agents that have nvidia-smi installed can
// wire in the linux collector; agents on GPU-less developer boxes use Fake
// to keep the control plane working end-to-end.
package topology

import (
	"context"

	agentv1 "hybridcloud/shared/proto/agent/v1"
)

// Collector returns a snapshot of the host's GPU topology. Callers treat a
// nil topology as "empty" rather than an error — an agent that has no GPUs
// should still register.
type Collector interface {
	Collect(ctx context.Context) (*agentv1.Topology, error)
}

// Static returns the same canned topology every call. Useful for Phase 2
// development and unit tests.
type Static struct {
	T *agentv1.Topology
}

// Collect satisfies Collector.
func (s Static) Collect(_ context.Context) (*agentv1.Topology, error) {
	if s.T == nil {
		return &agentv1.Topology{}, nil
	}
	return s.T, nil
}

// Empty returns a Collector that reports no GPUs.
func Empty() Collector { return Static{T: &agentv1.Topology{}} }
