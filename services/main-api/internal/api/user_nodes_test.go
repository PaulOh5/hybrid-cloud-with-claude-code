package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"hybridcloud/services/main-api/internal/api"
	"hybridcloud/services/main-api/internal/db/dbstore"
)

type fakeNodeLister struct {
	rows []dbstore.Node
}

func (f *fakeNodeLister) List(_ context.Context) ([]dbstore.Node, error) {
	return f.rows, nil
}

func TestUserNodes_ListOnlyOnline(t *testing.T) {
	t.Parallel()

	store := newFakeUserStore()
	authH := &api.AuthHandlers{
		Users:  store,
		Config: api.AuthConfig{SessionTTL: 60 * 60 * 1_000_000_000},
	}

	online := dbstore.Node{
		ID:           uuid.New(),
		NodeName:     "h20a",
		Status:       dbstore.NodeStatusOnline,
		TopologyJson: []byte(`{"gpus":[{"index":0},{"index":1}]}`),
	}
	offline := dbstore.Node{
		ID:           uuid.New(),
		NodeName:     "h20b",
		Status:       dbstore.NodeStatusOffline,
		TopologyJson: []byte(`{}`),
	}
	lister := &fakeNodeLister{rows: []dbstore.Node{online, offline}}

	router := api.NewUserRouter(api.UserHandlers{
		Auth:  authH,
		Nodes: &api.UserNodeHandlers{Nodes: lister},
	}, store)

	if _, err := store.CreateUser(context.Background(), "u@x.com", "longenough01"); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	loginRR := postJSON(t, router, "/api/v1/auth/login", map[string]string{
		"email":    "u@x.com",
		"password": "longenough01",
	})
	cookie := sessionCookie(loginRR)
	if cookie == nil {
		t.Fatal("no session cookie after login")
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/nodes", nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Nodes []api.UserNodeView `json:"nodes"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Nodes) != 1 || resp.Nodes[0].ID != online.ID || resp.Nodes[0].GPUCount != 2 {
		t.Fatalf("unexpected nodes: %+v", resp.Nodes)
	}
}

func TestUserNodes_RequiresAuth(t *testing.T) {
	t.Parallel()
	store := newFakeUserStore()
	authH := &api.AuthHandlers{
		Users:  store,
		Config: api.AuthConfig{SessionTTL: 60 * 60 * 1_000_000_000},
	}
	router := api.NewUserRouter(api.UserHandlers{
		Auth:  authH,
		Nodes: &api.UserNodeHandlers{Nodes: &fakeNodeLister{}},
	}, store)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/nodes", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status: %d", rr.Code)
	}
}
