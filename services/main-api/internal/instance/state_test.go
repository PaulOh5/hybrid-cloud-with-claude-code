package instance_test

import (
	"fmt"
	"testing"

	"hybridcloud/services/main-api/internal/instance"
)

func TestCanTransition_Table(t *testing.T) {
	t.Parallel()

	cases := []struct {
		from, to instance.State
		ok       bool
	}{
		// idempotent
		{instance.StatePending, instance.StatePending, true},
		{instance.StateRunning, instance.StateRunning, true},
		{instance.StateStopped, instance.StateStopped, true},

		// pending → …
		{instance.StatePending, instance.StateProvisioning, true},
		{instance.StatePending, instance.StateFailed, true},
		{instance.StatePending, instance.StateStopped, true},
		{instance.StatePending, instance.StateRunning, false}, // must go via provisioning

		// provisioning → …
		{instance.StateProvisioning, instance.StateRunning, true},
		{instance.StateProvisioning, instance.StateFailed, true},
		{instance.StateProvisioning, instance.StateStopping, true}, // user cancels mid-boot
		{instance.StateProvisioning, instance.StateStopped, false},
		{instance.StateProvisioning, instance.StatePending, false},

		// running → …
		{instance.StateRunning, instance.StateStopping, true},
		{instance.StateRunning, instance.StateFailed, true},
		{instance.StateRunning, instance.StateStopped, false}, // must go via stopping
		{instance.StateRunning, instance.StateProvisioning, false},

		// stopping → …
		{instance.StateStopping, instance.StateStopped, true},
		{instance.StateStopping, instance.StateFailed, true},
		{instance.StateStopping, instance.StateRunning, false},

		// terminals reject outgoing transitions
		{instance.StateStopped, instance.StateRunning, false},
		{instance.StateFailed, instance.StateRunning, false},
		{instance.StateFailed, instance.StateStopped, false},
	}

	for _, tc := range cases {
		tc := tc
		name := fmt.Sprintf("%s->%s", tc.from, tc.to)
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := instance.CanTransition(tc.from, tc.to); got != tc.ok {
				t.Fatalf("CanTransition(%q, %q) = %v, want %v", tc.from, tc.to, got, tc.ok)
			}
		})
	}
}

func TestIsTerminal(t *testing.T) {
	t.Parallel()
	for _, s := range instance.AllStates {
		want := s == instance.StateStopped || s == instance.StateFailed
		if got := instance.IsTerminal(s); got != want {
			t.Errorf("IsTerminal(%q)=%v, want %v", s, got, want)
		}
	}
}

func TestValidate(t *testing.T) {
	t.Parallel()
	for _, s := range instance.AllStates {
		if err := instance.Validate(s); err != nil {
			t.Errorf("Validate(%q) unexpected error: %v", s, err)
		}
	}
	if err := instance.Validate("bogus"); err == nil {
		t.Fatal("expected error on unknown state")
	}
}
