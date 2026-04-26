package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"hybridcloud/services/main-api/internal/api"
	"hybridcloud/services/main-api/internal/db/dbstore"
)

// stubAdminQueries returns canned data for admin handler tests.
type stubAdminQueries struct {
	users []dbstore.ListUsersAdminViewRow
	slots []dbstore.ListSlotsForAdminViewRow
}

func (s *stubAdminQueries) ListUsersAdminView(_ context.Context, _ int32) ([]dbstore.ListUsersAdminViewRow, error) {
	return s.users, nil
}

func (s *stubAdminQueries) ListSlotsForAdminView(_ context.Context) ([]dbstore.ListSlotsForAdminViewRow, error) {
	return s.slots, nil
}

func makeAdminRouter(t *testing.T, store *fakeUserStore, queries api.AdminQueries) http.Handler {
	t.Helper()
	authH := &api.AuthHandlers{
		Users:  store,
		Config: api.AuthConfig{SessionTTL: time.Hour},
	}
	return api.NewUserRouter(api.UserHandlers{
		Auth:  authH,
		Admin: &api.SessionAdminHandlers{Queries: queries},
	}, store)
}

func TestAdminGate_NonAdmin404(t *testing.T) {
	t.Parallel()
	store := newFakeUserStore()
	router := makeAdminRouter(t, store, &stubAdminQueries{})
	if _, err := store.CreateUser(context.Background(), "u@x.com", "longenough01"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cookie := loginAs(t, userTestRig{router: router}, "u@x.com", "longenough01")

	rr := sendWithCookie(t, router, http.MethodGet, "/api/v1/admin/users", nil, cookie)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("non-admin should get 404, got %d", rr.Code)
	}
}

func TestAdminGate_NotAuthenticated_404(t *testing.T) {
	t.Parallel()
	store := newFakeUserStore()
	router := makeAdminRouter(t, store, &stubAdminQueries{})
	rr := sendWithCookie(t, router, http.MethodGet, "/api/v1/admin/users", nil, nil)
	// Admin routes 404 to unauthenticated probes too — a 401 would tell an
	// attacker that the route exists and is admin-gated, which is the
	// enumeration leak we want to avoid.
	if rr.Code != http.StatusNotFound {
		t.Fatalf("unauthenticated → 404, got %d", rr.Code)
	}
}

func TestAdminGate_AdminAccessGranted(t *testing.T) {
	t.Parallel()
	store := newFakeUserStore()
	user, err := store.CreateUser(context.Background(), "admin@x.com", "longenough01")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	user.IsAdmin = true
	store.users["admin@x.com"] = user
	store.byID[user.ID] = user

	queries := &stubAdminQueries{
		users: []dbstore.ListUsersAdminViewRow{{
			ID:                  user.ID,
			Email:               user.Email,
			IsAdmin:             true,
			BalanceMilli:        12_345,
			ActiveInstanceCount: 1,
			CreatedAt:           pgtype.Timestamptz{Time: time.Now(), Valid: true},
		}},
	}
	router := makeAdminRouter(t, store, queries)
	cookie := loginAs(t, userTestRig{router: router}, "admin@x.com", "longenough01")

	rr := sendWithCookie(t, router, http.MethodGet, "/api/v1/admin/users", nil, cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("admin should pass, got %d body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Users []api.AdminUserView `json:"users"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Users) != 1 || resp.Users[0].BalanceMilli != 12_345 {
		t.Fatalf("user view: %+v", resp.Users)
	}
}

func TestAdminListSlots(t *testing.T) {
	t.Parallel()
	store := newFakeUserStore()
	user, _ := store.CreateUser(context.Background(), "admin@x.com", "longenough01")
	user.IsAdmin = true
	store.users["admin@x.com"] = user
	store.byID[user.ID] = user

	slotID := uuid.New()
	queries := &stubAdminQueries{
		slots: []dbstore.ListSlotsForAdminViewRow{{
			ID:        slotID,
			NodeID:    uuid.New(),
			NodeName:  "h20a",
			SlotIndex: 0,
			GpuCount:  2,
			Status:    dbstore.SlotStatusFree,
		}},
	}
	router := makeAdminRouter(t, store, queries)
	cookie := loginAs(t, userTestRig{router: router}, "admin@x.com", "longenough01")

	rr := sendWithCookie(t, router, http.MethodGet, "/api/v1/admin/slots", nil, cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: %d", rr.Code)
	}
	var resp struct {
		Slots []api.AdminSlotView `json:"slots"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if len(resp.Slots) != 1 || resp.Slots[0].ID != slotID {
		t.Fatalf("slot view: %+v", resp.Slots)
	}
}

// Verify the admin path doesn't accidentally short-circuit non-admin routes.
func TestUserRouter_NonAdminRouteWorksForRegularUser(t *testing.T) {
	t.Parallel()
	store := newFakeUserStore()
	router := makeAdminRouter(t, store, &stubAdminQueries{})
	if _, err := store.CreateUser(context.Background(), "u@x.com", "longenough01"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cookie := loginAs(t, userTestRig{router: router}, "u@x.com", "longenough01")

	rr := sendWithCookie(t, router, http.MethodGet, "/api/v1/auth/me", nil, cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("regular /api/v1/auth/me should still 200 for non-admin, got %d", rr.Code)
	}
}

// httptest doesn't expose r.Cookie via httptest.NewRequest; helper to add.
var _ = httptest.NewRequest // keep import in case test reorganization
