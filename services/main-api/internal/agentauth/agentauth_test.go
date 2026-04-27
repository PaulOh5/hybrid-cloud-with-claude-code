package agentauth_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"

	"hybridcloud/services/main-api/internal/agentauth"
	"hybridcloud/services/main-api/internal/db/dbstore"
)

// fakeRepo is an in-memory NodeAuthRepo. Each test owns one to keep cases
// independent and avoid timing dependencies.
type fakeRepo struct {
	tokens          map[uuid.UUID][]dbstore.NodeToken
	nodes           map[uuid.UUID]nodeView
	listCalls       atomic.Int64
	policyCalls     atomic.Int64
	listShouldError error
}

type nodeView struct {
	accessPolicy string
	ownerTeamID  uuid.NullUUID
	agentVersion string
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{
		tokens: map[uuid.UUID][]dbstore.NodeToken{},
		nodes:  map[uuid.UUID]nodeView{},
	}
}

func (f *fakeRepo) ListActiveNodeTokens(ctx context.Context, nodeID uuid.UUID) ([]dbstore.NodeToken, error) {
	f.listCalls.Add(1)
	if f.listShouldError != nil {
		return nil, f.listShouldError
	}
	return append([]dbstore.NodeToken{}, f.tokens[nodeID]...), nil
}

func (f *fakeRepo) NodeAuthView(ctx context.Context, nodeID uuid.UUID) (agentauth.NodeView, error) {
	f.policyCalls.Add(1)
	v, ok := f.nodes[nodeID]
	if !ok {
		return agentauth.NodeView{}, agentauth.ErrNodeNotFound
	}
	return agentauth.NodeView{
		NodeID:       nodeID,
		AccessPolicy: v.accessPolicy,
		OwnerTeamID:  v.ownerTeamID,
		AgentVersion: v.agentVersion,
	}, nil
}

func mustHash(t *testing.T, plain string) string {
	t.Helper()
	h, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	return string(h)
}

func newRequest(t *testing.T, body any) *http.Request {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return httptest.NewRequest(http.MethodPost, "/internal/agent-auth", strings.NewReader(string(raw)))
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode response: %v body=%s", err, rec.Body.String())
	}
	return m
}

func TestAgentAuth_ValidTokenReturns200WithPolicy(t *testing.T) {
	t.Parallel()

	nodeID := uuid.New()
	teamID := uuid.New()
	repo := newFakeRepo()
	repo.tokens[nodeID] = []dbstore.NodeToken{{
		ID:        uuid.New(),
		NodeID:    nodeID,
		TokenHash: mustHash(t, "plaintoken-abc"),
	}}
	repo.nodes[nodeID] = nodeView{
		accessPolicy: "owner_team",
		ownerTeamID:  uuid.NullUUID{UUID: teamID, Valid: true},
		agentVersion: "0.2.0",
	}

	h := agentauth.NewHandler(agentauth.Config{Repo: repo, CacheTTL: time.Minute})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newRequest(t, map[string]string{
		"node_id": nodeID.String(),
		"token":   "plaintoken-abc",
	}))

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	got := decode(t, rec)
	if got["ok"] != true {
		t.Fatalf("ok: %v", got["ok"])
	}
	if got["node_id"] != nodeID.String() {
		t.Fatalf("node_id: %v want %s", got["node_id"], nodeID.String())
	}
	if got["access_policy"] != "owner_team" {
		t.Fatalf("access_policy: %v", got["access_policy"])
	}
	if got["owner_team_id"] != teamID.String() {
		t.Fatalf("owner_team_id: %v", got["owner_team_id"])
	}
	if got["agent_version_seen"] != "0.2.0" {
		t.Fatalf("agent_version_seen: %v", got["agent_version_seen"])
	}
}

func TestAgentAuth_PublicNodeOmitsOwnerTeam(t *testing.T) {
	t.Parallel()

	nodeID := uuid.New()
	repo := newFakeRepo()
	repo.tokens[nodeID] = []dbstore.NodeToken{{
		ID:        uuid.New(),
		NodeID:    nodeID,
		TokenHash: mustHash(t, "tok"),
	}}
	repo.nodes[nodeID] = nodeView{accessPolicy: "public", agentVersion: "0.1.0"}

	h := agentauth.NewHandler(agentauth.Config{Repo: repo, CacheTTL: time.Minute})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newRequest(t, map[string]string{
		"node_id": nodeID.String(),
		"token":   "tok",
	}))

	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", rec.Code, rec.Body.String())
	}
	got := decode(t, rec)
	// owner_team_id must be omitted or empty for public nodes — ssh-proxy
	// reads access_policy first and only consults owner_team_id when needed.
	if v, present := got["owner_team_id"]; present && v != "" && v != nil {
		t.Fatalf("public node should not carry owner_team_id, got %v", v)
	}
}

func TestAgentAuth_InvalidTokenReturns401(t *testing.T) {
	t.Parallel()

	nodeID := uuid.New()
	repo := newFakeRepo()
	repo.tokens[nodeID] = []dbstore.NodeToken{{
		ID:        uuid.New(),
		NodeID:    nodeID,
		TokenHash: mustHash(t, "real-token"),
	}}
	repo.nodes[nodeID] = nodeView{accessPolicy: "public"}

	h := agentauth.NewHandler(agentauth.Config{Repo: repo, CacheTTL: time.Minute})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newRequest(t, map[string]string{
		"node_id": nodeID.String(),
		"token":   "wrong-token",
	}))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401", rec.Code)
	}
}

func TestAgentAuth_RevokedTokenReturns401(t *testing.T) {
	t.Parallel()

	nodeID := uuid.New()
	repo := newFakeRepo()
	// Active list intentionally empty: a revoked token would not appear in
	// ListActiveNodeTokens, so the handler should refuse.
	repo.tokens[nodeID] = nil
	repo.nodes[nodeID] = nodeView{accessPolicy: "public"}

	h := agentauth.NewHandler(agentauth.Config{Repo: repo, CacheTTL: time.Minute})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newRequest(t, map[string]string{
		"node_id": nodeID.String(),
		"token":   "anything",
	}))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: %d", rec.Code)
	}
}

func TestAgentAuth_UnknownNodeReturns401(t *testing.T) {
	t.Parallel()

	repo := newFakeRepo()
	h := agentauth.NewHandler(agentauth.Config{Repo: repo, CacheTTL: time.Minute})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newRequest(t, map[string]string{
		"node_id": uuid.New().String(),
		"token":   "tok",
	}))

	// 401 (not 404) — same as wrong token to avoid leaking node existence.
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: %d", rec.Code)
	}
}

func TestAgentAuth_BadJSONReturns400(t *testing.T) {
	t.Parallel()

	h := agentauth.NewHandler(agentauth.Config{Repo: newFakeRepo(), CacheTTL: time.Minute})
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/internal/agent-auth", strings.NewReader("not json"))
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: %d", rec.Code)
	}
}

func TestAgentAuth_BadNodeIDReturns400(t *testing.T) {
	t.Parallel()

	h := agentauth.NewHandler(agentauth.Config{Repo: newFakeRepo(), CacheTTL: time.Minute})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newRequest(t, map[string]string{
		"node_id": "not-a-uuid",
		"token":   "tok",
	}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: %d", rec.Code)
	}
}

func TestAgentAuth_OnlyAcceptsPOST(t *testing.T) {
	t.Parallel()

	h := agentauth.NewHandler(agentauth.Config{Repo: newFakeRepo(), CacheTTL: time.Minute})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/internal/agent-auth", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status: %d", rec.Code)
	}
}

func TestAgentAuth_CacheHitSkipsRepo(t *testing.T) {
	t.Parallel()

	nodeID := uuid.New()
	repo := newFakeRepo()
	repo.tokens[nodeID] = []dbstore.NodeToken{{
		ID:        uuid.New(),
		NodeID:    nodeID,
		TokenHash: mustHash(t, "tok"),
	}}
	repo.nodes[nodeID] = nodeView{accessPolicy: "public", agentVersion: "0.2.0"}

	h := agentauth.NewHandler(agentauth.Config{Repo: repo, CacheTTL: time.Minute})

	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, newRequest(t, map[string]string{
			"node_id": nodeID.String(),
			"token":   "tok",
		}))
		if rec.Code != http.StatusOK {
			t.Fatalf("iter %d: status %d", i, rec.Code)
		}
	}

	// Plan §S2 (60s cache): only the first call should hit the repo.
	if got := repo.listCalls.Load(); got != 1 {
		t.Fatalf("ListActiveNodeTokens calls: got %d, want 1 (cache should serve 2nd+ requests)", got)
	}
	if got := repo.policyCalls.Load(); got != 1 {
		t.Fatalf("NodeAuthView calls: got %d, want 1", got)
	}
}

func TestAgentAuth_CacheExpires(t *testing.T) {
	t.Parallel()

	nodeID := uuid.New()
	repo := newFakeRepo()
	repo.tokens[nodeID] = []dbstore.NodeToken{{
		ID:        uuid.New(),
		NodeID:    nodeID,
		TokenHash: mustHash(t, "tok"),
	}}
	repo.nodes[nodeID] = nodeView{accessPolicy: "public"}

	now := time.Unix(1_700_000_000, 0)
	clock := &mockClock{now: now}

	h := agentauth.NewHandler(agentauth.Config{
		Repo:     repo,
		CacheTTL: 60 * time.Second,
		Now:      clock.Now,
	})

	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, newRequest(t, map[string]string{
			"node_id": nodeID.String(),
			"token":   "tok",
		}))
		if rec.Code != http.StatusOK {
			t.Fatalf("first batch iter %d: status %d", i, rec.Code)
		}
	}
	if got := repo.listCalls.Load(); got != 1 {
		t.Fatalf("pre-expiry list calls: got %d, want 1", got)
	}

	// Advance past TTL — next call should hit the repo again.
	clock.now = now.Add(61 * time.Second)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newRequest(t, map[string]string{
		"node_id": nodeID.String(),
		"token":   "tok",
	}))
	if rec.Code != http.StatusOK {
		t.Fatalf("post-expiry status %d", rec.Code)
	}
	if got := repo.listCalls.Load(); got != 2 {
		t.Fatalf("post-expiry list calls: got %d, want 2", got)
	}
}

type mockClock struct{ now time.Time }

func (m *mockClock) Now() time.Time { return m.now }

func TestAgentAuth_RevocationInvalidatesCache(t *testing.T) {
	t.Parallel()

	nodeID := uuid.New()
	repo := newFakeRepo()
	repo.tokens[nodeID] = []dbstore.NodeToken{{
		ID:        uuid.New(),
		NodeID:    nodeID,
		TokenHash: mustHash(t, "tok"),
	}}
	repo.nodes[nodeID] = nodeView{accessPolicy: "public"}

	now := time.Unix(1_700_000_000, 0)
	clock := &mockClock{now: now}

	h := agentauth.NewHandler(agentauth.Config{
		Repo:     repo,
		CacheTTL: 60 * time.Second,
		Now:      clock.Now,
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newRequest(t, map[string]string{
		"node_id": nodeID.String(),
		"token":   "tok",
	}))
	if rec.Code != http.StatusOK {
		t.Fatalf("first call: %d", rec.Code)
	}

	// Operator revokes the token.
	repo.tokens[nodeID] = nil
	clock.now = now.Add(61 * time.Second) // past TTL

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, newRequest(t, map[string]string{
		"node_id": nodeID.String(),
		"token":   "tok",
	}))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 after revoke + ttl, got %d", rec.Code)
	}
}
