package muxserver_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"hybridcloud/services/ssh-proxy/internal/muxserver"
)

// Phase 2.1 Task 1.1 — HTTPVerifier wraps the call to main-api
// /internal/agent-auth (Phase 2.0 Task 0.4). Failure modes the muxserver
// must distinguish:
//   - 200 + body         → AuthResult populated
//   - 401                → ErrUnauthenticated (signal: refuse the agent)
//   - 5xx / network err  → wrapped error (signal: drop, retry on next session)

func newAuthServer(t *testing.T, expectedToken string, handler func(req map[string]string) (int, any)) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	calls := &atomic.Int64{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodPost {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Path != "/internal/agent-auth" {
			http.Error(w, "path", http.StatusNotFound)
			return
		}
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if got != expectedToken {
			http.Error(w, "auth", http.StatusUnauthorized)
			return
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "json", http.StatusBadRequest)
			return
		}
		status, payload := handler(body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(payload)
	}))
	t.Cleanup(srv.Close)
	return srv, calls
}

func TestHTTPVerifier_HappyPath(t *testing.T) {
	t.Parallel()

	srv, calls := newAuthServer(t, "internal-bearer", func(req map[string]string) (int, any) {
		if req["node_id"] != "node-A" || req["token"] != "tok" {
			return http.StatusUnauthorized, map[string]string{"code": "wrong"}
		}
		return http.StatusOK, map[string]any{
			"ok":                 true,
			"node_id":            "node-A",
			"access_policy":      "owner_team",
			"owner_team_id":      "team-X",
			"agent_version_seen": "0.2.5",
		}
	})

	v := muxserver.NewHTTPVerifier(muxserver.HTTPVerifierConfig{
		BaseURL:       srv.URL,
		InternalToken: "internal-bearer",
	})

	res, err := v.Verify(context.Background(), muxserver.AuthRequest{
		NodeID:       "node-A",
		Token:        "tok",
		AgentVersion: "0.2.5",
	})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if res.NodeID != "node-A" || res.AccessPolicy != "owner_team" || res.OwnerTeamID != "team-X" || res.AgentVersionSeen != "0.2.5" {
		t.Fatalf("unexpected result: %+v", res)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("call count: got %d, want 1", got)
	}
}

func TestHTTPVerifier_Unauthenticated(t *testing.T) {
	t.Parallel()

	srv, _ := newAuthServer(t, "internal-bearer", func(map[string]string) (int, any) {
		return http.StatusUnauthorized, map[string]string{"code": "unauthenticated"}
	})

	v := muxserver.NewHTTPVerifier(muxserver.HTTPVerifierConfig{
		BaseURL:       srv.URL,
		InternalToken: "internal-bearer",
	})

	_, err := v.Verify(context.Background(), muxserver.AuthRequest{NodeID: "x", Token: "y"})
	if !errors.Is(err, muxserver.ErrUnauthenticated) {
		t.Fatalf("expected ErrUnauthenticated, got %v", err)
	}
}

func TestHTTPVerifier_ServerError(t *testing.T) {
	t.Parallel()

	srv, _ := newAuthServer(t, "internal-bearer", func(map[string]string) (int, any) {
		return http.StatusInternalServerError, map[string]string{"code": "boom"}
	})

	v := muxserver.NewHTTPVerifier(muxserver.HTTPVerifierConfig{
		BaseURL:       srv.URL,
		InternalToken: "internal-bearer",
	})

	_, err := v.Verify(context.Background(), muxserver.AuthRequest{NodeID: "x", Token: "y"})
	if err == nil {
		t.Fatal("expected error on 500")
	}
	if errors.Is(err, muxserver.ErrUnauthenticated) {
		t.Fatalf("5xx must not map to ErrUnauthenticated: %v", err)
	}
}

func TestHTTPVerifier_CacheHit(t *testing.T) {
	t.Parallel()

	srv, calls := newAuthServer(t, "internal-bearer", func(map[string]string) (int, any) {
		return http.StatusOK, map[string]any{
			"ok":                 true,
			"node_id":            "node-A",
			"access_policy":      "public",
			"agent_version_seen": "0.2.0",
		}
	})

	v := muxserver.NewHTTPVerifier(muxserver.HTTPVerifierConfig{
		BaseURL:       srv.URL,
		InternalToken: "internal-bearer",
		CacheTTL:      time.Minute,
	})

	for i := 0; i < 3; i++ {
		if _, err := v.Verify(context.Background(), muxserver.AuthRequest{NodeID: "node-A", Token: "tok"}); err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream calls: got %d, want 1 (cache should serve 2nd+)", got)
	}
}

func TestHTTPVerifier_CacheTTLZeroDisables(t *testing.T) {
	t.Parallel()

	srv, calls := newAuthServer(t, "internal-bearer", func(map[string]string) (int, any) {
		return http.StatusOK, map[string]any{
			"ok":                 true,
			"node_id":            "node-A",
			"access_policy":      "public",
			"agent_version_seen": "0.2.0",
		}
	})

	v := muxserver.NewHTTPVerifier(muxserver.HTTPVerifierConfig{
		BaseURL:       srv.URL,
		InternalToken: "internal-bearer",
		CacheTTL:      0, // disabled
	})

	for i := 0; i < 3; i++ {
		_, _ = v.Verify(context.Background(), muxserver.AuthRequest{NodeID: "node-A", Token: "tok"})
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("upstream calls: got %d, want 3 (cache disabled)", got)
	}
}

func TestHTTPVerifier_CacheMissAfterTTL(t *testing.T) {
	t.Parallel()

	srv, calls := newAuthServer(t, "internal-bearer", func(map[string]string) (int, any) {
		return http.StatusOK, map[string]any{
			"ok":                 true,
			"node_id":            "node-A",
			"access_policy":      "public",
			"agent_version_seen": "0.2.0",
		}
	})

	now := time.Unix(1_700_000_000, 0)
	clock := &fakeClock{now: now}
	v := muxserver.NewHTTPVerifier(muxserver.HTTPVerifierConfig{
		BaseURL:       srv.URL,
		InternalToken: "internal-bearer",
		CacheTTL:      30 * time.Second,
		Now:           clock.Now,
	})

	if _, err := v.Verify(context.Background(), muxserver.AuthRequest{NodeID: "node-A", Token: "tok"}); err != nil {
		t.Fatalf("first: %v", err)
	}
	clock.now = now.Add(31 * time.Second)
	if _, err := v.Verify(context.Background(), muxserver.AuthRequest{NodeID: "node-A", Token: "tok"}); err != nil {
		t.Fatalf("second: %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream calls: got %d, want 2", got)
	}
}

type fakeClock struct{ now time.Time }

func (f *fakeClock) Now() time.Time { return f.now }
