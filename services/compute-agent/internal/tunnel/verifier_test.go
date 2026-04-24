package tunnel_test

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"hybridcloud/services/compute-agent/internal/tunnel"
)

var hmacSecret = bytes.Repeat([]byte{7}, 32)

func signTicket(t *testing.T, ticket tunnel.Ticket) tunnel.SignedTicket {
	t.Helper()
	raw, err := json.Marshal(ticket)
	if err != nil {
		t.Fatal(err)
	}
	payload := base64.StdEncoding.EncodeToString(raw)
	mac := hmac.New(sha256.New, hmacSecret)
	mac.Write([]byte(payload))
	return tunnel.SignedTicket{
		Payload:   payload,
		Signature: base64.StdEncoding.EncodeToString(mac.Sum(nil)),
	}
}

func TestHMACVerifier_HappyPath(t *testing.T) {
	t.Parallel()

	v, err := tunnel.NewHMACVerifier(hmacSecret)
	if err != nil {
		t.Fatal(err)
	}
	signed := signTicket(t, tunnel.Ticket{
		VMInternalIP: "192.168.122.47",
		VMPort:       22,
		ExpiresAt:    time.Now().Add(time.Minute),
	})

	got, err := v.Verify(signed)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got.VMInternalIP != "192.168.122.47" || got.VMPort != 22 {
		t.Fatalf("parsed: %+v", got)
	}
}

func TestHMACVerifier_RejectsBadSignature(t *testing.T) {
	t.Parallel()

	v, _ := tunnel.NewHMACVerifier(hmacSecret)
	signed := signTicket(t, tunnel.Ticket{ExpiresAt: time.Now().Add(time.Minute)})
	signed.Signature = "AAAA" + signed.Signature[4:]

	_, err := v.Verify(signed)
	if !errors.Is(err, tunnel.ErrInvalidSignature) {
		t.Fatalf("expected ErrInvalidSignature, got %v", err)
	}
}

func TestHMACVerifier_RejectsExpired(t *testing.T) {
	t.Parallel()

	v, _ := tunnel.NewHMACVerifier(hmacSecret)
	v.Now = func() time.Time { return time.Now().Add(time.Hour) }

	signed := signTicket(t, tunnel.Ticket{ExpiresAt: time.Now().Add(time.Minute)})
	_, err := v.Verify(signed)
	if !errors.Is(err, tunnel.ErrExpired) {
		t.Fatalf("expected ErrExpired, got %v", err)
	}
}

func TestNewHMACVerifier_RejectsShortSecret(t *testing.T) {
	t.Parallel()
	if _, err := tunnel.NewHMACVerifier([]byte("tiny")); err == nil {
		t.Fatal("short secret should fail")
	}
}
