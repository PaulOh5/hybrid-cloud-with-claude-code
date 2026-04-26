// Package sshkeys validates and persists user-provided OpenSSH public keys.
// Validation goes through golang.org/x/crypto/ssh.ParseAuthorizedKey so we
// reject malformed input before it lands in cloud-init seed data.
package sshkeys

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/ssh"

	"hybridcloud/services/main-api/internal/db/dbstore"
)

// MaxLabelLen is a soft cap so the UI can render labels in tables.
const MaxLabelLen = 64

// MaxPubkeyLen is enough room for ed25519/rsa/ecdsa keys with a comment.
const MaxPubkeyLen = 4096

var (
	// ErrInvalidPubkey wraps any failure to parse the OpenSSH key body.
	ErrInvalidPubkey = errors.New("sshkeys: invalid public key")
	// ErrDuplicate is returned when the user already has a key with the same
	// fingerprint.
	ErrDuplicate = errors.New("sshkeys: duplicate key")
	// ErrNotFound covers GET/DELETE with a stranger's id.
	ErrNotFound = errors.New("sshkeys: not found")
)

// Repo persists per-user SSH pubkeys.
type Repo struct {
	q *dbstore.Queries
}

// NewRepo wires a Repo around dbstore.Queries.
func NewRepo(q *dbstore.Queries) *Repo { return &Repo{q: q} }

// Add validates and persists a new key. The label is trimmed; the pubkey is
// canonicalised to its `type base64-body comment` form so duplicate detection
// is robust to whitespace.
func (r *Repo) Add(ctx context.Context, userID uuid.UUID, label, pubkey string) (dbstore.SshKey, error) {
	label = strings.TrimSpace(label)
	if label == "" {
		return dbstore.SshKey{}, fmt.Errorf("%w: label required", ErrInvalidPubkey)
	}
	if len(label) > MaxLabelLen {
		return dbstore.SshKey{}, fmt.Errorf("%w: label too long", ErrInvalidPubkey)
	}
	if len(pubkey) > MaxPubkeyLen {
		return dbstore.SshKey{}, fmt.Errorf("%w: pubkey too long", ErrInvalidPubkey)
	}
	parsed, _, _, _, err := ssh.ParseAuthorizedKey([]byte(pubkey))
	if err != nil {
		return dbstore.SshKey{}, fmt.Errorf("%w: %v", ErrInvalidPubkey, err)
	}
	canonical := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(parsed)))
	fp := Fingerprint(parsed)

	row, err := r.q.CreateSSHKey(ctx, dbstore.CreateSSHKeyParams{
		UserID:      userID,
		Label:       label,
		Pubkey:      canonical,
		Fingerprint: fp,
	})
	if err != nil {
		var pgErr interface{ SQLState() string }
		if errors.As(err, &pgErr) && pgErr.SQLState() == "23505" {
			return dbstore.SshKey{}, ErrDuplicate
		}
		return dbstore.SshKey{}, fmt.Errorf("create ssh_key: %w", err)
	}
	return row, nil
}

// List returns the user's keys ordered newest-first.
func (r *Repo) List(ctx context.Context, userID uuid.UUID) ([]dbstore.SshKey, error) {
	return r.q.ListSSHKeysForUser(ctx, userID)
}

// Delete removes a key only when it belongs to the caller. Returns
// ErrNotFound if no row matches the (id, user_id) tuple.
func (r *Repo) Delete(ctx context.Context, userID, id uuid.UUID) error {
	n, err := r.q.DeleteSSHKeyForUser(ctx, dbstore.DeleteSSHKeyForUserParams{
		ID:     id,
		UserID: userID,
	})
	if err != nil {
		return fmt.Errorf("delete ssh_key: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// LookupUserByFingerprint resolves the SHA-256 fingerprint presented by an
// SSH client to its owning user. Returns ErrNotFound when no key matches.
// ssh-proxy uses this to authenticate the user before issuing a tunnel
// ticket.
func (r *Repo) LookupUserByFingerprint(ctx context.Context, fingerprint string) (uuid.UUID, error) {
	row, err := r.q.LookupSSHKeyByFingerprint(ctx, fingerprint)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, ErrNotFound
		}
		return uuid.Nil, fmt.Errorf("lookup ssh_key fingerprint: %w", err)
	}
	return row.UserID, nil
}

// PubkeysForUser returns the canonical OpenSSH lines for cloud-init.
func (r *Repo) PubkeysForUser(ctx context.Context, userID uuid.UUID) ([]string, error) {
	rows, err := r.q.ListSSHKeysForUser(ctx, userID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Pubkey)
	}
	return out, nil
}

// Fingerprint returns the SHA-256 hash of the raw key body, base64-encoded
// without padding (matching `ssh-keygen -lf` SHA256 output for the same key).
func Fingerprint(key ssh.PublicKey) string {
	sum := sha256.Sum256(key.Marshal())
	return base64.RawStdEncoding.EncodeToString(sum[:])
}
