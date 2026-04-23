package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"hybridcloud/services/main-api/internal/api"
	"hybridcloud/services/main-api/internal/db/dbstore"
	grpcsrv "hybridcloud/services/main-api/internal/grpc"
	"hybridcloud/services/main-api/internal/instance"
	agentv1 "hybridcloud/shared/proto/agent/v1"
)

// --- fakes -----------------------------------------------------------------

type fakeInstanceRepo struct {
	mu       sync.Mutex
	rows     map[uuid.UUID]*dbstore.Instance
	created  []dbstore.Instance
	trans    []transition
	failNext bool
}

type transition struct {
	ID   uuid.UUID
	To   instance.State
	Opts instance.TransitionOptions
}

func newFakeInstanceRepo() *fakeInstanceRepo {
	return &fakeInstanceRepo{rows: map[uuid.UUID]*dbstore.Instance{}}
}

func (f *fakeInstanceRepo) Create(_ context.Context, in instance.CreateInput) (dbstore.Instance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failNext {
		f.failNext = false
		return dbstore.Instance{}, errors.New("forced")
	}
	id := uuid.New()
	now := time.Now()
	row := &dbstore.Instance{
		ID:          id,
		NodeID:      in.NodeID,
		Name:        in.Name,
		State:       dbstore.InstanceStatePending,
		MemoryMb:    in.MemoryMiB,
		Vcpus:       in.VCPUs,
		GpuCount:    in.GPUCount,
		SlotIndices: in.SlotIndices,
		SshPubkeys:  in.SSHPubkeys,
		CreatedAt:   pgtype.Timestamptz{Time: now, Valid: true},
		UpdatedAt:   pgtype.Timestamptz{Time: now, Valid: true},
	}
	f.rows[id] = row
	f.created = append(f.created, *row)
	return *row, nil
}

func (f *fakeInstanceRepo) Transition(_ context.Context, id uuid.UUID, to instance.State, opts instance.TransitionOptions) (dbstore.Instance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.trans = append(f.trans, transition{id, to, opts})
	row, ok := f.rows[id]
	if !ok {
		return dbstore.Instance{}, errors.New("not found")
	}
	row.State = to
	if opts.ErrorMessage != "" {
		row.ErrorMessage = opts.ErrorMessage
	}
	return *row, nil
}

func (f *fakeInstanceRepo) Get(_ context.Context, id uuid.UUID) (dbstore.Instance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.rows[id]; ok {
		return *r, nil
	}
	return dbstore.Instance{}, errors.New("not found")
}

func (f *fakeInstanceRepo) Delete(_ context.Context, id uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.rows, id)
	return nil
}

func (f *fakeInstanceRepo) ListForOwner(_ context.Context, _ uuid.NullUUID) ([]dbstore.Instance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]dbstore.Instance, 0, len(f.rows))
	for _, v := range f.rows {
		out = append(out, *v)
	}
	return out, nil
}

type fakeDispatcher struct {
	mu        sync.Mutex
	sent      []sentMsg
	connected map[uuid.UUID]bool
	sendErr   error
}

type sentMsg struct {
	NodeID uuid.UUID
	Msg    *agentv1.ControlMessage
}

func newFakeDispatcher() *fakeDispatcher {
	return &fakeDispatcher{connected: map[uuid.UUID]bool{}}
}

func (f *fakeDispatcher) Send(nodeID uuid.UUID, msg *agentv1.ControlMessage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sendErr != nil {
		return f.sendErr
	}
	f.sent = append(f.sent, sentMsg{nodeID, msg})
	return nil
}

func (f *fakeDispatcher) Connected(nodeID uuid.UUID) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connected[nodeID]
}

func (f *fakeDispatcher) setConnected(id uuid.UUID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.connected[id] = true
}

// --- tests -----------------------------------------------------------------

func makeRouter(t *testing.T, nodes api.NodeGetter, insts api.InstanceRepo, disp api.AgentDispatcher) http.Handler {
	t.Helper()
	return api.NewAdminRouter(
		&api.AdminHandlers{Nodes: &fakeRepo{}},
		&api.InstanceHandlers{Instances: insts, Nodes: nodes, Dispatcher: disp},
		"tok",
	)
}

// smallNodeGetter wraps fakeRepo to satisfy NodeGetter without exposing List.
type nodeOnly struct{ nodes map[uuid.UUID]dbstore.Node }

func (n nodeOnly) Get(_ context.Context, id uuid.UUID) (dbstore.Node, error) {
	if v, ok := n.nodes[id]; ok {
		return v, nil
	}
	return dbstore.Node{}, errors.New("not found")
}

func TestCreateInstance_Success(t *testing.T) {
	t.Parallel()

	nodeID := uuid.New()
	getter := nodeOnly{nodes: map[uuid.UUID]dbstore.Node{nodeID: {
		ID:     nodeID,
		Status: dbstore.NodeStatusOnline,
	}}}
	insts := newFakeInstanceRepo()
	disp := newFakeDispatcher()
	disp.setConnected(nodeID)

	router := makeRouter(t, getter, insts, disp)

	body, _ := json.Marshal(map[string]any{
		"node_id":     nodeID.String(),
		"name":        "demo",
		"memory_mb":   2048,
		"vcpus":       2,
		"ssh_pubkeys": []string{"ssh-ed25519 AAAA"},
		"image_ref":   "ubuntu-24.04",
	})
	req := httptest.NewRequest(http.MethodPost, "/admin/instances", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status: %d; body=%s", rr.Code, rr.Body.String())
	}

	var resp struct {
		Instance api.InstanceView `json:"instance"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp.Instance.State != "pending" {
		t.Fatalf("state: %s", resp.Instance.State)
	}

	// Agent received a CreateInstance.
	disp.mu.Lock()
	defer disp.mu.Unlock()
	if len(disp.sent) != 1 {
		t.Fatalf("dispatcher sends: %d", len(disp.sent))
	}
	ci := disp.sent[0].Msg.GetCreateInstance()
	if ci == nil || ci.Name != "demo" || ci.MemoryMb != 2048 || ci.Vcpus != 2 {
		t.Fatalf("unexpected create payload: %+v", ci)
	}
}

func TestCreateInstance_NodeOffline(t *testing.T) {
	t.Parallel()

	nodeID := uuid.New()
	getter := nodeOnly{nodes: map[uuid.UUID]dbstore.Node{nodeID: {
		ID:     nodeID,
		Status: dbstore.NodeStatusOffline,
	}}}
	insts := newFakeInstanceRepo()
	disp := newFakeDispatcher()

	router := makeRouter(t, getter, insts, disp)

	body, _ := json.Marshal(map[string]any{
		"node_id":   nodeID.String(),
		"name":      "demo",
		"memory_mb": 1024,
		"vcpus":     1,
	})
	req := httptest.NewRequest(http.MethodPost, "/admin/instances", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusConflict {
		t.Fatalf("status: %d; body=%s", rr.Code, rr.Body.String())
	}
	if len(insts.created) != 0 {
		t.Fatal("should not have created a row for offline node")
	}
}

func TestCreateInstance_DispatchFailureMarksFailed(t *testing.T) {
	t.Parallel()

	nodeID := uuid.New()
	getter := nodeOnly{nodes: map[uuid.UUID]dbstore.Node{nodeID: {
		ID:     nodeID,
		Status: dbstore.NodeStatusOnline,
	}}}
	insts := newFakeInstanceRepo()
	disp := newFakeDispatcher()
	disp.setConnected(nodeID)
	disp.sendErr = grpcsrv.ErrAgentNotConnected

	router := makeRouter(t, getter, insts, disp)

	body, _ := json.Marshal(map[string]any{
		"node_id":   nodeID.String(),
		"name":      "demo",
		"memory_mb": 1024,
		"vcpus":     1,
	})
	req := httptest.NewRequest(http.MethodPost, "/admin/instances", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status: %d", rr.Code)
	}
	if len(insts.trans) != 1 || insts.trans[0].To != instance.StateFailed {
		t.Fatalf("expected Failed transition, got %+v", insts.trans)
	}
}

func TestCreateInstance_ValidatesInput(t *testing.T) {
	t.Parallel()

	getter := nodeOnly{}
	insts := newFakeInstanceRepo()
	disp := newFakeDispatcher()
	router := makeRouter(t, getter, insts, disp)

	cases := []struct {
		name string
		body map[string]any
	}{
		{"bad uuid", map[string]any{"node_id": "not-a-uuid", "name": "x", "memory_mb": 1, "vcpus": 1}},
		{"no name", map[string]any{"node_id": uuid.NewString(), "memory_mb": 1, "vcpus": 1}},
		{"zero memory", map[string]any{"node_id": uuid.NewString(), "name": "x", "memory_mb": 0, "vcpus": 1}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			body, _ := json.Marshal(tc.body)
			req := httptest.NewRequest(http.MethodPost, "/admin/instances", bytes.NewReader(body))
			req.Header.Set("Authorization", "Bearer tok")
			rr := httptest.NewRecorder()
			router.ServeHTTP(rr, req)
			if rr.Code >= 300 && rr.Code < 400 {
				t.Fatal("unexpected redirect")
			}
			if rr.Code != http.StatusBadRequest && rr.Code != http.StatusNotFound {
				t.Fatalf("%s: got %d; body=%s", tc.name, rr.Code, rr.Body.String())
			}
		})
	}
}

func TestDeleteInstance_TransitionsAndDispatches(t *testing.T) {
	t.Parallel()

	nodeID := uuid.New()
	getter := nodeOnly{nodes: map[uuid.UUID]dbstore.Node{nodeID: {ID: nodeID, Status: dbstore.NodeStatusOnline}}}
	insts := newFakeInstanceRepo()
	disp := newFakeDispatcher()
	disp.setConnected(nodeID)

	router := makeRouter(t, getter, insts, disp)

	// Seed an instance directly by calling Create on the repo and then mark
	// it Running via a synthetic transition so Delete follows the normal
	// path.
	inst, _ := insts.Create(context.Background(), instance.CreateInput{
		NodeID: nodeID,
		Name:   "demo",
	})
	insts.rows[inst.ID].State = dbstore.InstanceStateRunning

	req := httptest.NewRequest(http.MethodDelete, "/admin/instances/"+inst.ID.String(), nil)
	req.Header.Set("Authorization", "Bearer tok")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("status: %d; body=%s", rr.Code, rr.Body.String())
	}
	if len(insts.trans) != 1 || insts.trans[0].To != instance.StateStopping {
		t.Fatalf("expected stopping transition, got %+v", insts.trans)
	}
	disp.mu.Lock()
	defer disp.mu.Unlock()
	if len(disp.sent) != 1 || disp.sent[0].Msg.GetDestroyInstance() == nil {
		t.Fatalf("dispatcher: %+v", disp.sent)
	}
}

func TestDeleteInstance_TerminalDropsRow(t *testing.T) {
	t.Parallel()

	nodeID := uuid.New()
	getter := nodeOnly{nodes: map[uuid.UUID]dbstore.Node{nodeID: {ID: nodeID, Status: dbstore.NodeStatusOnline}}}
	insts := newFakeInstanceRepo()
	disp := newFakeDispatcher()

	router := makeRouter(t, getter, insts, disp)

	inst, _ := insts.Create(context.Background(), instance.CreateInput{NodeID: nodeID, Name: "x"})
	insts.rows[inst.ID].State = dbstore.InstanceStateStopped

	req := httptest.NewRequest(http.MethodDelete, "/admin/instances/"+inst.ID.String(), nil)
	req.Header.Set("Authorization", "Bearer tok")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status: %d", rr.Code)
	}
	if _, err := insts.Get(context.Background(), inst.ID); err == nil {
		t.Fatal("expected instance to be deleted")
	}
}
