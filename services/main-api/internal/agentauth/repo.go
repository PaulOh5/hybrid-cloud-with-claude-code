package agentauth

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"hybridcloud/services/main-api/internal/db/dbstore"
)

// PgRepo wraps dbstore.Queries to implement the agentauth.Repo interface.
// Production wiring instantiates this with the same *pgxpool.Pool used by
// the rest of main-api.
type PgRepo struct {
	q *dbstore.Queries
}

// NewPgRepo constructs the production Repo from a sqlc-generated Queries
// handle. The handle is expected to be backed by *pgxpool.Pool so the call
// path is goroutine-safe.
func NewPgRepo(q *dbstore.Queries) *PgRepo { return &PgRepo{q: q} }

// ListActiveNodeTokens returns the active node_tokens rows for the node.
// Filtering by revoked_at IS NULL is done in SQL (queries/node_tokens.sql)
// so a freshly revoked row never reaches the bcrypt loop above.
func (r *PgRepo) ListActiveNodeTokens(ctx context.Context, nodeID uuid.UUID) ([]dbstore.NodeToken, error) {
	rows, err := r.q.ListActiveNodeTokens(ctx, nodeID)
	if err != nil {
		return nil, fmt.Errorf("list active node tokens: %w", err)
	}
	return rows, nil
}

// NodeAuthView projects nodes -> NodeView. Maps pgx.ErrNoRows to
// ErrNodeNotFound so the handler can map that to 401 (not 500).
func (r *PgRepo) NodeAuthView(ctx context.Context, nodeID uuid.UUID) (NodeView, error) {
	row, err := r.q.NodeAccessPolicy(ctx, nodeID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return NodeView{}, ErrNodeNotFound
		}
		return NodeView{}, fmt.Errorf("node access policy: %w", err)
	}
	// agent_version is tracked on the nodes row by the gRPC stream's
	// UpsertNode + heartbeat; read it through a second tiny query so the
	// NodeAccessPolicy projection stays narrow.
	full, err := r.q.GetNode(ctx, nodeID)
	if err != nil {
		return NodeView{}, fmt.Errorf("get node: %w", err)
	}
	return NodeView{
		NodeID:       nodeID,
		AccessPolicy: row.AccessPolicy,
		OwnerTeamID:  row.OwnerTeamID,
		AgentVersion: full.AgentVersion,
	}, nil
}
