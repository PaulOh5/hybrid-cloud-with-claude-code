//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"hybridcloud/services/main-api/internal/db/dbstore"
)

// Phase 2.0 Task 0.3 — verifies the access_policy / node_state / node_tokens
// migration. ADR-011 (access policy enum) and ADR-010 (node_state machine).
//
// The test deliberately drives behavior through the sqlc-generated query
// surface so it doubles as the contract test for NodeAccessPolicy /
// NodeTokenInsert / NodeTokenRevoke / GetActiveNodeToken.

func TestPhase2NodeColumnsHaveSafeDefaults(t *testing.T) {
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
		NodeName:     "phase2-test-node",
		Hostname:     "host.local",
		AgentVersion: "0.2.0",
		TopologyJson: []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("upsert node: %v", err)
	}

	policy, err := q.NodeAccessPolicy(ctx, node.ID)
	if err != nil {
		t.Fatalf("node access policy: %v", err)
	}
	if policy.AccessPolicy != "public" {
		t.Fatalf("access_policy default: got %q, want %q", policy.AccessPolicy, "public")
	}
	if policy.OwnerTeamID.Valid {
		t.Fatalf("owner_team_id default should be NULL, got %v", policy.OwnerTeamID.UUID)
	}
	if policy.NodeState != "online" {
		t.Fatalf("node_state default: got %q, want %q", policy.NodeState, "online")
	}
	if policy.LastDataPlaneAt.Valid {
		t.Fatalf("last_data_plane_at default should be NULL, got %v", policy.LastDataPlaneAt.Time)
	}
}

func TestPhase2NodeTokenInsertAndRevoke(t *testing.T) {
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

	// Seed a node + an admin user (created_by FK target).
	zone, err := q.GetDefaultZone(ctx)
	if err != nil {
		t.Fatalf("get default zone: %v", err)
	}
	node, err := q.UpsertNode(ctx, dbstore.UpsertNodeParams{
		ZoneID:       zone.ID,
		NodeName:     "phase2-token-node",
		Hostname:     "h",
		AgentVersion: "0.2.0",
		TopologyJson: []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("upsert node: %v", err)
	}

	adminID := uuid.New()
	if _, err := pool.Exec(ctx,
		`insert into users (id, email, password_hash, is_admin) values ($1, $2, $3, true)`,
		adminID, "admin@phase2.local", "x",
	); err != nil {
		t.Fatalf("seed admin: %v", err)
	}

	tok, err := q.NodeTokenInsert(ctx, dbstore.NodeTokenInsertParams{
		NodeID:    node.ID,
		TokenHash: "$2a$10$abcdefghijklmnopqrstuvwx",
		CreatedBy: uuid.NullUUID{UUID: adminID, Valid: true},
	})
	if err != nil {
		t.Fatalf("insert token: %v", err)
	}
	if tok.RevokedAt.Valid {
		t.Fatalf("freshly inserted token should not be revoked")
	}

	got, err := q.GetActiveNodeToken(ctx, dbstore.GetActiveNodeTokenParams{
		NodeID:    node.ID,
		TokenHash: "$2a$10$abcdefghijklmnopqrstuvwx",
	})
	if err != nil {
		t.Fatalf("get active token: %v", err)
	}
	if got.ID != tok.ID {
		t.Fatalf("active lookup returned a different token: %s vs %s", got.ID, tok.ID)
	}

	if err := q.NodeTokenRevoke(ctx, tok.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	if _, err := q.GetActiveNodeToken(ctx, dbstore.GetActiveNodeTokenParams{
		NodeID:    node.ID,
		TokenHash: "$2a$10$abcdefghijklmnopqrstuvwx",
	}); err == nil {
		t.Fatalf("expected GetActiveNodeToken to return error after revoke")
	}

	// Sanity: node table still works for the same node and revoked_at is set.
	var revokedAt *time.Time
	if err := pool.QueryRow(ctx, `select revoked_at from node_tokens where id = $1`, tok.ID).Scan(&revokedAt); err != nil {
		t.Fatalf("query revoked_at: %v", err)
	}
	if revokedAt == nil {
		t.Fatalf("expected revoked_at to be set, got NULL")
	}
}

func TestPhase2AccessPolicyConstraint(t *testing.T) {
	t.Parallel()

	url := startPostgres(t)
	migrateUp(t, url)

	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("connect pool: %v", err)
	}
	defer pool.Close()

	zone, err := dbstore.New(pool).GetDefaultZone(context.Background())
	if err != nil {
		t.Fatalf("get default zone: %v", err)
	}

	// 'private' is not allowed; the CHECK should reject it.
	_, err = pool.Exec(context.Background(),
		`insert into nodes (zone_id, node_name, access_policy) values ($1, $2, 'private')`,
		zone.ID, "bad-policy",
	)
	if err == nil {
		t.Fatalf("expected CHECK constraint to reject access_policy='private'")
	}
}
