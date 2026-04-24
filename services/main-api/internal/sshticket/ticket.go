// Package sshticket issues and verifies the short-lived HMAC tickets that
// authenticate a proxy → agent byte-tunnel. Phase 6 design: main-api signs a
// ticket describing the session (target node, VM IP:port, expiry), the proxy
// presents it to the agent's local tunnel listener, and the agent verifies
// before opening a TCP connection to the VM.
package sshticket

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Ticket is the signed payload.
type Ticket struct {
	SessionID      string    `json:"session_id"`
	InstanceID     uuid.UUID `json:"instance_id"`
	NodeID         uuid.UUID `json:"node_id"`
	VMInternalIP   string    `json:"vm_internal_ip"`
	VMPort         uint16    `json:"vm_port"`
	TunnelEndpoint string    `json:"tunnel_endpoint"` // host:port the proxy dials
	ExpiresAt      time.Time `json:"expires_at"`
}

// Signed wraps a ticket with its signature. JSON bytes are deterministic
// (no map fields, fixed order via struct tags) so the proxy re-signs and
// compares.
type Signed struct {
	Payload   string `json:"payload"`   // base64(JSON(Ticket))
	Signature string `json:"signature"` // base64(HMAC-SHA256(secret, payload bytes))
}

// Signer issues fresh tickets.
type Signer struct {
	Secret []byte
	TTL    time.Duration
	Now    func() time.Time
}

// NewSigner validates inputs and returns a Signer ready for Issue calls.
func NewSigner(secret []byte, ttl time.Duration) (*Signer, error) {
	if len(secret) < 16 {
		return nil, errors.New("sshticket: secret must be at least 16 bytes")
	}
	if ttl <= 0 {
		return nil, errors.New("sshticket: TTL must be positive")
	}
	return &Signer{Secret: secret, TTL: ttl, Now: time.Now}, nil
}

// Issue builds, serialises, and signs a ticket. The returned Signed struct
// is safe to JSON-encode and send to ssh-proxy.
func (s *Signer) Issue(t Ticket) (Signed, error) {
	if t.SessionID == "" {
		t.SessionID = uuid.NewString()
	}
	if t.ExpiresAt.IsZero() {
		t.ExpiresAt = s.Now().Add(s.TTL).UTC()
	}

	raw, err := json.Marshal(t)
	if err != nil {
		return Signed{}, fmt.Errorf("marshal ticket: %w", err)
	}
	payload := base64.StdEncoding.EncodeToString(raw)

	mac := hmac.New(sha256.New, s.Secret)
	mac.Write([]byte(payload))
	sig := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	return Signed{Payload: payload, Signature: sig}, nil
}

// ErrInvalidSignature is returned when the signature does not match the
// payload.
var ErrInvalidSignature = errors.New("sshticket: invalid signature")

// ErrExpired is returned when a ticket's expires_at is in the past. This is
// distinct from ErrInvalidSignature so operators see a clear TTL-miss vs
// tamper-attempt signal.
var ErrExpired = errors.New("sshticket: expired")

// Verifier validates tickets on the agent side.
type Verifier struct {
	Secret []byte
	Now    func() time.Time
}

// NewVerifier wraps a secret.
func NewVerifier(secret []byte) *Verifier {
	return &Verifier{Secret: secret, Now: time.Now}
}

// Verify returns the parsed Ticket when the signature matches and it has not
// yet expired.
func (v *Verifier) Verify(signed Signed) (Ticket, error) {
	mac := hmac.New(sha256.New, v.Secret)
	mac.Write([]byte(signed.Payload))
	expected := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(expected), []byte(signed.Signature)) {
		return Ticket{}, ErrInvalidSignature
	}

	raw, err := base64.StdEncoding.DecodeString(signed.Payload)
	if err != nil {
		return Ticket{}, fmt.Errorf("decode payload: %w", err)
	}
	var t Ticket
	if err := json.Unmarshal(raw, &t); err != nil {
		return Ticket{}, fmt.Errorf("parse ticket: %w", err)
	}
	now := v.now()
	if now.After(t.ExpiresAt) {
		return Ticket{}, ErrExpired
	}
	return t, nil
}

func (v *Verifier) now() time.Time {
	if v.Now != nil {
		return v.Now()
	}
	return time.Now()
}
