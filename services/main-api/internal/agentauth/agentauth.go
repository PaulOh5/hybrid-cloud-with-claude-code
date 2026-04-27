// Package agentauth implements the POST /internal/agent-auth endpoint
// ssh-proxy calls during a yamux/TLS handshake to validate an agent's
// (node_id, token) pair. It is the single source of truth for agent
// credentials — Phase 2 ADR-009.
//
// The handler runs behind RequireInternalToken (api.RequireInternalToken)
// so only ssh-proxy can reach it. The plaintext token presented by the
// agent is bcrypt-compared against active rows in node_tokens for that
// node_id; revoked rows are filtered out at the repo layer.
//
// Token validation is the only response signal — the handler intentionally
// returns 401 for both "wrong token" and "no such node" so a probe cannot
// enumerate node ids by inspecting status codes.
//
// A small in-memory cache (default 60s TTL, plan §S2) keeps DB+bcrypt cost
// off the hot path while still bounding the operator's revocation latency.
// The TTL is tunable via Config.CacheTTL; tests inject Config.Now to
// exercise expiry deterministically.
package agentauth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"

	"hybridcloud/services/main-api/internal/db/dbstore"
)

// ErrNodeNotFound is returned by Repo.NodeAuthView when the node row does
// not exist. Handlers map this to 401 (same as wrong token) so the response
// does not leak which node ids are registered.
var ErrNodeNotFound = errors.New("node not found")

// Repo is the storage surface the handler needs. The production
// implementation lives in this package (PgRepo) and wraps dbstore.Queries;
// tests substitute a fake.
type Repo interface {
	// ListActiveNodeTokens returns the bcrypt hashes of every active
	// (revoked_at IS NULL) token for the node. Order does not matter —
	// the handler iterates and bcrypt-compares each entry.
	ListActiveNodeTokens(ctx context.Context, nodeID uuid.UUID) ([]dbstore.NodeToken, error)

	// NodeAuthView returns the ACL-relevant fields ssh-proxy passes back to
	// itself with the auth result. Returns ErrNodeNotFound when the row
	// does not exist.
	NodeAuthView(ctx context.Context, nodeID uuid.UUID) (NodeView, error)
}

// NodeView is the ACL-shaped projection of nodes that the handler returns
// in its response payload. Kept narrow so production wiring does not have
// to expose the whole nodes row to ssh-proxy.
type NodeView struct {
	NodeID       uuid.UUID
	AccessPolicy string
	OwnerTeamID  uuid.NullUUID
	AgentVersion string
}

// Config controls a Handler.
type Config struct {
	Repo     Repo
	CacheTTL time.Duration

	// Now defaults to time.Now. Tests inject a clock to exercise TTL.
	Now func() time.Time
}

// Handler is the http.Handler returned by NewHandler. The exposed type is
// concrete (rather than http.Handler) because tests need direct access to
// the cache for expiry assertions.
type Handler struct {
	repo     Repo
	cacheTTL time.Duration
	now      func() time.Time

	mu    sync.Mutex
	cache map[cacheKey]cacheEntry
}

// cacheKey is the (node_id, token) pair. Token is held in memory only for
// the cache lifetime — the cache is per-process and does not survive
// restart, so leaking it through the heap is acceptable for the beta scale.
type cacheKey struct {
	nodeID uuid.UUID
	token  string
}

type cacheEntry struct {
	view      NodeView
	expiresAt time.Time
}

// NewHandler returns the http.HandlerFunc for /internal/agent-auth.
//
// CacheTTL <= 0 disables caching entirely — every request hits the repo
// (used by integration tests that exercise revocation latency without
// waiting on real time). Production wiring passes 60 * time.Second per
// plan §S2.
func NewHandler(cfg Config) *Handler {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Handler{
		repo:     cfg.Repo,
		cacheTTL: cfg.CacheTTL,
		now:      now,
		cache:    make(map[cacheKey]cacheEntry),
	}
}

type request struct {
	NodeID string `json:"node_id"`
	Token  string `json:"token"`
}

type response struct {
	OK               bool   `json:"ok"`
	NodeID           string `json:"node_id"`
	AccessPolicy     string `json:"access_policy"`
	OwnerTeamID      string `json:"owner_team_id,omitempty"`
	AgentVersionSeen string `json:"agent_version_seen"`
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer func() { _ = r.Body.Close() }()

	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST required")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<10))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read_body", err.Error())
		return
	}
	var req request
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	nodeID, err := uuid.Parse(req.NodeID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_node_id", "node_id must be a UUID")
		return
	}
	if req.Token == "" {
		// Treat as 401 not 400 — a missing token is an auth failure, not a
		// schema violation, and matches what ssh-proxy will see for a
		// rotated-out credential.
		writeError(w, http.StatusUnauthorized, "unauthenticated", "token required")
		return
	}

	view, ok := h.lookup(r.Context(), nodeID, req.Token)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthenticated", "invalid or revoked token")
		return
	}

	resp := response{
		OK:               true,
		NodeID:           nodeID.String(),
		AccessPolicy:     view.AccessPolicy,
		AgentVersionSeen: view.AgentVersion,
	}
	if view.OwnerTeamID.Valid {
		resp.OwnerTeamID = view.OwnerTeamID.UUID.String()
	}
	writeJSON(w, http.StatusOK, resp)
}

// lookup returns the cached or freshly-fetched NodeView for the (node, token)
// pair, or false if no active token matches.
func (h *Handler) lookup(ctx context.Context, nodeID uuid.UUID, token string) (NodeView, bool) {
	key := cacheKey{nodeID: nodeID, token: token}

	if h.cacheTTL > 0 {
		h.mu.Lock()
		if e, ok := h.cache[key]; ok && h.now().Before(e.expiresAt) {
			h.mu.Unlock()
			return e.view, true
		}
		h.mu.Unlock()
	}

	// Cache miss / expired — go to the repo.
	tokens, err := h.repo.ListActiveNodeTokens(ctx, nodeID)
	if err != nil || len(tokens) == 0 {
		return NodeView{}, false
	}

	matched := false
	for _, t := range tokens {
		if bcrypt.CompareHashAndPassword([]byte(t.TokenHash), []byte(token)) == nil {
			matched = true
			break
		}
	}
	if !matched {
		return NodeView{}, false
	}

	view, err := h.repo.NodeAuthView(ctx, nodeID)
	if err != nil {
		// Token matched but node row missing — treat as auth failure
		// rather than 500 so ssh-proxy gets a single failure mode.
		return NodeView{}, false
	}

	if h.cacheTTL > 0 {
		h.mu.Lock()
		h.cache[key] = cacheEntry{view: view, expiresAt: h.now().Add(h.cacheTTL)}
		h.mu.Unlock()
	}

	return view, true
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"code": code, "error": msg})
}
