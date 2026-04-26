package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"hybridcloud/services/main-api/internal/api"
	"hybridcloud/services/main-api/internal/db/dbstore"
	"hybridcloud/services/main-api/internal/instance"
	agentv1 "hybridcloud/shared/proto/agent/v1"
)

// makeUserRouter wires the full user router with auth + a fakeInstanceRepo,
// and registers two pre-existing users (alice, bob) in the auth store so
// tests can call /login and obtain real session cookies.
type userTestRig struct {
	router  http.Handler
	store   *fakeUserStore
	insts   *fakeInstanceRepo
	disp    *fakeDispatcher
	getter  nodeOnly
	aliceID uuid.UUID
	bobID   uuid.UUID
}

func setupUserRig(t *testing.T) userTestRig {
	t.Helper()

	insts := newFakeInstanceRepo()
	disp := newFakeDispatcher()
	store := newFakeUserStore()

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
	}

	authH := &api.AuthHandlers{
		Users:  store,
		Config: api.AuthConfig{SessionTTL: 0},
	}
	// SessionTTL must be > 0 for CreateSession; use a sane default.
	authH.Config.SessionTTL = 60 * 60 * 1_000_000_000 // 1h in ns

	router := api.NewUserRouter(api.UserHandlers{
		Auth:      authH,
		Instances: api.NewUserInstanceHandlers(admin),
	}, store)

	alice, err := store.CreateUser(context.Background(), "alice@example.com", "longenough01")
	if err != nil {
		t.Fatalf("seed alice: %v", err)
	}
	bob, err := store.CreateUser(context.Background(), "bob@example.com", "longenough02")
	if err != nil {
		t.Fatalf("seed bob: %v", err)
	}

	return userTestRig{
		router:  router,
		store:   store,
		insts:   insts,
		disp:    disp,
		getter:  getter,
		aliceID: alice.ID,
		bobID:   bob.ID,
	}
}

func loginAs(t *testing.T, rig userTestRig, email, password string) *http.Cookie {
	t.Helper()
	rr := postJSON(t, rig.router, "/api/v1/auth/login", map[string]string{
		"email":    email,
		"password": password,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("login %q: %d body=%s", email, rr.Code, rr.Body.String())
	}
	c := sessionCookie(rr)
	if c == nil {
		t.Fatalf("login %q: no cookie", email)
	}
	return c
}

func sendWithCookie(t *testing.T, h http.Handler, method, path string, body any, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		buf, _ := json.Marshal(body)
		rd = bytes.NewReader(buf)
	}
	var req *http.Request
	if rd != nil {
		req = httptest.NewRequest(method, path, rd)
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// seedInstance inserts a row directly so tests can simulate "user A's
// instance exists" without round-tripping through the user create endpoint.
func seedInstance(t *testing.T, rig userTestRig, ownerID uuid.UUID, name string) dbstore.Instance {
	t.Helper()
	inst, err := rig.insts.Create(context.Background(), instance.CreateInput{
		OwnerID: uuid.NullUUID{UUID: ownerID, Valid: true},
		NodeID:  someNodeID(rig),
		Name:    name,
	})
	if err != nil {
		t.Fatalf("seed instance: %v", err)
	}
	return inst
}

func someNodeID(rig userTestRig) uuid.UUID {
	for id := range rig.getter.nodes {
		return id
	}
	return uuid.Nil
}

// --- tests -----------------------------------------------------------------

func TestUserInstances_RequireAuth(t *testing.T) {
	t.Parallel()
	rig := setupUserRig(t)

	rr := sendWithCookie(t, rig.router, http.MethodGet, "/api/v1/instances", nil, nil)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rr.Code)
	}
}

func TestUserInstances_ListIsScopedByOwner(t *testing.T) {
	t.Parallel()
	rig := setupUserRig(t)

	seedInstance(t, rig, rig.aliceID, "alice-vm")
	seedInstance(t, rig, rig.bobID, "bob-vm")

	cookie := loginAs(t, rig, "alice@example.com", "longenough01")
	rr := sendWithCookie(t, rig.router, http.MethodGet, "/api/v1/instances", nil, cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", rr.Code, rr.Body.String())
	}

	var resp struct {
		Instances []api.InstanceView `json:"instances"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Instances) != 1 || resp.Instances[0].Name != "alice-vm" {
		t.Fatalf("scope leak: %+v", resp.Instances)
	}
}

// S3: user A → DELETE user B's instance → 404 (not 403).
func TestUserInstances_DeleteOtherUserInstance_404(t *testing.T) {
	t.Parallel()
	rig := setupUserRig(t)

	bobInst := seedInstance(t, rig, rig.bobID, "bob-vm")
	// Mark Running so Delete takes the transition path, not the terminal-drop path.
	rig.insts.rows[bobInst.ID].State = dbstore.InstanceStateRunning

	aliceCookie := loginAs(t, rig, "alice@example.com", "longenough01")

	rr := sendWithCookie(t, rig.router, http.MethodDelete, "/api/v1/instances/"+bobInst.ID.String(), nil, aliceCookie)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404 (no enumerate), got %d body=%s", rr.Code, rr.Body.String())
	}
	// Bob's row must still exist + no DestroyInstance dispatched.
	if _, err := rig.insts.Get(context.Background(), bobInst.ID); err != nil {
		t.Fatal("bob's instance should not be deleted")
	}
	rig.disp.mu.Lock()
	defer rig.disp.mu.Unlock()
	if len(rig.disp.sent) != 0 {
		t.Fatalf("dispatcher must not be called: %+v", rig.disp.sent)
	}
}

func TestUserInstances_GetOtherUserInstance_404(t *testing.T) {
	t.Parallel()
	rig := setupUserRig(t)
	bobInst := seedInstance(t, rig, rig.bobID, "bob-vm")
	cookie := loginAs(t, rig, "alice@example.com", "longenough01")

	rr := sendWithCookie(t, rig.router, http.MethodGet, "/api/v1/instances/"+bobInst.ID.String(), nil, cookie)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rr.Code)
	}
}

func TestUserInstances_GetOwnInstance_200(t *testing.T) {
	t.Parallel()
	rig := setupUserRig(t)
	mine := seedInstance(t, rig, rig.aliceID, "alice-vm")
	cookie := loginAs(t, rig, "alice@example.com", "longenough01")

	rr := sendWithCookie(t, rig.router, http.MethodGet, "/api/v1/instances/"+mine.ID.String(), nil, cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestUserInstances_DeleteOwnInstance_TransitionsToStopping(t *testing.T) {
	t.Parallel()
	rig := setupUserRig(t)
	mine := seedInstance(t, rig, rig.aliceID, "alice-vm")
	rig.insts.rows[mine.ID].State = dbstore.InstanceStateRunning

	cookie := loginAs(t, rig, "alice@example.com", "longenough01")

	rr := sendWithCookie(t, rig.router, http.MethodDelete, "/api/v1/instances/"+mine.ID.String(), nil, cookie)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status: %d body=%s", rr.Code, rr.Body.String())
	}

	if len(rig.insts.trans) != 1 || rig.insts.trans[0].To != instance.StateStopping {
		t.Fatalf("expected stopping transition, got %+v", rig.insts.trans)
	}
	rig.disp.mu.Lock()
	defer rig.disp.mu.Unlock()
	if len(rig.disp.sent) != 1 || rig.disp.sent[0].Msg.GetDestroyInstance() == nil {
		t.Fatalf("expected destroy dispatch, got %+v", rig.disp.sent)
	}
}

func TestUserInstances_CreateStampsOwnerID(t *testing.T) {
	t.Parallel()
	rig := setupUserRig(t)
	cookie := loginAs(t, rig, "alice@example.com", "longenough01")

	body := map[string]any{
		"node_id":   someNodeID(rig).String(),
		"name":      "alice-vm",
		"memory_mb": 1024,
		"vcpus":     1,
	}
	rr := sendWithCookie(t, rig.router, http.MethodPost, "/api/v1/instances", body, cookie)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status: %d body=%s", rr.Code, rr.Body.String())
	}

	if len(rig.insts.created) != 1 {
		t.Fatalf("expected 1 row created, got %d", len(rig.insts.created))
	}
	row := rig.insts.created[0]
	if !row.OwnerID.Valid || row.OwnerID.UUID != rig.aliceID {
		t.Fatalf("owner stamp: %+v want %s", row.OwnerID, rig.aliceID)
	}

	// And admin dispatcher saw exactly one CreateInstance.
	rig.disp.mu.Lock()
	defer rig.disp.mu.Unlock()
	if len(rig.disp.sent) != 1 {
		t.Fatalf("dispatcher: %d", len(rig.disp.sent))
	}
	if rig.disp.sent[0].Msg.GetCreateInstance() == nil {
		t.Fatalf("expected CreateInstance, got %T", rig.disp.sent[0].Msg.Payload)
	}
}

// User-facing routes do NOT grant admins access to other users' instances
// — admin operator workflows belong to /api/v1/admin/instances. The 404
// response is deliberate: an admin who lands on the user route should not
// see a different action's audit signal than a regular user would.
func TestUserInstances_AdminGetsNotFoundOnOthers(t *testing.T) {
	t.Parallel()
	rig := setupUserRig(t)
	bobInst := seedInstance(t, rig, rig.bobID, "bob-vm")

	stored := rig.store.users["alice@example.com"]
	stored.IsAdmin = true
	rig.store.users["alice@example.com"] = stored
	rig.store.byID[stored.ID] = stored

	cookie := loginAs(t, rig, "alice@example.com", "longenough01")
	rr := sendWithCookie(t, rig.router, http.MethodGet, "/api/v1/instances/"+bobInst.ID.String(), nil, cookie)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("admin on user route should get 404 on other-owner instance, got %d body=%s",
			rr.Code, rr.Body.String())
	}
}

// Make sure the admin agentv1 import isn't accidentally pruned — without an
// agent payload the test rig never references the package.
var _ = agentv1.ControlMessage{}
