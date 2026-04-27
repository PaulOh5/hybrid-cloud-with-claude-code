//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"

	"hybridcloud/services/main-api/internal/agentauth"
	"hybridcloud/services/main-api/internal/api"
	"hybridcloud/services/main-api/internal/db/dbstore"
)

// Phase 2.0 Task 0.4 — exercises the full path: real Postgres, sqlc-backed
// PgRepo, bcrypt verification, and the RequireInternalToken middleware.

func TestAgentAuth_E2E_HappyPath(t *testing.T) {
	t.Parallel()

	url := startPostgres(t)
	migrateUp(t, url)

	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("connect pool: %v", err)
	}
	defer pool.Close()

	ctx := context.Background()
	q := dbstore.New(pool)

	zone, err := q.GetDefaultZone(ctx)
	if err != nil {
		t.Fatalf("get default zone: %v", err)
	}
	node, err := q.UpsertNode(ctx, dbstore.UpsertNodeParams{
		ZoneID:       zone.ID,
		NodeName:     "byo-node-e2e",
		Hostname:     "h",
		AgentVersion: "0.2.5",
		TopologyJson: []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("upsert node: %v", err)
	}

	plain := "byo-token-supersecret"
	hash, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	if _, err := q.NodeTokenInsert(ctx, dbstore.NodeTokenInsertParams{
		NodeID:    node.ID,
		TokenHash: string(hash),
		CreatedBy: uuid.NullUUID{},
	}); err != nil {
		t.Fatalf("token insert: %v", err)
	}

	handler := agentauth.NewHandler(agentauth.Config{
		Repo:     agentauth.NewPgRepo(q),
		CacheTTL: 60 * time.Second,
	})

	router := api.NewInternalRouter(api.SSHTicketDeps{}, handler, "internal-bearer")

	// Authenticated request — happy path.
	body, _ := json.Marshal(map[string]string{
		"node_id": node.ID.String(),
		"token":   plain,
	})
	req := httptest.NewRequest(http.MethodPost, "/internal/agent-auth", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer internal-bearer")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["ok"] != true {
		t.Fatalf("ok: %v", got["ok"])
	}
	if got["access_policy"] != "public" {
		t.Fatalf("access_policy: %v", got["access_policy"])
	}
	if got["agent_version_seen"] != "0.2.5" {
		t.Fatalf("agent_version_seen: %v", got["agent_version_seen"])
	}
}

func TestAgentAuth_E2E_RevokedTokenRejectedWithinTTL(t *testing.T) {
	t.Parallel()

	url := startPostgres(t)
	migrateUp(t, url)

	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("connect pool: %v", err)
	}
	defer pool.Close()

	ctx := context.Background()
	q := dbstore.New(pool)

	zone, err := q.GetDefaultZone(ctx)
	if err != nil {
		t.Fatalf("default zone: %v", err)
	}
	node, err := q.UpsertNode(ctx, dbstore.UpsertNodeParams{
		ZoneID: zone.ID, NodeName: "byo-revoke-e2e", Hostname: "h",
		AgentVersion: "0.2.0", TopologyJson: []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	plain := "rotateme"
	hash, _ := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.MinCost)
	tok, err := q.NodeTokenInsert(ctx, dbstore.NodeTokenInsertParams{
		NodeID: node.ID, TokenHash: string(hash),
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Cache TTL = 0s so each call hits the repo — deterministic for the
	// "revocation within TTL" assertion without needing a fake clock.
	handler := agentauth.NewHandler(agentauth.Config{
		Repo:     agentauth.NewPgRepo(q),
		CacheTTL: 0,
	})
	router := api.NewInternalRouter(api.SSHTicketDeps{}, handler, "internal-bearer")

	call := func() int {
		body, _ := json.Marshal(map[string]string{"node_id": node.ID.String(), "token": plain})
		req := httptest.NewRequest(http.MethodPost, "/internal/agent-auth", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer internal-bearer")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec.Code
	}

	if got := call(); got != http.StatusOK {
		t.Fatalf("pre-revoke: got %d", got)
	}
	if err := q.NodeTokenRevoke(ctx, tok.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if got := call(); got != http.StatusUnauthorized {
		t.Fatalf("post-revoke: got %d, want 401", got)
	}
}

func TestAgentAuth_E2E_BearerTokenRequired(t *testing.T) {
	t.Parallel()

	url := startPostgres(t)
	migrateUp(t, url)

	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("connect pool: %v", err)
	}
	defer pool.Close()

	q := dbstore.New(pool)
	handler := agentauth.NewHandler(agentauth.Config{
		Repo: agentauth.NewPgRepo(q), CacheTTL: time.Minute,
	})
	router := api.NewInternalRouter(api.SSHTicketDeps{}, handler, "internal-bearer")

	body, _ := json.Marshal(map[string]string{"node_id": uuid.New().String(), "token": "x"})

	// No Authorization header — RequireInternalToken should reject.
	req := httptest.NewRequest(http.MethodPost, "/internal/agent-auth", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing bearer: got %d, want 401", rec.Code)
	}

	// Wrong bearer — should also be 401.
	req = httptest.NewRequest(http.MethodPost, "/internal/agent-auth", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer wrong")
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong bearer: got %d, want 401", rec.Code)
	}
}
