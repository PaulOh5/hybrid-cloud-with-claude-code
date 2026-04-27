//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"hybridcloud/services/main-api/internal/agentauth"
	"hybridcloud/services/main-api/internal/api"
	"hybridcloud/services/main-api/internal/db/dbstore"
)

// Phase 2.3 Task 3.2 — admin CLI lifecycle test.
//
// This test exec()s the admin binary so it covers main(), arg parsing,
// and the actual SQL the production binary runs. It also verifies the
// "create -> /internal/agent-auth 200 -> revoke -> 401" round-trip the
// plan calls out in §3.2 Verification.

func buildAdminBinary(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "admin")
	cmd := exec.Command("go", "build", "-o", out, "../../../main-api/cmd/admin")
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		t.Fatalf("go build admin: %v", err)
	}
	return out
}

func runCLI(t *testing.T, bin, dbURL, muxEndpoint string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = append(cmd.Env,
		"DATABASE_URL="+dbURL,
		"SSH_PROXY_MUX_ENDPOINT="+muxEndpoint,
		"PATH=/usr/bin:/bin",
	)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.String(), err
}

func TestAdminCLI_NodeTokenLifecycle(t *testing.T) {
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

	// Seed: a team, a node, and an admin who issues the token.
	zone, err := q.GetDefaultZone(ctx)
	if err != nil {
		t.Fatalf("zone: %v", err)
	}
	if _, err := q.UpsertNode(ctx, dbstore.UpsertNodeParams{
		ZoneID: zone.ID, NodeName: "byo-userA-rtx4090",
		Hostname: "h", AgentVersion: "0.2.0", TopologyJson: []byte(`{}`),
	}); err != nil {
		t.Fatalf("seed node: %v", err)
	}
	if _, err := q.TeamCreate(ctx, dbstore.TeamCreateParams{
		Name: "alpha", Description: "phase 2 partner alpha",
	}); err != nil {
		t.Fatalf("seed team: %v", err)
	}

	bin := buildAdminBinary(t)

	// 1. Create a token. Output must contain AGENT_API_TOKEN= and the
	//    mux endpoint string we passed.
	out, err := runCLI(t, bin, url, "mux.example.test:8443",
		"node-token", "create",
		"--node-name", "byo-userA-rtx4090",
		"--owner-team", "alpha")
	if err != nil {
		t.Fatalf("create: %v\n%s", err, out)
	}
	if !strings.Contains(out, "AGENT_API_TOKEN=") {
		t.Fatalf("missing AGENT_API_TOKEN in output:\n%s", out)
	}
	if !strings.Contains(out, "AGENT_MUX_ENDPOINT=mux.example.test:8443") {
		t.Fatalf("missing mux endpoint in output:\n%s", out)
	}

	plaintext := extractEnvValue(out, "AGENT_API_TOKEN")
	if plaintext == "" {
		t.Fatalf("could not parse plaintext token from output:\n%s", out)
	}

	// Sanity: node access_policy was pinned to owner_team.
	var policy string
	if err := pool.QueryRow(ctx,
		`select access_policy from nodes where node_name = $1`,
		"byo-userA-rtx4090",
	).Scan(&policy); err != nil {
		t.Fatalf("read policy: %v", err)
	}
	if policy != "owner_team" {
		t.Fatalf("access_policy: got %q, want owner_team", policy)
	}

	// 2. /internal/agent-auth must accept the token.
	authHandler := agentauth.NewHandler(agentauth.Config{
		Repo:     agentauth.NewPgRepo(q),
		CacheTTL: 0, // disable cache so the post-revoke step is deterministic
	})
	router := api.NewInternalRouter(api.SSHTicketDeps{}, authHandler, "internal-bearer")

	var nodeID uuid.UUID
	if err := pool.QueryRow(ctx,
		`select id from nodes where node_name = $1`, "byo-userA-rtx4090",
	).Scan(&nodeID); err != nil {
		t.Fatalf("read node id: %v", err)
	}

	rrPre := postAgentAuth(t, router, nodeID.String(), plaintext)
	if rrPre.Code != http.StatusOK {
		t.Fatalf("pre-revoke agent-auth: got %d, want 200; body=%s", rrPre.Code, rrPre.Body.String())
	}

	// 3. List shows it as active.
	out, err = runCLI(t, bin, url, "mux.example.test:8443",
		"node-token", "list", "--node-name", "byo-userA-rtx4090")
	if err != nil {
		t.Fatalf("list: %v\n%s", err, out)
	}
	if !strings.Contains(out, "state=active") {
		t.Fatalf("expected state=active in list output:\n%s", out)
	}

	tokenID := extractFirstFieldStartingWith(out, " ")
	if tokenID == "" {
		// Token ID is the first whitespace-separated field of the
		// list line. Parse defensively.
		fields := strings.Fields(out)
		if len(fields) > 0 {
			tokenID = fields[0]
		}
	}
	if tokenID == "" {
		t.Fatalf("could not extract token id from list output:\n%s", out)
	}

	// 4. Revoke.
	out, err = runCLI(t, bin, url, "mux.example.test:8443",
		"node-token", "revoke", "--token-id", tokenID)
	if err != nil {
		t.Fatalf("revoke: %v\n%s", err, out)
	}
	if !strings.Contains(out, "revoked "+tokenID) {
		t.Fatalf("expected confirmation in revoke output:\n%s", out)
	}

	// 5. /internal/agent-auth must reject after revoke (cache disabled
	//    so the rejection is immediate).
	rrPost := postAgentAuth(t, router, nodeID.String(), plaintext)
	if rrPost.Code != http.StatusUnauthorized {
		t.Fatalf("post-revoke agent-auth: got %d, want 401", rrPost.Code)
	}
}

func TestAdminCLI_TeamCreate_AddMember_ListMembers(t *testing.T) {
	t.Parallel()

	url := startPostgres(t)
	migrateUp(t, url)
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	bin := buildAdminBinary(t)

	// Create team.
	out, err := runCLI(t, bin, url, "", "team", "create", "--name", "beta-partners")
	if err != nil {
		t.Fatalf("team create: %v\n%s", err, out)
	}
	teamID := extractTeamID(out)
	if teamID == "" {
		t.Fatalf("could not parse team id:\n%s", out)
	}

	// Seed a user, then add via CLI.
	if _, err := pool.Exec(context.Background(),
		`insert into users (email, password_hash) values ($1, 'x')`,
		"user@example.test",
	); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	out, err = runCLI(t, bin, url, "",
		"team", "add-member",
		"--team-id", teamID,
		"--user-email", "user@example.test")
	if err != nil {
		t.Fatalf("add-member: %v\n%s", err, out)
	}
	if !strings.Contains(out, "added user user@example.test") {
		t.Fatalf("missing add confirmation:\n%s", out)
	}

	// Idempotent re-add.
	if _, err := runCLI(t, bin, url, "",
		"team", "add-member",
		"--team-id", teamID,
		"--user-email", "user@example.test"); err != nil {
		t.Fatalf("re-add must be idempotent: %v", err)
	}

	out, err = runCLI(t, bin, url, "",
		"team", "list-members", "--team-id", teamID)
	if err != nil {
		t.Fatalf("list-members: %v\n%s", err, out)
	}
	if !strings.Contains(out, "user@example.test") {
		t.Fatalf("user not listed:\n%s", out)
	}
}

func TestAdminCLI_RevokeNonExistentTokenSucceeds(t *testing.T) {
	t.Parallel()

	url := startPostgres(t)
	migrateUp(t, url)
	bin := buildAdminBinary(t)

	// NodeTokenRevoke is :exec — revoking a non-existent UUID is a
	// no-op rather than an error. Operators running revoke twice from
	// a runbook should not see a hard error the second time.
	out, err := runCLI(t, bin, url, "",
		"node-token", "revoke", "--token-id", uuid.New().String())
	if err != nil {
		t.Fatalf("revoke missing: %v\n%s", err, out)
	}
}

// --- helpers ---------------------------------------------------------

func postAgentAuth(t *testing.T, router http.Handler, nodeID, token string) *httptest.ResponseRecorder {
	t.Helper()
	body := []byte(`{"node_id":"` + nodeID + `","token":"` + token + `"}`)
	req := httptest.NewRequest(http.MethodPost, "/internal/agent-auth", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer internal-bearer")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	return rr
}

func extractEnvValue(out, key string) string {
	prefix := "  " + key + "="
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(line, prefix))
		}
	}
	return ""
}

func extractTeamID(out string) string {
	for _, line := range strings.Split(out, "\n") {
		idx := strings.Index(line, "id=")
		if idx >= 0 {
			rest := line[idx+3:]
			fields := strings.Fields(rest)
			if len(fields) > 0 {
				return fields[0]
			}
		}
	}
	return ""
}

func extractFirstFieldStartingWith(s, _ string) string {
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) >= 4 && strings.HasPrefix(fields[1], "state=") {
			return fields[0]
		}
	}
	return ""
}

// Sanity: this test file links the same time package the admin
// binary uses, so build tags don't drift.
var _ = time.Now
