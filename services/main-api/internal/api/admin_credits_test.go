package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"hybridcloud/services/main-api/internal/api"
	"hybridcloud/services/main-api/internal/credit"
	"hybridcloud/services/main-api/internal/db/dbstore"
)

// fakeCreditPoster is an in-memory CreditPoster: it preserves idempotency-key
// uniqueness and accumulates a per-user balance so handlers can be tested
// without a postgres dependency.
type fakeCreditPoster struct {
	mu       sync.Mutex
	balances map[uuid.UUID]int64
	keys     map[string]bool
	ledger   map[uuid.UUID][]dbstore.CreditLedger
	nextID   int64
}

func newFakeCreditPoster() *fakeCreditPoster {
	return &fakeCreditPoster{
		balances: map[uuid.UUID]int64{},
		keys:     map[string]bool{},
		ledger:   map[uuid.UUID][]dbstore.CreditLedger{},
	}
}

func (f *fakeCreditPoster) Post(_ context.Context, in credit.PostInput) (dbstore.CreditLedger, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.keys[in.IdempotencyKey] {
		return dbstore.CreditLedger{}, credit.ErrDuplicateIdempotency
	}
	f.keys[in.IdempotencyKey] = true
	f.nextID++
	row := dbstore.CreditLedger{
		ID:             f.nextID,
		UserID:         in.UserID,
		DeltaMilli:     in.DeltaMilli,
		Reason:         in.Reason,
		IdempotencyKey: in.IdempotencyKey,
		InstanceID:     in.InstanceID,
		Metadata:       in.Metadata,
		CreatedAt:      pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}
	f.balances[in.UserID] += in.DeltaMilli
	f.ledger[in.UserID] = append(f.ledger[in.UserID], row)
	return row, nil
}

func (f *fakeCreditPoster) Balance(_ context.Context, userID uuid.UUID) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.balances[userID], nil
}

func (f *fakeCreditPoster) History(_ context.Context, userID uuid.UUID, _ int32) ([]dbstore.CreditLedger, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]dbstore.CreditLedger, len(f.ledger[userID]))
	copy(out, f.ledger[userID])
	return out, nil
}

func adminRecharge(t *testing.T, router http.Handler, userID uuid.UUID, body any) *httptest.ResponseRecorder {
	t.Helper()
	buf, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/admin/users/"+userID.String()+"/credits", bytes.NewReader(buf))
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	return rr
}

func newAdminCreditsRouter(t *testing.T) (http.Handler, *fakeCreditPoster) {
	t.Helper()
	store := newFakeCreditPoster()
	router := api.NewAdminRouter(
		&api.AdminHandlers{Nodes: &fakeRepo{}},
		nil,
		&api.AdminCreditHandlers{Credits: store},
		"tok",
	)
	return router, store
}

func TestAdminRecharge_PersistsAndIncrementsBalance(t *testing.T) {
	t.Parallel()
	router, store := newAdminCreditsRouter(t)
	userID := uuid.New()

	rr := adminRecharge(t, router, userID, map[string]any{
		"delta_milli":     500_000,
		"reason":          "manual_topup",
		"idempotency_key": "topup-1",
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("status: %d body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Entry        api.CreditEntryView `json:"entry"`
		BalanceMilli int64               `json:"balance_milli"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Entry.DeltaMilli != 500_000 || resp.BalanceMilli != 500_000 {
		t.Fatalf("entry/balance: %+v", resp)
	}
	if got := store.balances[userID]; got != 500_000 {
		t.Fatalf("store balance: %d", got)
	}
}

func TestAdminRecharge_DuplicateIdempotency_409(t *testing.T) {
	t.Parallel()
	router, _ := newAdminCreditsRouter(t)
	userID := uuid.New()
	body := map[string]any{
		"delta_milli":     1000,
		"reason":          "x",
		"idempotency_key": "dup",
	}
	if rr := adminRecharge(t, router, userID, body); rr.Code != http.StatusCreated {
		t.Fatalf("first: %d", rr.Code)
	}
	if rr := adminRecharge(t, router, userID, body); rr.Code != http.StatusConflict {
		t.Fatalf("second: %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestAdminRecharge_ValidatesInput(t *testing.T) {
	t.Parallel()
	router, _ := newAdminCreditsRouter(t)
	userID := uuid.New()

	cases := []struct {
		name string
		body map[string]any
		want int
	}{
		{"missing delta", map[string]any{"reason": "x", "idempotency_key": "k"}, http.StatusBadRequest},
		{"missing reason", map[string]any{"delta_milli": 100, "idempotency_key": "k"}, http.StatusBadRequest},
		{"missing key", map[string]any{"delta_milli": 100, "reason": "x"}, http.StatusBadRequest},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if rr := adminRecharge(t, router, userID, tc.body); rr.Code != tc.want {
				t.Fatalf("got %d want %d", rr.Code, tc.want)
			}
		})
	}
}

func TestAdminBalance_ReturnsBalanceAndLedger(t *testing.T) {
	t.Parallel()
	router, _ := newAdminCreditsRouter(t)
	userID := uuid.New()

	for i, key := range []string{"a", "b", "c"} {
		rr := adminRecharge(t, router, userID, map[string]any{
			"delta_milli":     int64(100 * (i + 1)),
			"reason":          "topup",
			"idempotency_key": key,
		})
		if rr.Code != http.StatusCreated {
			t.Fatalf("recharge %s: %d", key, rr.Code)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/admin/users/"+userID.String()+"/credits", nil)
	req.Header.Set("Authorization", "Bearer tok")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: %d", rr.Code)
	}
	var resp struct {
		BalanceMilli int64                 `json:"balance_milli"`
		Ledger       []api.CreditEntryView `json:"ledger"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp.BalanceMilli != 600 {
		t.Fatalf("balance: %d", resp.BalanceMilli)
	}
	if len(resp.Ledger) != 3 {
		t.Fatalf("ledger size: %d", len(resp.Ledger))
	}
}
