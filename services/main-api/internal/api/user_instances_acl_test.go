package api_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"hybridcloud/services/main-api/internal/api"
	"hybridcloud/services/main-api/internal/db/dbstore"
)

// Phase 2.3 Task 3.1 — ACL on CreateForOwner. owner_team nodes return 404
// to non-members so a probe cannot distinguish "node exists but I'm not
// authorised" from "node does not exist" (S3 enumerate prevention).

type fakeMembership struct {
	in  bool
	err error
}

func (f *fakeMembership) IsUserInTeam(_ context.Context, _, _ uuid.UUID) (bool, error) {
	return f.in, f.err
}

func newUserInstanceACLRig(t *testing.T, betaNodeID, betaTeamID uuid.UUID, membership *fakeMembership) (http.Handler, *http.Cookie) {
	t.Helper()

	insts := newFakeInstanceRepo()
	disp := newFakeDispatcher()
	store := newFakeUserStore()
	disp.setConnected(betaNodeID)

	getter := nodeOnly{nodes: map[uuid.UUID]dbstore.Node{betaNodeID: {
		ID:           betaNodeID,
		Status:       dbstore.NodeStatusOnline,
		AccessPolicy: "owner_team",
		OwnerTeamID:  uuid.NullUUID{UUID: betaTeamID, Valid: true},
	}}}

	admin := &api.InstanceHandlers{
		Instances:      insts,
		Nodes:          getter,
		Dispatcher:     disp,
		TeamMembership: membership,
	}
	authH := &api.AuthHandlers{
		Users:  store,
		Config: api.AuthConfig{SessionTTL: 60 * 60 * 1_000_000_000},
	}
	router := api.NewUserRouter(api.UserHandlers{
		Auth:      authH,
		Instances: api.NewUserInstanceHandlers(admin),
	}, store)

	if _, err := store.CreateUser(context.Background(), "alice@example.com", "longenough01"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rr := postJSON(t, router, "/api/v1/auth/login", map[string]string{
		"email":    "alice@example.com",
		"password": "longenough01",
	})
	cookie := sessionCookie(rr)
	if cookie == nil {
		t.Fatal("no cookie")
	}
	return router, cookie
}

func TestCreateForOwner_OwnerTeamNode_NonMember_Returns404(t *testing.T) {
	t.Parallel()

	betaNodeID := uuid.New()
	betaTeamID := uuid.New()
	membership := &fakeMembership{in: false}
	router, cookie := newUserInstanceACLRig(t, betaNodeID, betaTeamID, membership)

	rr := sendWithCookie(t, router, http.MethodPost, "/api/v1/instances", map[string]any{
		"node_id":   betaNodeID.String(),
		"name":      "denied",
		"memory_mb": 1024,
		"vcpus":     1,
	}, cookie)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404 (S3 enumerate prevention); body=%s", rr.Code, rr.Body.String())
	}
}

func TestCreateForOwner_OwnerTeamNode_Member_Allowed(t *testing.T) {
	t.Parallel()

	betaNodeID := uuid.New()
	betaTeamID := uuid.New()
	membership := &fakeMembership{in: true}
	router, cookie := newUserInstanceACLRig(t, betaNodeID, betaTeamID, membership)

	rr := sendWithCookie(t, router, http.MethodPost, "/api/v1/instances", map[string]any{
		"node_id":   betaNodeID.String(),
		"name":      "allowed",
		"memory_mb": 1024,
		"vcpus":     1,
	}, cookie)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status: got %d, want 201; body=%s", rr.Code, rr.Body.String())
	}
}

func TestCreateForOwner_OwnerTeamNode_MissingTeamID_404(t *testing.T) {
	t.Parallel()

	betaNodeID := uuid.New()
	insts := newFakeInstanceRepo()
	disp := newFakeDispatcher()
	store := newFakeUserStore()
	disp.setConnected(betaNodeID)

	// node has access_policy='owner_team' but OwnerTeamID is NULL — a
	// data inconsistency. The CHECK constraint on the table prevents
	// this in production, but defense in depth: the handler must still
	// 404 rather than dereferencing a missing UUID.
	getter := nodeOnly{nodes: map[uuid.UUID]dbstore.Node{betaNodeID: {
		ID:           betaNodeID,
		Status:       dbstore.NodeStatusOnline,
		AccessPolicy: "owner_team",
	}}}

	admin := &api.InstanceHandlers{
		Instances:      insts,
		Nodes:          getter,
		Dispatcher:     disp,
		TeamMembership: &fakeMembership{in: true}, // would be allowed if the team check ran
	}
	authH := &api.AuthHandlers{Users: store, Config: api.AuthConfig{SessionTTL: 60 * 60 * 1_000_000_000}}
	router := api.NewUserRouter(api.UserHandlers{
		Auth: authH, Instances: api.NewUserInstanceHandlers(admin),
	}, store)
	if _, err := store.CreateUser(context.Background(), "u@x.com", "longenough01"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rr := postJSON(t, router, "/api/v1/auth/login", map[string]string{
		"email": "u@x.com", "password": "longenough01",
	})
	cookie := sessionCookie(rr)

	rr = sendWithCookie(t, router, http.MethodPost, "/api/v1/instances", map[string]any{
		"node_id": betaNodeID.String(), "name": "x", "memory_mb": 1, "vcpus": 1,
	}, cookie)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404", rr.Code)
	}
}

func TestCreateForOwner_OwnerTeamNode_MembershipLookupError_500(t *testing.T) {
	t.Parallel()

	betaNodeID := uuid.New()
	betaTeamID := uuid.New()
	membership := &fakeMembership{err: errors.New("db down")}
	router, cookie := newUserInstanceACLRig(t, betaNodeID, betaTeamID, membership)

	rr := sendWithCookie(t, router, http.MethodPost, "/api/v1/instances", map[string]any{
		"node_id": betaNodeID.String(), "name": "err", "memory_mb": 1, "vcpus": 1,
	}, cookie)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500; body=%s", rr.Code, rr.Body.String())
	}
}
