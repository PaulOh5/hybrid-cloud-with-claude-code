package tunnel

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"
)

// ErrInvalidSignature mirrors main-api's error so agent logs read the same.
var ErrInvalidSignature = errors.New("tunnel: invalid signature")

// ErrExpired is returned when the ticket is past its ExpiresAt.
var ErrExpired = errors.New("tunnel: expired")

// HMACVerifier reproduces main-api's signing scheme so agents can verify
// tickets locally without an API round-trip.
type HMACVerifier struct {
	Secret []byte
	Now    func() time.Time
}

// NewHMACVerifier validates the secret length and returns a Verifier.
func NewHMACVerifier(secret []byte) (*HMACVerifier, error) {
	if len(secret) < 16 {
		return nil, errors.New("tunnel: secret must be at least 16 bytes")
	}
	return &HMACVerifier{Secret: secret, Now: time.Now}, nil
}

// Verify satisfies tunnel.Verifier.
func (v *HMACVerifier) Verify(signed SignedTicket) (Ticket, error) {
	mac := hmac.New(sha256.New, v.Secret)
	mac.Write([]byte(signed.Payload))
	expected := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(expected), []byte(signed.Signature)) {
		return Ticket{}, ErrInvalidSignature
	}
	raw, err := base64.StdEncoding.DecodeString(signed.Payload)
	if err != nil {
		return Ticket{}, err
	}
	var t Ticket
	if err := json.Unmarshal(raw, &t); err != nil {
		return Ticket{}, err
	}
	now := time.Now()
	if v.Now != nil {
		now = v.Now()
	}
	if now.After(t.ExpiresAt) {
		return Ticket{}, ErrExpired
	}
	return t, nil
}
