// Package credit owns the user balance + ledger. The ledger is append-only
// (DB trigger blocks UPDATE/DELETE on credit_ledger); a per-user `credits`
// row caches the running sum so balance reads are a single PK lookup.
package credit

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"hybridcloud/services/main-api/internal/db/dbstore"
)

// Errors surfaced to the HTTP layer.
var (
	// ErrDuplicateIdempotency means a ledger entry with the same key already
	// exists. Caller should return 409 (the recharge already applied).
	ErrDuplicateIdempotency = errors.New("credit: duplicate idempotency key")
	// ErrInsufficientBalance is for the 9.3 create-time gate.
	ErrInsufficientBalance = errors.New("credit: insufficient balance")
)

// TxBeginner is the slice of pgxpool.Pool we need.
type TxBeginner interface {
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
}

// Repo bundles ledger + cache writes behind a transactional API.
type Repo struct {
	beg     TxBeginner
	queries *dbstore.Queries
}

// NewRepo wires a Repo around pgx + dbstore queries.
func NewRepo(beg TxBeginner, queries *dbstore.Queries) *Repo {
	return &Repo{beg: beg, queries: queries}
}

// PostInput is one ledger row to add.
type PostInput struct {
	UserID         uuid.UUID
	DeltaMilli     int64
	Reason         string
	InstanceID     uuid.NullUUID
	IdempotencyKey string
	Metadata       []byte
}

// Post writes a ledger entry and updates the cached balance in one tx. The
// idempotency_key unique constraint deduplicates: a duplicate insert returns
// ErrDuplicateIdempotency without touching the balance.
func (r *Repo) Post(ctx context.Context, in PostInput) (dbstore.CreditLedger, error) {
	if in.IdempotencyKey == "" {
		return dbstore.CreditLedger{}, errors.New("credit: idempotency_key required")
	}
	if len(in.Metadata) == 0 {
		in.Metadata = []byte("{}")
	}

	var entry dbstore.CreditLedger
	err := r.inTx(ctx, func(q *dbstore.Queries) error {
		row, err := q.InsertCreditLedgerEntry(ctx, dbstore.InsertCreditLedgerEntryParams{
			UserID:         in.UserID,
			DeltaMilli:     in.DeltaMilli,
			Reason:         in.Reason,
			InstanceID:     in.InstanceID,
			IdempotencyKey: in.IdempotencyKey,
			Metadata:       in.Metadata,
		})
		if err != nil {
			if isUniqueViolation(err) {
				return ErrDuplicateIdempotency
			}
			return fmt.Errorf("insert ledger: %w", err)
		}
		if err := q.UpsertCredits(ctx, dbstore.UpsertCreditsParams{
			UserID:       in.UserID,
			BalanceMilli: in.DeltaMilli,
		}); err != nil {
			return fmt.Errorf("update balance: %w", err)
		}
		entry = row
		return nil
	})
	return entry, err
}

// Balance returns the cached balance. Returns 0 (not an error) when the user
// has no credits row yet — they simply haven't been touched by the ledger.
func (r *Repo) Balance(ctx context.Context, userID uuid.UUID) (int64, error) {
	c, err := r.queries.GetCredits(ctx, userID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("get credits: %w", err)
	}
	return c.BalanceMilli, nil
}

// History returns the most recent ledger entries for one user.
func (r *Repo) History(ctx context.Context, userID uuid.UUID, limit int32) ([]dbstore.CreditLedger, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	return r.queries.ListCreditLedgerEntries(ctx, dbstore.ListCreditLedgerEntriesParams{
		UserID: userID,
		Limit:  limit,
	})
}

// UsersWithNegativeBalance returns user IDs whose cached balance is ≤ 0. The
// 9.3 sweep dispatches DestroyInstance for each.
func (r *Repo) UsersWithNegativeBalance(ctx context.Context) ([]uuid.UUID, error) {
	return r.queries.ListUsersWithNegativeBalance(ctx)
}

func (r *Repo) inTx(ctx context.Context, fn func(q *dbstore.Queries) error) error {
	tx, err := r.beg.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(r.queries.WithTx(tx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func isUniqueViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	if errors.As(err, &pgErr) {
		return pgErr.SQLState() == "23505"
	}
	return false
}
