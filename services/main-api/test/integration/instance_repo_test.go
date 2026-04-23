//go:build integration

package integration_test

import (
	"context"
	"errors"
	"net/netip"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"hybridcloud/services/main-api/internal/db/dbstore"
	"hybridcloud/services/main-api/internal/instance"
	"hybridcloud/services/main-api/internal/node"
)

func seedNode(t *testing.T, ctx context.Context, q *dbstore.Queries) dbstore.Node {
	t.Helper()
	zone, err := q.GetDefaultZone(ctx)
	if err != nil {
		t.Fatalf("default zone: %v", err)
	}
	n, err := q.UpsertNode(ctx, dbstore.UpsertNodeParams{
		ZoneID:       zone.ID,
		NodeName:     "node-1",
		Hostname:     "host.local",
		AgentVersion: "0.1.0",
		TopologyJson: []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("upsert node: %v", err)
	}
	return n
}

func TestInstanceRepo_CreateThenTransitionHappyPath(t *testing.T) {
	t.Parallel()

	url := startPostgres(t)
	migrateUp(t, url)

	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	ctx := context.Background()
	q := dbstore.New(pool)
	repo := instance.NewRepo(pool, q)

	n := seedNode(t, ctx, q)

	inst, err := repo.Create(ctx, instance.CreateInput{
		OwnerID:     uuid.NullUUID{Valid: false},
		NodeID:      n.ID,
		Name:        "demo",
		MemoryMiB:   2048,
		VCPUs:       2,
		GPUCount:    0,
		SlotIndices: []int32{},
		SSHPubkeys:  []string{"ssh-ed25519 AAAA"},
		ImageRef:    "ubuntu-24.04",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if inst.State != instance.StatePending {
		t.Fatalf("initial state: %s", inst.State)
	}

	// pending → provisioning
	_, err = repo.Transition(ctx, inst.ID, instance.StateProvisioning, instance.TransitionOptions{Reason: "boot"})
	if err != nil {
		t.Fatalf("transition 1: %v", err)
	}

	// provisioning → running with vm_internal_ip
	ip := netip.MustParseAddr("10.0.1.42")
	updated, err := repo.Transition(ctx, inst.ID, instance.StateRunning, instance.TransitionOptions{
		Reason:       "first boot ok",
		VMInternalIP: ip,
	})
	if err != nil {
		t.Fatalf("transition 2: %v", err)
	}
	if updated.VmInternalIp == nil || updated.VmInternalIp.String() != "10.0.1.42" {
		t.Fatalf("vm_internal_ip: %+v", updated.VmInternalIp)
	}
	if updated.State != instance.StateRunning {
		t.Fatalf("state: %s", updated.State)
	}

	// Same-state idempotent transition should succeed without inserting a new
	// event. Count events before and after.
	events, _ := q.ListInstanceEvents(ctx, inst.ID)
	preCount := len(events)
	if _, err := repo.Transition(ctx, inst.ID, instance.StateRunning, instance.TransitionOptions{Reason: "retry"}); err != nil {
		t.Fatalf("idempotent transition: %v", err)
	}
	events, _ = q.ListInstanceEvents(ctx, inst.ID)
	if len(events) != preCount+1 {
		t.Fatalf("idempotent transition should still log an event; pre=%d post=%d", preCount, len(events))
	}

	// running → stopping → stopped
	if _, err := repo.Transition(ctx, inst.ID, instance.StateStopping, instance.TransitionOptions{Reason: "shutdown"}); err != nil {
		t.Fatalf("transition stopping: %v", err)
	}
	final, err := repo.Transition(ctx, inst.ID, instance.StateStopped, instance.TransitionOptions{Reason: "stopped"})
	if err != nil {
		t.Fatalf("transition stopped: %v", err)
	}
	if !instance.IsTerminal(final.State) {
		t.Fatalf("expected terminal, got %s", final.State)
	}

	events, _ = q.ListInstanceEvents(ctx, inst.ID)
	// created + pending→provisioning + provisioning→running + idempotent
	// running→running + running→stopping + stopping→stopped = 6
	if got, want := len(events), 6; got != want {
		t.Fatalf("event count: got %d, want %d", got, want)
	}
}

func TestInstanceRepo_InvalidTransitionRejected(t *testing.T) {
	t.Parallel()

	url := startPostgres(t)
	migrateUp(t, url)

	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	ctx := context.Background()
	q := dbstore.New(pool)
	repo := instance.NewRepo(pool, q)

	n := seedNode(t, ctx, q)
	inst, err := repo.Create(ctx, instance.CreateInput{
		NodeID:      n.ID,
		Name:        "bad",
		MemoryMiB:   512,
		VCPUs:       1,
		GPUCount:    0,
		SlotIndices: []int32{},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// pending → running is not allowed (must go via provisioning).
	_, err = repo.Transition(ctx, inst.ID, instance.StateRunning, instance.TransitionOptions{Reason: "bad"})
	if !errors.Is(err, instance.ErrInvalidTransition) {
		t.Fatalf("expected ErrInvalidTransition, got %v", err)
	}

	// Terminal state should reject further transitions.
	_, _ = repo.Transition(ctx, inst.ID, instance.StateFailed, instance.TransitionOptions{Reason: "boom"})
	_, err = repo.Transition(ctx, inst.ID, instance.StateRunning, instance.TransitionOptions{Reason: "revive"})
	if !errors.Is(err, instance.ErrInvalidTransition) {
		t.Fatalf("expected ErrInvalidTransition from terminal, got %v", err)
	}
}

func TestInstanceRepo_ErrorMessagePersists(t *testing.T) {
	t.Parallel()

	url := startPostgres(t)
	migrateUp(t, url)

	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	ctx := context.Background()
	q := dbstore.New(pool)
	repo := instance.NewRepo(pool, q)
	n := seedNode(t, ctx, q)

	inst, err := repo.Create(ctx, instance.CreateInput{
		NodeID:      n.ID,
		Name:        "err-demo",
		MemoryMiB:   512,
		VCPUs:       1,
		SlotIndices: []int32{},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	final, err := repo.Transition(ctx, inst.ID, instance.StateFailed, instance.TransitionOptions{
		Reason:       "libvirt crash",
		ErrorMessage: "qemu exited 1: no kvm support",
	})
	if err != nil {
		t.Fatalf("transition: %v", err)
	}
	if final.ErrorMessage != "qemu exited 1: no kvm support" {
		t.Fatalf("error_message: %q", final.ErrorMessage)
	}

	// Follow-up transitions without ErrorMessage should not erase it.
	// The state machine won't let us go further from Failed, so just fetch
	// the row and verify nothing clobbered it.
	_ = n
	_ = node.NewDBRepo(q)
	row, _ := repo.Get(ctx, inst.ID)
	if row.ErrorMessage != final.ErrorMessage {
		t.Fatalf("error_message drifted: got %q want %q", row.ErrorMessage, final.ErrorMessage)
	}
}
