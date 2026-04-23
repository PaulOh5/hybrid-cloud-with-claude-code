// Package node wraps dbstore with node-focused operations so handlers can be
// unit-tested against a small interface rather than the whole Querier.
package node

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"hybridcloud/services/main-api/internal/db/dbstore"
)

// Repo is the subset of node operations used by the gRPC and REST handlers.
type Repo interface {
	UpsertOnline(ctx context.Context, in UpsertInput) (dbstore.Node, error)
	TouchHeartbeat(ctx context.Context, id uuid.UUID) error
	UpdateTopology(ctx context.Context, id uuid.UUID, topologyJSON []byte) error
	MarkStaleOffline(ctx context.Context, before time.Time) (int64, error)
	List(ctx context.Context) ([]dbstore.Node, error)
	Get(ctx context.Context, id uuid.UUID) (dbstore.Node, error)
}

// UpsertInput is the subset of fields Register sets on a node.
type UpsertInput struct {
	ZoneID       uuid.UUID
	NodeName     string
	Hostname     string
	AgentVersion string
	TopologyJSON []byte
}

// Queries is the subset of dbstore methods we use, kept narrow so tests can
// substitute a fake without generating a full mock.
type Queries interface {
	UpsertNode(ctx context.Context, arg dbstore.UpsertNodeParams) (dbstore.Node, error)
	TouchNodeHeartbeat(ctx context.Context, id uuid.UUID) error
	MarkStaleNodesOffline(ctx context.Context, cutoff pgtype.Timestamptz) (int64, error)
	GetNode(ctx context.Context, id uuid.UUID) (dbstore.Node, error)
	ListNodes(ctx context.Context) ([]dbstore.Node, error)
	GetDefaultZone(ctx context.Context) (dbstore.Zone, error)
}

// DBRepo is the production Repo, backed by sqlc-generated queries.
type DBRepo struct {
	q Queries
}

// NewDBRepo constructs a DBRepo from any dbstore.Querier-compatible type.
func NewDBRepo(q Queries) *DBRepo {
	return &DBRepo{q: q}
}

// UpsertOnline creates or refreshes a node row and marks it online.
func (r *DBRepo) UpsertOnline(ctx context.Context, in UpsertInput) (dbstore.Node, error) {
	return r.q.UpsertNode(ctx, dbstore.UpsertNodeParams{
		ZoneID:       in.ZoneID,
		NodeName:     in.NodeName,
		Hostname:     in.Hostname,
		AgentVersion: in.AgentVersion,
		TopologyJson: in.TopologyJSON,
	})
}

// TouchHeartbeat bumps last_heartbeat_at and transitions offline→online.
func (r *DBRepo) TouchHeartbeat(ctx context.Context, id uuid.UUID) error {
	return r.q.TouchNodeHeartbeat(ctx, id)
}

// UpdateTopology is a placeholder — Phase 2 stores topology in the Upsert only;
// subsequent topology changes require a schema extension (Phase 4+).
func (r *DBRepo) UpdateTopology(_ context.Context, _ uuid.UUID, _ []byte) error {
	return nil
}

// MarkStaleOffline sweeps nodes whose last heartbeat is older than before.
func (r *DBRepo) MarkStaleOffline(ctx context.Context, before time.Time) (int64, error) {
	return r.q.MarkStaleNodesOffline(ctx, pgtype.Timestamptz{Time: before, Valid: true})
}

// List returns all known nodes.
func (r *DBRepo) List(ctx context.Context) ([]dbstore.Node, error) {
	return r.q.ListNodes(ctx)
}

// Get returns one node by id.
func (r *DBRepo) Get(ctx context.Context, id uuid.UUID) (dbstore.Node, error) {
	return r.q.GetNode(ctx, id)
}

// DefaultZoneID loads the seed zone id; agents register into it by default.
func (r *DBRepo) DefaultZoneID(ctx context.Context) (uuid.UUID, error) {
	z, err := r.q.GetDefaultZone(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	return z.ID, nil
}
