// Package muxserver implements the ssh-proxy side of the Phase 2 data
// plane: a TLS 1.3 listener that accepts compute-agent yamux sessions
// after authenticating each agent via main-api /internal/agent-auth
// (Task 0.4, ADR-009).
//
// The auth header is one JSON line sent by the agent immediately after
// the TLS handshake:
//
//	{"node_id":"...","token":"...","agent_version":"..."}
//
// The Verifier interface is what the connection accept loop calls. The
// production implementation (HTTPVerifier) wraps the HTTP call to
// main-api; tests substitute a fake. ErrUnauthenticated is the signal
// that lets the accept loop bump the prometheus auth-failure counter
// with a clean reason and drop the connection without retry.
package muxserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// ErrUnauthenticated is returned by Verifier.Verify when main-api refused
// the agent's (node_id, token) pair. Callers must treat this as a
// terminal authentication failure for the connection — no retry.
var ErrUnauthenticated = errors.New("muxserver: unauthenticated")

// AuthRequest is the JSON header agents send right after the TLS handshake.
type AuthRequest struct {
	NodeID       string `json:"node_id"`
	Token        string `json:"token"`
	AgentVersion string `json:"agent_version"`
}

// AuthResult is what main-api returns when the request is accepted. The
// fields are intentionally narrow — ssh-proxy only needs enough to log
// the agent version and (Phase 2.2+) tag streams with ACL metadata.
type AuthResult struct {
	NodeID           string
	AccessPolicy     string
	OwnerTeamID      string
	AgentVersionSeen string
}

// Verifier authenticates an AuthRequest. Implementations must be safe for
// concurrent use across many agent connections.
type Verifier interface {
	Verify(ctx context.Context, req AuthRequest) (AuthResult, error)
}

// HTTPVerifierConfig wires a production Verifier against main-api.
type HTTPVerifierConfig struct {
	// BaseURL is main-api's reachable URL ("http://127.0.0.1:8080" in
	// the co-located deployment, internal hostname in production).
	BaseURL string
	// InternalToken is the bearer token main-api gates /internal/* with.
	// Same secret as MAIN_API_INTERNAL_TOKEN.
	InternalToken string
	// HTTPClient is optional. Default has a 5s overall timeout.
	HTTPClient *http.Client
	// CacheTTL bounds how long a positive Verify result is reused for
	// the same (node_id, token) pair. 0 disables caching — recommended
	// in production until plan §S2 budget (combined ssh-proxy +
	// main-api revocation latency) is reconciled. Negative is treated
	// as 0.
	CacheTTL time.Duration
	// Now defaults to time.Now. Tests inject a clock to exercise TTL.
	Now func() time.Time
}

// HTTPVerifier is the production Verifier. Construct with NewHTTPVerifier.
type HTTPVerifier struct {
	baseURL       string
	internalToken string
	client        *http.Client
	cacheTTL      time.Duration
	now           func() time.Time

	mu    sync.Mutex
	cache map[cacheKey]cacheEntry
}

type cacheKey struct {
	nodeID string
	token  string
}

type cacheEntry struct {
	result    AuthResult
	expiresAt time.Time
}

// NewHTTPVerifier builds a Verifier targeting the given main-api endpoint.
func NewHTTPVerifier(cfg HTTPVerifierConfig) *HTTPVerifier {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	cli := cfg.HTTPClient
	if cli == nil {
		cli = &http.Client{Timeout: 5 * time.Second}
	}
	ttl := cfg.CacheTTL
	if ttl < 0 {
		ttl = 0
	}
	return &HTTPVerifier{
		baseURL:       cfg.BaseURL,
		internalToken: cfg.InternalToken,
		client:        cli,
		cacheTTL:      ttl,
		now:           now,
		cache:         make(map[cacheKey]cacheEntry),
	}
}

type wireResponse struct {
	OK               bool   `json:"ok"`
	NodeID           string `json:"node_id"`
	AccessPolicy     string `json:"access_policy"`
	OwnerTeamID      string `json:"owner_team_id"`
	AgentVersionSeen string `json:"agent_version_seen"`
}

// Verify implements the Verifier interface.
func (v *HTTPVerifier) Verify(ctx context.Context, req AuthRequest) (AuthResult, error) {
	if v.cacheTTL > 0 {
		key := cacheKey{nodeID: req.NodeID, token: req.Token}
		v.mu.Lock()
		if e, ok := v.cache[key]; ok && v.now().Before(e.expiresAt) {
			v.mu.Unlock()
			return e.result, nil
		}
		v.mu.Unlock()
	}

	body, err := json.Marshal(map[string]string{
		"node_id": req.NodeID,
		"token":   req.Token,
	})
	if err != nil {
		return AuthResult{}, fmt.Errorf("marshal: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		v.baseURL+"/internal/agent-auth", bytes.NewReader(body))
	if err != nil {
		return AuthResult{}, fmt.Errorf("new request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+v.internalToken)

	resp, err := v.client.Do(httpReq)
	if err != nil {
		return AuthResult{}, fmt.Errorf("agent-auth call: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
		var w wireResponse
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		if err := json.Unmarshal(buf, &w); err != nil {
			return AuthResult{}, fmt.Errorf("decode 200: %w", err)
		}
		if !w.OK {
			// 200 with ok=false would be a server bug — treat as
			// unauthenticated to avoid trusting unknown state.
			return AuthResult{}, ErrUnauthenticated
		}
		result := AuthResult{
			NodeID:           w.NodeID,
			AccessPolicy:     w.AccessPolicy,
			OwnerTeamID:      w.OwnerTeamID,
			AgentVersionSeen: w.AgentVersionSeen,
		}
		if v.cacheTTL > 0 {
			key := cacheKey{nodeID: req.NodeID, token: req.Token}
			v.mu.Lock()
			v.cache[key] = cacheEntry{result: result, expiresAt: v.now().Add(v.cacheTTL)}
			v.mu.Unlock()
		}
		return result, nil
	case http.StatusUnauthorized:
		return AuthResult{}, ErrUnauthenticated
	default:
		return AuthResult{}, fmt.Errorf("agent-auth status %d", resp.StatusCode)
	}
}
