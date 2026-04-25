package auth

import (
	"errors"
	"strings"
	"testing"
)

func TestHashPassword_RejectsShort(t *testing.T) {
	t.Parallel()
	_, err := HashPassword("short")
	if !errors.Is(err, ErrWeakPassword) {
		t.Fatalf("expected ErrWeakPassword, got %v", err)
	}
}

func TestHashPassword_RoundTrips(t *testing.T) {
	t.Parallel()
	hash, err := HashPassword("correct-horse-battery-staple")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if hash == "correct-horse-battery-staple" {
		t.Fatal("hash equals plaintext")
	}
	if err := ComparePassword(hash, "correct-horse-battery-staple"); err != nil {
		t.Fatalf("compare: %v", err)
	}
	if err := ComparePassword(hash, "wrong-horse-battery-staple"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("expected ErrInvalidCredentials, got %v", err)
	}
}

func TestHashPassword_RejectsTooLong(t *testing.T) {
	t.Parallel()
	if _, err := HashPassword(strings.Repeat("a", 73)); !errors.Is(err, ErrWeakPassword) {
		t.Fatalf("expected ErrWeakPassword for 73-char input, got %v", err)
	}
}

func TestValidateEmail(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in    string
		valid bool
	}{
		{"u@example.com", true},
		{"u+tag@example.com", true},
		{"", false},
		{"foo", false},
		{"@example.com", false},
		{"u@", false},
		{"a b@example.com", false},
	}
	for _, tc := range cases {
		err := ValidateEmail(tc.in)
		if tc.valid && err != nil {
			t.Errorf("%q: unexpected error %v", tc.in, err)
		}
		if !tc.valid && err == nil {
			t.Errorf("%q: expected error", tc.in)
		}
	}
}

func TestNormalizeEmail(t *testing.T) {
	t.Parallel()
	if got := NormalizeEmail("  USER@Example.COM "); got != "user@example.com" {
		t.Fatalf("normalize: got %q", got)
	}
}

func TestGenerateSessionToken_Distinct(t *testing.T) {
	t.Parallel()
	a, ah, err := GenerateSessionToken()
	if err != nil {
		t.Fatalf("gen a: %v", err)
	}
	b, bh, err := GenerateSessionToken()
	if err != nil {
		t.Fatalf("gen b: %v", err)
	}
	if a == b || ah == bh {
		t.Fatalf("two consecutive tokens collided: %q vs %q", a, b)
	}
	// Hash must match HashSessionToken applied to the raw token.
	if HashSessionToken(a) != ah {
		t.Fatal("hash mismatch")
	}
}
