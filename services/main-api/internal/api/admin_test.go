package api_test

import (
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
	"hybridcloud/services/main-api/internal/db/dbstore"
	"hybridcloud/services/main-api/internal/node"
)

// --- fake repo (mirror of the gRPC test; kept package-local for clarity) ---

type fakeRepo struct {
	mu    sync.Mutex
	nodes []dbstore.Node
}

func (f *fakeRepo) UpsertOnline(_ context.Context, _ node.UpsertInput) (dbstore.Node, error) {
	return dbstore.Node{}, nil
}
func (f *fakeRepo) TouchHeartbeat(_ context.Context, _ uuid.UUID) error { return nil }
func (f *fakeRepo) UpdateTopology(_ context.Context, _ uuid.UUID, _ []byte) error {
	return nil
}
func (f *fakeRepo) MarkStaleOffline(_ context.Context, _ time.Time) (int64, error) { return 0, nil }
func (f *fakeRepo) List(_ context.Context) ([]dbstore.Node, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]dbstore.Node, len(f.nodes))
	copy(out, f.nodes)
	return out, nil
}
func (f *fakeRepo) Get(_ context.Context, id uuid.UUID) (dbstore.Node, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, n := range f.nodes {
		if n.ID == id {
			return n, nil
		}
	}
	return dbstore.Node{}, http.ErrNoCookie
}

// --- tests -----------------------------------------------------------------

func TestListNodes_OK(t *testing.T) {
	t.Parallel()

	id := uuid.New()
	now := time.Now().UTC()
	repo := &fakeRepo{
		nodes: []dbstore.Node{{
			ID:              id,
			NodeName:        "node-1",
			Hostname:        "host.local",
			Status:          dbstore.NodeStatusOnline,
			AgentVersion:    "0.1.0",
			TopologyJson:    []byte(`{"gpus":[{"index":0},{"index":1}],"iommu_enabled":true}`),
			RegisteredAt:    pgtype.Timestamptz{Time: now, Valid: true},
			LastHeartbeatAt: pgtype.Timestamptz{Time: now, Valid: true},
		}},
	}

	h := &api.AdminHandlers{Nodes: repo}
	router := api.NewAdminRouter(h, nil, "tok")

	req := httptest.NewRequest(http.MethodGet, "/admin/nodes", nil)
	req.Header.Set("Authorization", "Bearer tok")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}

	var body struct {
		Nodes []api.NodeView `json:"nodes"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Nodes) != 1 {
		t.Fatalf("nodes: got %d, want 1", len(body.Nodes))
	}
	got := body.Nodes[0]
	if got.ID != id || got.Status != "online" || got.GPUCount != 2 {
		t.Fatalf("unexpected view: %+v", got)
	}
	if got.LastHeartbeatAt == nil {
		t.Fatal("expected last_heartbeat_at to be populated")
	}
}

func TestListNodes_MissingToken(t *testing.T) {
	t.Parallel()

	router := api.NewAdminRouter(&api.AdminHandlers{Nodes: &fakeRepo{}}, nil, "tok")
	req := httptest.NewRequest(http.MethodGet, "/admin/nodes", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401", rr.Code)
	}
}

func TestListNodes_WrongToken(t *testing.T) {
	t.Parallel()

	router := api.NewAdminRouter(&api.AdminHandlers{Nodes: &fakeRepo{}}, nil, "tok")
	req := httptest.NewRequest(http.MethodGet, "/admin/nodes", nil)
	req.Header.Set("Authorization", "Bearer nope")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401", rr.Code)
	}
}

func TestListNodes_EmptyTopologyJSON(t *testing.T) {
	t.Parallel()

	repo := &fakeRepo{
		nodes: []dbstore.Node{{
			ID:           uuid.New(),
			NodeName:     "no-gpus",
			Status:       dbstore.NodeStatusOnline,
			TopologyJson: []byte(`{}`),
			RegisteredAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
		}},
	}
	router := api.NewAdminRouter(&api.AdminHandlers{Nodes: repo}, nil, "tok")
	req := httptest.NewRequest(http.MethodGet, "/admin/nodes", nil)
	req.Header.Set("Authorization", "Bearer tok")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d", rr.Code)
	}
	var body struct {
		Nodes []api.NodeView `json:"nodes"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	if body.Nodes[0].GPUCount != 0 {
		t.Fatalf("gpu count: got %d, want 0", body.Nodes[0].GPUCount)
	}
}
