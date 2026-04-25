package api

import (
	"context"
	"net/http"
	"time"

	"github.com/google/uuid"

	"hybridcloud/services/main-api/internal/db/dbstore"
)

// AdminQueries is the slice of dbstore queries the session-admin handlers
// need. Narrow interface so tests don't need a postgres container.
type AdminQueries interface {
	ListUsersAdminView(ctx context.Context, limit int32) ([]dbstore.ListUsersAdminViewRow, error)
	ListSlotsForAdminView(ctx context.Context) ([]dbstore.ListSlotsForAdminViewRow, error)
}

// SessionAdminHandlers serves /api/v1/admin/* — same operator views as the
// bearer-token /admin/* routes, but reachable from the dashboard via the
// authenticated session cookie. Wrapped in RequireAdmin so non-admins get
// 404, not 403.
type SessionAdminHandlers struct {
	Queries AdminQueries
}

// AdminUserView is the JSON shape returned for the users table.
type AdminUserView struct {
	ID                  uuid.UUID `json:"id"`
	Email               string    `json:"email"`
	IsAdmin             bool      `json:"is_admin"`
	BalanceMilli        int64     `json:"balance_milli"`
	ActiveInstanceCount int64     `json:"active_instance_count"`
	CreatedAt           time.Time `json:"created_at"`
}

// AdminSlotView is the JSON shape returned for the slots table.
type AdminSlotView struct {
	ID                uuid.UUID  `json:"id"`
	NodeID            uuid.UUID  `json:"node_id"`
	NodeName          string     `json:"node_name"`
	SlotIndex         int32      `json:"slot_index"`
	GPUCount          int32      `json:"gpu_count"`
	GPUIndices        []int32    `json:"gpu_indices"`
	NVLinkDomain      string     `json:"nvlink_domain"`
	Status            string     `json:"status"`
	CurrentInstanceID *uuid.UUID `json:"current_instance_id,omitempty"`
}

// ListUsers serves GET /api/v1/admin/users.
func (h *SessionAdminHandlers) ListUsers(w http.ResponseWriter, r *http.Request) {
	rows, err := h.Queries.ListUsersAdminView(r.Context(), 200)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list_users_failed", err.Error())
		return
	}
	views := make([]AdminUserView, 0, len(rows))
	for _, row := range rows {
		views = append(views, AdminUserView{
			ID:                  row.ID,
			Email:               row.Email,
			IsAdmin:             row.IsAdmin,
			BalanceMilli:        row.BalanceMilli,
			ActiveInstanceCount: row.ActiveInstanceCount,
			CreatedAt:           row.CreatedAt.Time,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": views})
}

// ListSlots serves GET /api/v1/admin/slots.
func (h *SessionAdminHandlers) ListSlots(w http.ResponseWriter, r *http.Request) {
	rows, err := h.Queries.ListSlotsForAdminView(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list_slots_failed", err.Error())
		return
	}
	views := make([]AdminSlotView, 0, len(rows))
	for _, row := range rows {
		v := AdminSlotView{
			ID:           row.ID,
			NodeID:       row.NodeID,
			NodeName:     row.NodeName,
			SlotIndex:    row.SlotIndex,
			GPUCount:     row.GpuCount,
			GPUIndices:   row.GpuIndices,
			NVLinkDomain: row.NvlinkDomain,
			Status:       string(row.Status),
		}
		if row.CurrentInstanceID.Valid {
			id := row.CurrentInstanceID.UUID
			v.CurrentInstanceID = &id
		}
		views = append(views, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"slots": views})
}
