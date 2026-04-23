//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"io/fs"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"hybridcloud/services/main-api/internal/db/dbstore"
	"hybridcloud/services/main-api/internal/db/migrations"
)

func startPostgres(t *testing.T) string {
	t.Helper()
	ctx := context.Background()

	container, err := tcpostgres.Run(ctx,
		"postgres:16-alpine",
		tcpostgres.WithDatabase("hybrid"),
		tcpostgres.WithUsername("hybrid"),
		tcpostgres.WithPassword("hybrid"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(30*time.Second),
		),
	)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(func() {
		if err := container.Terminate(context.Background()); err != nil {
			t.Logf("terminate container: %v", err)
		}
	})

	url, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	return url
}

func migrateUp(t *testing.T, url string) {
	t.Helper()

	dbh, err := sql.Open("pgx", url)
	if err != nil {
		t.Fatalf("open pgx: %v", err)
	}
	defer func() { _ = dbh.Close() }()

	sub, err := fs.Sub(migrations.FS(), ".")
	if err != nil {
		t.Fatalf("sub fs: %v", err)
	}

	goose.SetBaseFS(sub)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("set dialect: %v", err)
	}
	if err := goose.Up(dbh, "."); err != nil {
		t.Fatalf("goose up: %v", err)
	}
}

func pgTS(at time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: at, Valid: true}
}

func TestSchemaApply_UpDownIdempotent(t *testing.T) {
	t.Parallel()

	url := startPostgres(t)
	migrateUp(t, url)

	dbh, err := sql.Open("pgx", url)
	if err != nil {
		t.Fatalf("open pgx: %v", err)
	}
	defer func() { _ = dbh.Close() }()

	sub, err := fs.Sub(migrations.FS(), ".")
	if err != nil {
		t.Fatalf("sub fs: %v", err)
	}
	goose.SetBaseFS(sub)

	if err := goose.Down(dbh, "."); err != nil {
		t.Fatalf("goose down: %v", err)
	}
	if err := goose.Up(dbh, "."); err != nil {
		t.Fatalf("goose up again: %v", err)
	}
}

func TestDefaultZoneSeeded(t *testing.T) {
	t.Parallel()

	url := startPostgres(t)
	migrateUp(t, url)

	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("connect pool: %v", err)
	}
	defer pool.Close()

	q := dbstore.New(pool)
	zone, err := q.GetDefaultZone(context.Background())
	if err != nil {
		t.Fatalf("get default zone: %v", err)
	}
	if zone.Name == "" || !zone.IsDefault {
		t.Fatalf("unexpected zone: %+v", zone)
	}
}

func TestNodeUpsertIdempotent(t *testing.T) {
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

	first, err := q.UpsertNode(ctx, dbstore.UpsertNodeParams{
		ZoneID:       zone.ID,
		NodeName:     "node-1",
		Hostname:     "host.local",
		AgentVersion: "0.1.0",
		TopologyJson: []byte(`{"gpus":[]}`),
	})
	if err != nil {
		t.Fatalf("upsert 1: %v", err)
	}

	second, err := q.UpsertNode(ctx, dbstore.UpsertNodeParams{
		ZoneID:       zone.ID,
		NodeName:     "node-1",
		Hostname:     "host2.local",
		AgentVersion: "0.1.1",
		TopologyJson: []byte(`{"gpus":[{"i":0}]}`),
	})
	if err != nil {
		t.Fatalf("upsert 2: %v", err)
	}
	if first.ID != second.ID {
		t.Fatalf("upsert changed id: %s vs %s", first.ID, second.ID)
	}
	if second.Hostname != "host2.local" {
		t.Fatalf("upsert did not refresh hostname: %q", second.Hostname)
	}

	// Stale sweep: "cutoff in the future" matches every row.
	affected, err := q.MarkStaleNodesOffline(ctx, pgTS(time.Now().Add(time.Minute)))
	if err != nil {
		t.Fatalf("mark stale: %v", err)
	}
	if affected != 1 {
		t.Fatalf("affected rows: got %d, want 1", affected)
	}

	n, err := q.GetNode(ctx, second.ID)
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
	if n.Status != dbstore.NodeStatusOffline {
		t.Fatalf("expected offline, got %s", n.Status)
	}
}

func TestInstanceLifecycleAndEvents(t *testing.T) {
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
		NodeName:     "node-1",
		Hostname:     "host.local",
		AgentVersion: "0.1.0",
		TopologyJson: []byte(`{"gpus":[]}`),
	})
	if err != nil {
		t.Fatalf("upsert node: %v", err)
	}

	inst, err := q.CreateInstance(ctx, dbstore.CreateInstanceParams{
		OwnerID:     uuid.NullUUID{Valid: false},
		NodeID:      node.ID,
		Name:        "demo",
		MemoryMb:    2048,
		Vcpus:       2,
		GpuCount:    0,
		SlotIndices: []int32{},
		SshPubkeys:  []string{"ssh-ed25519 AAAA"},
		ImageRef:    "ubuntu-24.04",
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	if inst.State != dbstore.InstanceStatePending {
		t.Fatalf("expected pending, got %s", inst.State)
	}

	if err := q.InsertInstanceEvent(ctx, dbstore.InsertInstanceEventParams{
		InstanceID: inst.ID,
		FromState:  dbstore.NullInstanceState{InstanceState: dbstore.InstanceStatePending, Valid: true},
		ToState:    dbstore.InstanceStateProvisioning,
		Reason:     "start",
		Metadata:   []byte(`{}`),
	}); err != nil {
		t.Fatalf("insert event: %v", err)
	}

	events, err := q.ListInstanceEvents(ctx, inst.ID)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if got, want := len(events), 1; got != want {
		t.Fatalf("events: got %d, want %d", got, want)
	}
	if events[0].ToState != dbstore.InstanceStateProvisioning {
		t.Fatalf("unexpected to_state: %s", events[0].ToState)
	}
}

func TestCreditLedgerIsAppendOnly(t *testing.T) {
	t.Parallel()

	url := startPostgres(t)
	migrateUp(t, url)

	dbh, err := sql.Open("pgx", url)
	if err != nil {
		t.Fatalf("open pgx: %v", err)
	}
	defer func() { _ = dbh.Close() }()

	if _, err := dbh.Exec(`insert into users (id, email, password_hash)
		values ('11111111-1111-1111-1111-111111111111', 'a@b', 'x')`); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := dbh.Exec(`insert into credit_ledger (user_id, delta_milli, reason, idempotency_key)
		values ('11111111-1111-1111-1111-111111111111', 1000, 'topup', 'k1')`); err != nil {
		t.Fatalf("seed ledger: %v", err)
	}

	if _, err := dbh.Exec(`update credit_ledger set delta_milli = 9999`); err == nil {
		t.Fatal("expected update to be rejected by trigger")
	}
	if _, err := dbh.Exec(`delete from credit_ledger`); err == nil {
		t.Fatal("expected delete to be rejected by trigger")
	}
}
