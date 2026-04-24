package router_test

import (
	"errors"
	"testing"

	"hybridcloud/services/ssh-proxy/internal/router"
)

func TestExtractRoute_Accepts(t *testing.T) {
	t.Parallel()

	cases := []struct {
		target, zone string
		wantPrefix   string
	}{
		{"abc12345.hybrid-cloud.com", "hybrid-cloud.com", "abc12345"},
		{"ABC12345.hybrid-cloud.com", "hybrid-cloud.com", "abc12345"},  // lowercased
		{"abc12345.hybrid-cloud.com.", "hybrid-cloud.com", "abc12345"}, // trailing dot
		{"inst01.staging.example.com", "staging.example.com", "inst01"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.target, func(t *testing.T) {
			t.Parallel()
			r, err := router.ExtractRoute(tc.target, tc.zone)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if r.Prefix != tc.wantPrefix {
				t.Fatalf("prefix: got %q, want %q", r.Prefix, tc.wantPrefix)
			}
		})
	}
}

func TestExtractRoute_RejectsOutOfZone(t *testing.T) {
	t.Parallel()

	cases := []string{
		"abc12345.evil.com",
		"abc12345.hybrid-cloud.net", // wrong TLD
		"hybrid-cloud.com",          // no subdomain
		"",
	}
	for _, target := range cases {
		target := target
		t.Run(target, func(t *testing.T) {
			t.Parallel()
			_, err := router.ExtractRoute(target, "hybrid-cloud.com")
			if !errors.Is(err, router.ErrWrongZone) && !errors.Is(err, router.ErrBadSubdomain) {
				t.Fatalf("expected ErrWrongZone/ErrBadSubdomain, got %v", err)
			}
		})
	}
}

func TestExtractRoute_RejectsNestedSubdomain(t *testing.T) {
	t.Parallel()

	_, err := router.ExtractRoute("a.b.hybrid-cloud.com", "hybrid-cloud.com")
	if !errors.Is(err, router.ErrBadSubdomain) {
		t.Fatalf("expected ErrBadSubdomain, got %v", err)
	}
}
