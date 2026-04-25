package billing

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"hybridcloud/services/main-api/internal/credit"
	"hybridcloud/services/main-api/internal/db/dbstore"
	agentv1 "hybridcloud/shared/proto/agent/v1"
)

// fakeCredit accumulates per-user balance and dedupes on idempotency key.
type fakeCredit struct {
	mu       sync.Mutex
	balances map[uuid.UUID]int64
	keys     map[string]bool
}

func newFakeCredit() *fakeCredit {
	return &fakeCredit{balances: map[uuid.UUID]int64{}, keys: map[string]bool{}}
}

func (f *fakeCredit) Post(_ context.Context, in credit.PostInput) (dbstore.CreditLedger, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.keys[in.IdempotencyKey] {
		return dbstore.CreditLedger{}, credit.ErrDuplicateIdempotency
	}
	f.keys[in.IdempotencyKey] = true
	f.balances[in.UserID] += in.DeltaMilli
	return dbstore.CreditLedger{ID: int64(len(f.keys))}, nil
}

func (f *fakeCredit) UsersWithNegativeBalance(_ context.Context) ([]uuid.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []uuid.UUID{}
	for id, b := range f.balances {
		if b <= 0 {
			out = append(out, id)
		}
	}
	return out, nil
}

type fakeInstanceList struct {
	billable []dbstore.ListBillableRunningInstancesRow
	byOwner  map[uuid.UUID][]dbstore.Instance
	listErr  error
}

func (f *fakeInstanceList) ListBillableRunningInstances(_ context.Context) ([]dbstore.ListBillableRunningInstancesRow, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.billable, nil
}

func (f *fakeInstanceList) ListInstances(_ context.Context, ownerID uuid.NullUUID) ([]dbstore.Instance, error) {
	if !ownerID.Valid {
		return nil, errors.New("owner required for sweep")
	}
	return f.byOwner[ownerID.UUID], nil
}

type fakeDispatcher struct {
	mu   sync.Mutex
	sent []sentMsg
}

type sentMsg struct {
	NodeID uuid.UUID
	Msg    *agentv1.ControlMessage
}

func (f *fakeDispatcher) Send(nodeID uuid.UUID, msg *agentv1.ControlMessage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, sentMsg{NodeID: nodeID, Msg: msg})
	return nil
}

func makeWorker(insts InstanceLister, credits CreditPoster, disp Dispatcher) *Worker {
	return &Worker{
		Instances:  insts,
		Credits:    credits,
		Rates:      &RateTable{Rates: map[int32]int64{0: 100, 1: 16667, 2: 33333}},
		Dispatcher: disp,
		Now:        func() time.Time { return time.Unix(60, 0).UTC() }, // bucket=1
	}
}

func TestWorker_ChargesEachRunningInstance(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	owner := uuid.New()
	instID := uuid.New()
	insts := &fakeInstanceList{
		billable: []dbstore.ListBillableRunningInstancesRow{{
			ID:       instID,
			OwnerID:  uuid.NullUUID{UUID: owner, Valid: true},
			GpuCount: 1,
		}},
	}
	credits := newFakeCredit()
	w := makeWorker(insts, credits, nil)

	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := credits.balances[owner]; got != -16667 {
		t.Fatalf("owner balance: %d", got)
	}
}

func TestWorker_IsIdempotentInsideBucket(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	owner := uuid.New()
	insts := &fakeInstanceList{
		billable: []dbstore.ListBillableRunningInstancesRow{{
			ID:       uuid.New(),
			OwnerID:  uuid.NullUUID{UUID: owner, Valid: true},
			GpuCount: 2,
		}},
	}
	credits := newFakeCredit()
	w := makeWorker(insts, credits, nil)

	for i := 0; i < 5; i++ {
		if err := w.RunOnce(ctx); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
	if got := credits.balances[owner]; got != -33333 {
		t.Fatalf("balance after 5 ticks in same bucket: %d", got)
	}
}

func TestWorker_DifferentBucketsCharge(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	owner := uuid.New()
	insts := &fakeInstanceList{
		billable: []dbstore.ListBillableRunningInstancesRow{{
			ID:       uuid.New(),
			OwnerID:  uuid.NullUUID{UUID: owner, Valid: true},
			GpuCount: 1,
		}},
	}
	credits := newFakeCredit()
	w := makeWorker(insts, credits, nil)

	// bucket 1
	if err := w.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	// bucket 2
	w.Now = func() time.Time { return time.Unix(120, 0).UTC() }
	if err := w.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := credits.balances[owner]; got != -33334 {
		t.Fatalf("expected -33334 (2 minutes), got %d", got)
	}
}

func TestWorker_SkipsAdminOwnedInstance(t *testing.T) {
	t.Parallel()
	insts := &fakeInstanceList{
		billable: []dbstore.ListBillableRunningInstancesRow{{
			ID:       uuid.New(),
			OwnerID:  uuid.NullUUID{Valid: false},
			GpuCount: 4,
		}},
	}
	credits := newFakeCredit()
	w := makeWorker(insts, credits, nil)
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(credits.balances) != 0 {
		t.Fatalf("expected no charges, got %+v", credits.balances)
	}
}

func TestWorker_SweepDispatchesDestroyForBankruptUser(t *testing.T) {
	t.Parallel()
	owner := uuid.New()
	instID := uuid.New()
	nodeID := uuid.New()

	credits := newFakeCredit()
	credits.balances[owner] = -1 // already negative

	insts := &fakeInstanceList{
		billable: []dbstore.ListBillableRunningInstancesRow{},
		byOwner: map[uuid.UUID][]dbstore.Instance{
			owner: {{
				ID:        instID,
				OwnerID:   uuid.NullUUID{UUID: owner, Valid: true},
				NodeID:    nodeID,
				State:     dbstore.InstanceStateRunning,
				CreatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
			}},
		},
	}
	disp := &fakeDispatcher{}
	w := makeWorker(insts, credits, disp)

	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	disp.mu.Lock()
	defer disp.mu.Unlock()
	if len(disp.sent) != 1 {
		t.Fatalf("dispatcher: %d", len(disp.sent))
	}
	got := disp.sent[0].Msg.GetDestroyInstance()
	if got == nil || got.InstanceId != instID.String() {
		t.Fatalf("payload: %+v", got)
	}
}

func TestWorker_SweepIgnoresStoppedInstances(t *testing.T) {
	t.Parallel()
	owner := uuid.New()
	credits := newFakeCredit()
	credits.balances[owner] = -100

	insts := &fakeInstanceList{
		billable: []dbstore.ListBillableRunningInstancesRow{},
		byOwner: map[uuid.UUID][]dbstore.Instance{
			owner: {{
				ID:      uuid.New(),
				OwnerID: uuid.NullUUID{UUID: owner, Valid: true},
				State:   dbstore.InstanceStateStopped,
			}},
		},
	}
	disp := &fakeDispatcher{}
	w := makeWorker(insts, credits, disp)

	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	disp.mu.Lock()
	defer disp.mu.Unlock()
	if len(disp.sent) != 0 {
		t.Fatalf("should not dispatch for stopped instances, got %d", len(disp.sent))
	}
}
