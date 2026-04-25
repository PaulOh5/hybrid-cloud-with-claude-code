package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"hybridcloud/services/main-api/internal/api"
	"hybridcloud/services/main-api/internal/db/dbstore"
)

// minimal in-memory balance store for the gate test.
type fakeBalanceStore struct {
	mu sync.Mutex
	b  map[uuid.UUID]int64
}

func (f *fakeBalanceStore) Balance(_ context.Context, id uuid.UUID) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.b[id], nil
}

func setupGateRig(t *testing.T) (http.Handler, *fakeUserStore, *fakeBalanceStore, uuid.UUID, uuid.UUID) {
	t.Helper()

	insts := newFakeInstanceRepo()
	disp := newFakeDispatcher()
	store := newFakeUserStore()
	balances := &fakeBalanceStore{b: map[uuid.UUID]int64{}}

	nodeID := uuid.New()
	getter := nodeOnly{nodes: map[uuid.UUID]dbstore.Node{nodeID: {
		ID:     nodeID,
		Status: dbstore.NodeStatusOnline,
	}}}
	disp.setConnected(nodeID)

	admin := &api.InstanceHandlers{
		Instances:  insts,
		Nodes:      getter,
		Dispatcher: disp,
		BalanceForOwner: func(ctx context.Context, owner uuid.UUID) (int64, error) {
			return balances.Balance(ctx, owner)
		},
	}

	authH := &api.AuthHandlers{Users: store, Config: api.AuthConfig{SessionTTL: time.Hour}}
	router := api.NewUserRouter(api.UserHandlers{
		Auth:      authH,
		Instances: api.NewUserInstanceHandlers(admin),
	}, store)

	alice, err := store.CreateUser(context.Background(), "a@x.com", "longenough01")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	return router, store, balances, alice.ID, nodeID
}

func TestCreditGate_RejectsZeroBalance(t *testing.T) {
	t.Parallel()
	router, _, _, _, nodeID := setupGateRig(t)
	cookie := loginAs(t, userTestRig{router: router}, "a@x.com", "longenough01")

	rr := sendWithCookie(t, router, http.MethodPost, "/api/v1/instances", map[string]any{
		"node_id":   nodeID.String(),
		"name":      "broke-vm",
		"memory_mb": 1024,
		"vcpus":     1,
	}, cookie)
	if rr.Code != http.StatusPaymentRequired {
		t.Fatalf("expected 402, got %d body=%s", rr.Code, rr.Body.String())
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	if body.Error.Code != "insufficient_balance" {
		t.Fatalf("error code: %q", body.Error.Code)
	}
}

func TestCreditGate_AllowsPositiveBalance(t *testing.T) {
	t.Parallel()
	router, _, balances, aliceID, nodeID := setupGateRig(t)
	balances.b[aliceID] = 100_000
	cookie := loginAs(t, userTestRig{router: router}, "a@x.com", "longenough01")

	rr := sendWithCookie(t, router, http.MethodPost, "/api/v1/instances", map[string]any{
		"node_id":   nodeID.String(),
		"name":      "ok-vm",
		"memory_mb": 1024,
		"vcpus":     1,
	}, cookie)
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestCreditGate_RejectsNegativeBalance(t *testing.T) {
	t.Parallel()
	router, _, balances, aliceID, nodeID := setupGateRig(t)
	balances.b[aliceID] = -1
	cookie := loginAs(t, userTestRig{router: router}, "a@x.com", "longenough01")

	rr := sendWithCookie(t, router, http.MethodPost, "/api/v1/instances", map[string]any{
		"node_id":   nodeID.String(),
		"name":      "broke-vm",
		"memory_mb": 1024,
		"vcpus":     1,
	}, cookie)
	if rr.Code != http.StatusPaymentRequired {
		t.Fatalf("expected 402, got %d", rr.Code)
	}
}
