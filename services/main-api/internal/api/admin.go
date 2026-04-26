// Package api hosts main-api's HTTP handlers. The admin subset is behind a
// bearer-token middleware and exposes the operator views; the user-facing API
// lands in Phase 7.
package api

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"hybridcloud/services/main-api/internal/db/dbstore"
	"hybridcloud/services/main-api/internal/node"
)

// AdminHandlers holds dependencies for /admin/* endpoints.
type AdminHandlers struct {
	Nodes node.Repo
}

// NodeView is the public JSON shape returned by /admin/nodes. It hides the
// topology blob (operator drills into /admin/nodes/{id} for that in a later
// phase) and surfaces the operator-useful derived count.
type NodeView struct {
	ID              uuid.UUID  `json:"id"`
	NodeName        string     `json:"node_name"`
	Hostname        string     `json:"hostname"`
	Status          string     `json:"status"`
	AgentVersion    string     `json:"agent_version"`
	GPUCount        int        `json:"gpu_count"`
	LastHeartbeatAt *time.Time `json:"last_heartbeat_at,omitempty"`
	RegisteredAt    time.Time  `json:"registered_at"`
}

// ListNodes serves GET /admin/nodes.
func (h *AdminHandlers) ListNodes(w http.ResponseWriter, r *http.Request) {
	nodes, err := h.Nodes.List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list_nodes_failed", err.Error())
		return
	}
	views := make([]NodeView, 0, len(nodes))
	for _, n := range nodes {
		views = append(views, toView(n))
	}
	writeJSON(w, http.StatusOK, map[string]any{"nodes": views})
}

func toView(n dbstore.Node) NodeView {
	v := NodeView{
		ID:           n.ID,
		NodeName:     n.NodeName,
		Hostname:     n.Hostname,
		Status:       string(n.Status),
		AgentVersion: n.AgentVersion,
		GPUCount:     gpuCountFromTopology(n.TopologyJson),
		RegisteredAt: n.RegisteredAt.Time,
	}
	if n.LastHeartbeatAt.Valid {
		t := n.LastHeartbeatAt.Time
		v.LastHeartbeatAt = &t
	}
	return v
}

// gpuCountFromTopology extracts len(topology.gpus) from the stored JSON
// without forcing callers to unmarshal into the proto type. The JSON is
// produced by protojsonMarshal, so the key is "gpus".
func gpuCountFromTopology(raw []byte) int {
	if len(raw) == 0 {
		return 0
	}
	var partial struct {
		Gpus []json.RawMessage `json:"gpus"`
	}
	if err := json.Unmarshal(raw, &partial); err != nil {
		return 0
	}
	return len(partial.Gpus)
}

// --- middleware + helpers --------------------------------------------------

// RequireAdminToken returns a middleware that rejects any request without the
// matching bearer token. Empty expected disables the middleware (useful in
// tests); production boot MUST set it.
func RequireAdminToken(expected string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if expected == "" {
				next.ServeHTTP(w, r)
				return
			}
			h := r.Header.Get("Authorization")
			token := strings.TrimPrefix(h, "Bearer ")
			if token == h || subtle.ConstantTimeCompare([]byte(token), []byte(expected)) != 1 {
				writeError(w, http.StatusUnauthorized, "unauthenticated", "invalid or missing bearer token")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{
		"code":    code,
		"message": msg,
	}})
}
