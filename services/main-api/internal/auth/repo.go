package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"hybridcloud/services/main-api/internal/db/dbstore"
)

// ErrUserNotFound + ErrSessionNotFound let the HTTP layer decide whether to
// return 404 or fold them into ErrInvalidCredentials.
var (
	ErrUserNotFound    = errors.New("auth: user not found")
	ErrSessionNotFound = errors.New("auth: session not found")
	ErrEmailTaken      = errors.New("auth: email already registered")
)

// Repo bundles user + session persistence on top of dbstore.
type Repo struct {
	queries *dbstore.Queries
}

// NewRepo wraps a *dbstore.Queries.
func NewRepo(q *dbstore.Queries) *Repo { return &Repo{queries: q} }

// CreateUser hashes the password and inserts the row. is_admin is always
// false here; the admin flag is flipped manually via SQL.
func (r *Repo) CreateUser(ctx context.Context, email, password string) (dbstore.User, error) {
	email = NormalizeEmail(email)
	if err := ValidateEmail(email); err != nil {
		return dbstore.User{}, err
	}
	hash, err := HashPassword(password)
	if err != nil {
		return dbstore.User{}, err
	}
	u, err := r.queries.CreateUser(ctx, dbstore.CreateUserParams{
		Email:        email,
		PasswordHash: hash,
		IsAdmin:      false,
	})
	if err != nil {
		// Postgres unique-violation code; we surface a typed error so the
		// handler returns 409 instead of 500.
		if isUniqueViolation(err) {
			return dbstore.User{}, ErrEmailTaken
		}
		return dbstore.User{}, fmt.Errorf("create user: %w", err)
	}
	return u, nil
}

// Authenticate looks up the user by email and verifies the password. Both
// "user not found" and "wrong password" map to ErrInvalidCredentials so an
// attacker cannot enumerate valid emails.
func (r *Repo) Authenticate(ctx context.Context, email, password string) (dbstore.User, error) {
	email = NormalizeEmail(email)
	u, err := r.queries.GetUserByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return dbstore.User{}, ErrInvalidCredentials
		}
		return dbstore.User{}, fmt.Errorf("get user: %w", err)
	}
	if err := ComparePassword(u.PasswordHash, password); err != nil {
		return dbstore.User{}, ErrInvalidCredentials
	}
	return u, nil
}

// GetUser fetches a user row.
func (r *Repo) GetUser(ctx context.Context, id uuid.UUID) (dbstore.User, error) {
	u, err := r.queries.GetUser(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return dbstore.User{}, ErrUserNotFound
		}
		return dbstore.User{}, fmt.Errorf("get user: %w", err)
	}
	return u, nil
}

// CreateSession generates a fresh token, persists its hash, and returns the
// raw token to send to the client. ttl is added to time.Now to compute
// expires_at.
func (r *Repo) CreateSession(ctx context.Context, userID uuid.UUID, ttl time.Duration) (rawToken string, sess dbstore.Session, err error) {
	if ttl <= 0 {
		return "", dbstore.Session{}, errors.New("auth: ttl must be positive")
	}
	raw, hash, err := GenerateSessionToken()
	if err != nil {
		return "", dbstore.Session{}, err
	}
	expires := time.Now().Add(ttl).UTC()
	sess, err = r.queries.CreateSession(ctx, dbstore.CreateSessionParams{
		UserID:    userID,
		TokenHash: hash,
		ExpiresAt: pgtype.Timestamptz{Time: expires, Valid: true},
	})
	if err != nil {
		return "", dbstore.Session{}, fmt.Errorf("create session: %w", err)
	}
	return raw, sess, nil
}

// LookupSession resolves a raw token to (session, user). Returns
// ErrSessionNotFound if the row is missing or expired.
func (r *Repo) LookupSession(ctx context.Context, rawToken string) (dbstore.Session, dbstore.User, error) {
	if rawToken == "" {
		return dbstore.Session{}, dbstore.User{}, ErrSessionNotFound
	}
	hash := HashSessionToken(rawToken)
	sess, err := r.queries.GetSessionByTokenHash(ctx, hash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return dbstore.Session{}, dbstore.User{}, ErrSessionNotFound
		}
		return dbstore.Session{}, dbstore.User{}, fmt.Errorf("get session: %w", err)
	}
	user, err := r.queries.GetUser(ctx, sess.UserID)
	if err != nil {
		return dbstore.Session{}, dbstore.User{}, fmt.Errorf("get user: %w", err)
	}
	return sess, user, nil
}

// RevokeSession deletes the session row matching the raw token. Idempotent.
func (r *Repo) RevokeSession(ctx context.Context, rawToken string) error {
	if rawToken == "" {
		return nil
	}
	return r.queries.DeleteSessionByTokenHash(ctx, HashSessionToken(rawToken))
}

// SweepExpired removes expired sessions; called periodically by the main loop.
func (r *Repo) SweepExpired(ctx context.Context) (int64, error) {
	return r.queries.DeleteExpiredSessions(ctx)
}

// isUniqueViolation matches Postgres SQLSTATE 23505 without pulling the
// pgconn package into the handler layer.
func isUniqueViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	if errors.As(err, &pgErr) {
		return pgErr.SQLState() == "23505"
	}
	return false
}
