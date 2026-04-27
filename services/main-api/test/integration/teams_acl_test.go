//go:build integration

package integration_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"hybridcloud/services/main-api/internal/db/dbstore"
)

// Phase 2.3 Task 3.1 — teams + team_members migration and the
// ACL-aware node listing query.

func seedUser(t *testing.T, pool *pgxpool.Pool, email string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`insert into users (id, email, password_hash) values ($1, $2, 'x')`,
		id, email); err != nil {
		t.Fatalf("seed user %s: %v", email, err)
	}
	return id
}

func TestPhase2_TeamCreate_AddMember_IsMember(t *testing.T) {
	t.Parallel()

	url := startPostgres(t)
	migrateUp(t, url)

	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	ctx := context.Background()
	q := dbstore.New(pool)

	team, err := q.TeamCreate(ctx, dbstore.TeamCreateParams{
		Name:        "alpha",
		Description: "team alpha",
	})
	if err != nil {
		t.Fatalf("team create: %v", err)
	}

	userInTeam := seedUser(t, pool, "in@phase2.local")
	userOutOfTeam := seedUser(t, pool, "out@phase2.local")

	if err := q.TeamMemberAdd(ctx, dbstore.TeamMemberAddParams{
		TeamID: team.ID,
		UserID: userInTeam,
	}); err != nil {
		t.Fatalf("add member: %v", err)
	}
	// Idempotent re-add.
	if err := q.TeamMemberAdd(ctx, dbstore.TeamMemberAddParams{
		TeamID: team.ID,
		UserID: userInTeam,
	}); err != nil {
		t.Fatalf("re-add member must be no-op, got: %v", err)
	}

	in, err := q.IsUserInTeam(ctx, dbstore.IsUserInTeamParams{
		UserID: userInTeam,
		TeamID: team.ID,
	})
	if err != nil {
		t.Fatalf("is member (in): %v", err)
	}
	if !in {
		t.Fatal("expected user to be a member")
	}

	out, err := q.IsUserInTeam(ctx, dbstore.IsUserInTeamParams{
		UserID: userOutOfTeam,
		TeamID: team.ID,
	})
	if err != nil {
		t.Fatalf("is member (out): %v", err)
	}
	if out {
		t.Fatal("expected user to NOT be a member")
	}
}

func TestPhase2_ListNodesAccessibleToUser_FiltersOwnerTeamNodes(t *testing.T) {
	t.Parallel()

	url := startPostgres(t)
	migrateUp(t, url)

	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	ctx := context.Background()
	q := dbstore.New(pool)

	zone, err := q.GetDefaultZone(ctx)
	if err != nil {
		t.Fatalf("zone: %v", err)
	}

	publicNode, err := q.UpsertNode(ctx, dbstore.UpsertNodeParams{
		ZoneID: zone.ID, NodeName: "public-node",
		Hostname: "h", AgentVersion: "0.2.0", TopologyJson: []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("public node: %v", err)
	}
	betaNode, err := q.UpsertNode(ctx, dbstore.UpsertNodeParams{
		ZoneID: zone.ID, NodeName: "beta-node",
		Hostname: "h", AgentVersion: "0.2.0", TopologyJson: []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("beta node: %v", err)
	}

	team, err := q.TeamCreate(ctx, dbstore.TeamCreateParams{Name: "beta-partner-A"})
	if err != nil {
		t.Fatalf("team: %v", err)
	}

	// Flip beta-node to owner_team policy with this team.
	if _, err := pool.Exec(ctx,
		`update nodes set access_policy = 'owner_team', owner_team_id = $1 where id = $2`,
		team.ID, betaNode.ID,
	); err != nil {
		t.Fatalf("update node: %v", err)
	}

	owner := seedUser(t, pool, "owner@phase2.local")
	stranger := seedUser(t, pool, "stranger@phase2.local")
	if err := q.TeamMemberAdd(ctx, dbstore.TeamMemberAddParams{
		TeamID: team.ID, UserID: owner,
	}); err != nil {
		t.Fatalf("add owner: %v", err)
	}

	ownerNodes, err := q.ListNodesAccessibleToUser(ctx, owner)
	if err != nil {
		t.Fatalf("list as owner: %v", err)
	}
	gotOwner := nodeNames(ownerNodes)
	if !contains(gotOwner, "public-node") || !contains(gotOwner, "beta-node") {
		t.Fatalf("owner should see both nodes, got %v", gotOwner)
	}

	strangerNodes, err := q.ListNodesAccessibleToUser(ctx, stranger)
	if err != nil {
		t.Fatalf("list as stranger: %v", err)
	}
	gotStranger := nodeNames(strangerNodes)
	if !contains(gotStranger, "public-node") {
		t.Fatalf("stranger should see public-node, got %v", gotStranger)
	}
	if contains(gotStranger, "beta-node") {
		t.Fatalf("stranger MUST NOT see beta-node (S3 enumerate prevention), got %v", gotStranger)
	}
	_ = publicNode // referenced by name above
}

func nodeNames(rows []dbstore.Node) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.NodeName)
	}
	return out
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
