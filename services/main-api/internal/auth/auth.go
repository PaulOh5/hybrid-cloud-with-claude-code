// Package auth holds the password hashing, opaque session-token generation,
// and email-format helpers used by main-api's user-facing auth endpoints.
//
// Passwords use bcrypt (cost 12, ~150 ms / hash on modern hardware), session
// tokens are 32-byte cryptographically-random strings stored as SHA-256 hashes
// so a stolen sessions row cannot replay a session.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

// MinPasswordLength is the minimum acceptable password length per the Phase 7
// spec ("패스워드 정책: 최소 10자").
const MinPasswordLength = 10

// BcryptCost is the cost factor used for password hashes. 12 is a sensible
// 2026 default; raise once the request-rate budget is known.
const BcryptCost = 12

// SessionTokenBytes is the entropy length for generated session tokens. 32
// bytes (~43 base64 chars) makes brute force infeasible.
const SessionTokenBytes = 32

// HashPassword returns the bcrypt hash of the plain password. Validates the
// length first so the caller does not accidentally hash an empty string.
func HashPassword(password string) (string, error) {
	if err := ValidatePassword(password); err != nil {
		return "", err
	}
	out, err := bcrypt.GenerateFromPassword([]byte(password), BcryptCost)
	if err != nil {
		return "", fmt.Errorf("bcrypt: %w", err)
	}
	return string(out), nil
}

// ComparePassword returns nil when password matches the stored bcrypt hash,
// ErrInvalidCredentials otherwise. Treats malformed hashes as a credential
// mismatch (rather than a 500) so an attacker cannot probe for them.
func ComparePassword(hash, password string) error {
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)); err != nil {
		return ErrInvalidCredentials
	}
	return nil
}

// ValidatePassword enforces the Phase 7 password policy.
func ValidatePassword(password string) error {
	if len(password) < MinPasswordLength {
		return fmt.Errorf("%w: minimum %d characters", ErrWeakPassword, MinPasswordLength)
	}
	// bcrypt silently truncates inputs at 72 bytes — reject anything past
	// that so two long-but-distinct passwords cannot collide.
	if len(password) > 72 {
		return fmt.Errorf("%w: maximum 72 characters", ErrWeakPassword)
	}
	return nil
}

// NormalizeEmail trims whitespace and lowercases the address so case-only
// duplicates collapse to the same row.
func NormalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// ValidateEmail does a minimal sanity check — full RFC compliance is not
// worth the maintenance burden at this layer; downstream services rely on the
// unique index for collisions.
func ValidateEmail(email string) error {
	if email == "" {
		return ErrInvalidEmail
	}
	at := strings.Index(email, "@")
	if at < 1 || at == len(email)-1 {
		return ErrInvalidEmail
	}
	if strings.Contains(email, " ") {
		return ErrInvalidEmail
	}
	return nil
}

// GenerateSessionToken returns (rawToken, sha256Hex(rawToken)). Callers send
// rawToken to the client and persist the hash so a leaked sessions table
// cannot be replayed against this main-api.
func GenerateSessionToken() (string, string, error) {
	buf := make([]byte, SessionTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("rand: %w", err)
	}
	raw := base64.RawURLEncoding.EncodeToString(buf)
	return raw, HashSessionToken(raw), nil
}

// HashSessionToken returns the lowercase hex SHA-256 of token. Used both at
// session creation (to insert) and at lookup (to compare). A non-cryptographic
// digest would be enough since the entropy lives in the raw token, but
// SHA-256 keeps the table cheap to scan and avoids future "is this digest
// adequate" questions.
func HashSessionToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

var (
	// ErrWeakPassword is returned when the password violates the policy.
	ErrWeakPassword = errors.New("auth: password does not meet policy")
	// ErrInvalidEmail is returned for malformed addresses.
	ErrInvalidEmail = errors.New("auth: invalid email")
	// ErrInvalidCredentials is the single error surfaced for "bad email" or
	// "bad password" so callers cannot enumerate registered emails.
	ErrInvalidCredentials = errors.New("auth: invalid credentials")
)
