//go:build integration

package integration_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"hybridcloud/services/main-api/internal/db/dbstore"
	"hybridcloud/services/main-api/internal/slot"
)

func seedSlots(t *testing.T, ctx context.Context, q *dbstore.Queries, nodeID uuid.UUID, count int, gpuSize int32) []dbstore.GpuSlot {
	t.Helper()
	var out []dbstore.GpuSlot
	for i := 0; i < count; i++ {
		row, err := q.InsertSlot(ctx, dbstore.InsertSlotParams{
			NodeID:       nodeID,
			SlotIndex:    int32(i),
			GpuCount:     gpuSize,
			GpuIndices:   []int32{int32(i)},
			NvlinkDomain: "",
		})
		if err != nil {
			t.Fatalf("seed slot %d: %v", i, err)
		}
		out = append(out, row)
	}
	return out
}

func TestSlotRepo_ReserveAndRelease(t *testing.T) {
	t.Parallel()

	url := startPostgres(t)
	migrateUp(t, url)

	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	ctx := context.Background()
	q := dbstore.New(pool)
	repo := slot.NewRepo(pool, q)

	node := seedNode(t, ctx, q)
	seedSlots(t, ctx, q, node.ID, 4, 1)

	res, err := repo.Reserve(ctx, node.ID, 1, 1)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if len(res.Slots) != 1 || res.Slots[0].Status != dbstore.SlotStatusReserved {
		t.Fatalf("unexpected reservation: %+v", res.Slots)
	}
	if idx := res.SlotIndices(); idx[0] != 0 {
		t.Fatalf("expected slot 0 first, got %v", idx)
	}

	// Binding promotes to in_use. BindToInstance sets the gpu_slots FK, so
	// the instance row must already exist.
	inst, err := q.CreateInstance(ctx, dbstore.CreateInstanceParams{
		NodeID:      node.ID,
		Name:        "slot-bind-test",
		MemoryMb:    1024,
		Vcpus:       1,
		GpuCount:    1,
		SlotIndices: []int32{0},
		SshPubkeys:  []string{},
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	instanceID := inst.ID
	if err := repo.BindToInstance(ctx, res, instanceID); err != nil {
		t.Fatalf("bind: %v", err)
	}
	rows, _ := q.ListSlotsForNode(ctx, node.ID)
	if rows[0].Status != dbstore.SlotStatusInUse {
		t.Fatalf("status: %s", rows[0].Status)
	}

	// Release on instance teardown.
	released, err := repo.ReleaseForInstance(ctx, instanceID)
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	if released != 1 {
		t.Fatalf("expected 1 released, got %d", released)
	}
	rows, _ = q.ListSlotsForNode(ctx, node.ID)
	if rows[0].Status != dbstore.SlotStatusFree {
		t.Fatalf("status: %s", rows[0].Status)
	}
}

func TestSlotRepo_NoFreeSlotsReturnsError(t *testing.T) {
	t.Parallel()

	url := startPostgres(t)
	migrateUp(t, url)

	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	ctx := context.Background()
	q := dbstore.New(pool)
	repo := slot.NewRepo(pool, q)
	node := seedNode(t, ctx, q)
	// No slots seeded.

	_, err = repo.Reserve(ctx, node.ID, 1, 1)
	if !errors.Is(err, slot.ErrNoFreeSlots) {
		t.Fatalf("expected ErrNoFreeSlots, got %v", err)
	}
}

// TestSlotRepo_ReleaseReservedRollsBack exercises the scheduler rollback
// path: Reserve in one call, then ReleaseReserved to undo.
func TestSlotRepo_ReleaseReservedRollsBack(t *testing.T) {
	t.Parallel()

	url := startPostgres(t)
	migrateUp(t, url)

	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	ctx := context.Background()
	q := dbstore.New(pool)
	repo := slot.NewRepo(pool, q)
	node := seedNode(t, ctx, q)
	seedSlots(t, ctx, q, node.ID, 2, 1)

	res, err := repo.Reserve(ctx, node.ID, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.ReleaseReserved(ctx, res); err != nil {
		t.Fatalf("release reserved: %v", err)
	}

	count, err := q.CountFreeSlotsByGPUCount(ctx, dbstore.CountFreeSlotsByGPUCountParams{
		NodeID:   node.ID,
		GpuCount: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("expected 2 free after release, got %d", count)
	}
}

// TestSlotRepo_ConcurrentReservation is the main correctness claim for the
// advisory-lock design: N concurrent single-slot reservations against a
// capacity-M pool yield exactly min(N, M) successes and no double-book.
func TestSlotRepo_ConcurrentReservation(t *testing.T) {
	t.Parallel()

	url := startPostgres(t)
	migrateUp(t, url)

	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	ctx := context.Background()
	q := dbstore.New(pool)
	repo := slot.NewRepo(pool, q)
	node := seedNode(t, ctx, q)

	const capacity = 8
	seedSlots(t, ctx, q, node.ID, capacity, 1)

	const attempts = 50
	var successes, noFree atomic.Int64
	var wg sync.WaitGroup
	mu := sync.Mutex{}
	seenIDs := map[uuid.UUID]int{}

	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			res, err := repo.Reserve(ctx, node.ID, 1, 1)
			if err != nil {
				if errors.Is(err, slot.ErrNoFreeSlots) {
					noFree.Add(1)
					return
				}
				t.Errorf("unexpected error: %v", err)
				return
			}
			successes.Add(1)
			mu.Lock()
			for _, s := range res.Slots {
				seenIDs[s.ID]++
			}
			mu.Unlock()
		}()
	}
	wg.Wait()

	if got := int(successes.Load()); got != capacity {
		t.Fatalf("successes: got %d, want %d", got, capacity)
	}
	if got := int(noFree.Load()); got != attempts-capacity {
		t.Fatalf("no-free: got %d, want %d", got, attempts-capacity)
	}
	for id, n := range seenIDs {
		if n != 1 {
			t.Fatalf("slot %s reserved %d times (should be 1)", id, n)
		}
	}
}
