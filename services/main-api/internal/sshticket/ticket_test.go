package sshticket_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"hybridcloud/services/main-api/internal/sshticket"
)

var testSecret = []byte("0123456789abcdef0123456789abcdef")

func newPair(t *testing.T, ttl time.Duration) (*sshticket.Signer, *sshticket.Verifier) {
	t.Helper()
	s, err := sshticket.NewSigner(testSecret, ttl)
	if err != nil {
		t.Fatal(err)
	}
	return s, sshticket.NewVerifier(testSecret)
}

func TestIssueAndVerify_HappyPath(t *testing.T) {
	t.Parallel()

	signer, verifier := newPair(t, 15*time.Second)

	signed, err := signer.Issue(sshticket.Ticket{
		InstanceID:     uuid.New(),
		NodeID:         uuid.New(),
		VMInternalIP:   "192.168.122.42",
		VMPort:         22,
		TunnelEndpoint: "127.0.0.1:8082",
	})
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if signed.Payload == "" || signed.Signature == "" {
		t.Fatal("empty fields")
	}

	ticket, err := verifier.Verify(signed)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if ticket.SessionID == "" {
		t.Fatal("session id auto-assignment missing")
	}
	if ticket.VMInternalIP != "192.168.122.42" || ticket.VMPort != 22 {
		t.Fatalf("fields lost: %+v", ticket)
	}
}

func TestVerify_RejectsTamperedSignature(t *testing.T) {
	t.Parallel()

	signer, verifier := newPair(t, 15*time.Second)
	signed, _ := signer.Issue(sshticket.Ticket{VMInternalIP: "1.1.1.1", VMPort: 22})

	signed.Signature = "AAAA" + signed.Signature[4:]

	_, err := verifier.Verify(signed)
	if !errors.Is(err, sshticket.ErrInvalidSignature) {
		t.Fatalf("expected ErrInvalidSignature, got %v", err)
	}
}

func TestVerify_RejectsTamperedPayload(t *testing.T) {
	t.Parallel()

	signer, verifier := newPair(t, 15*time.Second)
	signed, _ := signer.Issue(sshticket.Ticket{VMInternalIP: "1.1.1.1", VMPort: 22})

	// Change the payload without updating the signature.
	signed.Payload = strings.ToLower(signed.Payload)

	_, err := verifier.Verify(signed)
	if !errors.Is(err, sshticket.ErrInvalidSignature) {
		t.Fatalf("expected ErrInvalidSignature, got %v", err)
	}
}

func TestVerify_RejectsExpired(t *testing.T) {
	t.Parallel()

	signer, _ := newPair(t, 50*time.Millisecond)
	past := time.Now().Add(-time.Hour)
	signer.Now = func() time.Time { return past }

	signed, _ := signer.Issue(sshticket.Ticket{VMInternalIP: "1.1.1.1", VMPort: 22})

	verifier := sshticket.NewVerifier(testSecret)
	verifier.Now = func() time.Time { return past.Add(time.Hour + time.Minute) }

	_, err := verifier.Verify(signed)
	if !errors.Is(err, sshticket.ErrExpired) {
		t.Fatalf("expected ErrExpired, got %v", err)
	}
}

func TestNewSigner_RejectsShortSecret(t *testing.T) {
	t.Parallel()

	_, err := sshticket.NewSigner([]byte("tiny"), time.Second)
	if err == nil {
		t.Fatal("short secret must be rejected")
	}
}

func TestNewSigner_RejectsBadTTL(t *testing.T) {
	t.Parallel()

	_, err := sshticket.NewSigner(testSecret, 0)
	if err == nil {
		t.Fatal("zero TTL must be rejected")
	}
}
