package sshkeys

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"

	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"
)

func generateTestKey(t *testing.T) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatalf("ssh pub: %v", err)
	}
	return string(ssh.MarshalAuthorizedKey(sshPub))
}

func TestFingerprintMatchesParsedKey(t *testing.T) {
	t.Parallel()
	raw := generateTestKey(t)
	parsed, _, _, _, err := ssh.ParseAuthorizedKey([]byte(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	fp := Fingerprint(parsed)
	if fp == "" {
		t.Fatal("empty fingerprint")
	}
	// Re-parsing a marshalled form should yield the same fingerprint.
	roundtrip, _, _, _, err := ssh.ParseAuthorizedKey(ssh.MarshalAuthorizedKey(parsed))
	if err != nil {
		t.Fatalf("roundtrip parse: %v", err)
	}
	if Fingerprint(roundtrip) != fp {
		t.Fatal("fingerprint not stable under re-marshal")
	}
}

func TestAdd_RejectsInvalidPubkey(t *testing.T) {
	t.Parallel()
	repo := NewRepo(nil) // queries unused on the validation path.
	_, err := repo.Add(t.Context(), uuid.Nil, "label", "not a key")
	if !errors.Is(err, ErrInvalidPubkey) {
		t.Fatalf("expected ErrInvalidPubkey, got %v", err)
	}
}

func TestAdd_RejectsEmptyLabel(t *testing.T) {
	t.Parallel()
	repo := NewRepo(nil)
	raw := generateTestKey(t)
	_, err := repo.Add(t.Context(), uuid.Nil, "  ", raw)
	if !errors.Is(err, ErrInvalidPubkey) {
		t.Fatalf("expected ErrInvalidPubkey, got %v", err)
	}
}
