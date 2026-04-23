// Package instance holds the instance lifecycle state machine and the repo
// that persists transitions + audit events atomically.
package instance

import (
	"errors"
	"fmt"

	"hybridcloud/services/main-api/internal/db/dbstore"
)

// State is a friendlier alias for the dbstore enum, scoped to this package so
// callers do not have to import dbstore directly.
type State = dbstore.InstanceState

const (
	StatePending      State = dbstore.InstanceStatePending
	StateProvisioning State = dbstore.InstanceStateProvisioning
	StateRunning      State = dbstore.InstanceStateRunning
	StateStopping     State = dbstore.InstanceStateStopping
	StateStopped      State = dbstore.InstanceStateStopped
	StateFailed       State = dbstore.InstanceStateFailed
)

// AllStates is useful for tests and enum iteration.
var AllStates = []State{
	StatePending, StateProvisioning, StateRunning,
	StateStopping, StateStopped, StateFailed,
}

// allowedTransitions encodes the state machine. The value is the set of
// states reachable in one step. Same-state (idempotent) is handled by
// CanTransition rather than listed here.
var allowedTransitions = map[State]map[State]struct{}{
	StatePending:      {StateProvisioning: {}, StateFailed: {}, StateStopped: {}},
	StateProvisioning: {StateRunning: {}, StateFailed: {}, StateStopping: {}},
	StateRunning:      {StateStopping: {}, StateFailed: {}},
	StateStopping:     {StateStopped: {}, StateFailed: {}},
	StateStopped:      {}, // terminal
	StateFailed:       {}, // terminal
}

// IsTerminal reports whether the state is final (no further transitions).
func IsTerminal(s State) bool {
	return s == StateStopped || s == StateFailed
}

// ErrInvalidTransition is returned when a requested transition is not allowed
// by the state machine.
var ErrInvalidTransition = errors.New("instance: invalid state transition")

// CanTransition reports whether from→to is allowed. from==to is always
// allowed so callers can retry safely.
func CanTransition(from, to State) bool {
	if from == to {
		return true
	}
	allowed, ok := allowedTransitions[from]
	if !ok {
		return false
	}
	_, ok = allowed[to]
	return ok
}

// Validate ensures the caller's state strings round-trip through the enum.
func Validate(s State) error {
	switch s {
	case StatePending, StateProvisioning, StateRunning,
		StateStopping, StateStopped, StateFailed:
		return nil
	}
	return fmt.Errorf("instance: unknown state %q", s)
}
