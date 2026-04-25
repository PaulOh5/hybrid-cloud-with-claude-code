package api

import (
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"hybridcloud/services/main-api/internal/db/dbstore"
	grpcsrv "hybridcloud/services/main-api/internal/grpc"
	"hybridcloud/services/main-api/internal/instance"
	agentv1 "hybridcloud/shared/proto/agent/v1"
)

// UserInstanceHandlers serves the /api/v1/instances/* endpoints scoped to the
// authenticated user. The admin handlers in admin_instances.go remain in
// place for operator workflows; this type narrows visibility to owner_id.
//
// Create defers to InstanceHandlers.CreateForOwner so reservation + dispatch
// behave identically to the admin path.
type UserInstanceHandlers struct {
	Admin *InstanceHandlers
}

// NewUserInstanceHandlers wires a user-facing handler around the admin one.
func NewUserInstanceHandlers(admin *InstanceHandlers) *UserInstanceHandlers {
	return &UserInstanceHandlers{Admin: admin}
}

// List returns the caller's own instances.
func (h *UserInstanceHandlers) List(w http.ResponseWriter, r *http.Request) {
	user, _ := UserFromContext(r.Context())
	rows, err := h.Admin.Instances.ListForOwner(r.Context(), uuid.NullUUID{UUID: user.ID, Valid: true})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list_failed", err.Error())
		return
	}
	views := make([]InstanceView, 0, len(rows))
	for _, row := range rows {
		views = append(views, toInstanceView(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{"instances": views})
}

// Get returns one of the caller's instances. Returns 404 (not 403) when the
// instance exists but belongs to someone else, so an attacker cannot
// enumerate IDs across users.
func (h *UserInstanceHandlers) Get(w http.ResponseWriter, r *http.Request) {
	user, _ := UserFromContext(r.Context())
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_id", err.Error())
		return
	}
	inst, err := h.Admin.Instances.Get(r.Context(), id)
	if err != nil {
		// Both "missing row" and a generic lookup failure collapse to 404
		// here so user-A cannot probe whether user-B's UUIDs exist. A real
		// pgx.ErrNoRows is the common path; other errors are rare and the
		// no-enumerate property matters more than fidelity.
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "not_found", "instance not found")
			return
		}
		writeError(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	if !ownsInstance(inst, user) {
		writeError(w, http.StatusNotFound, "not_found", "instance not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"instance": toInstanceView(inst)})
}

// Create stamps the caller as owner and forwards to the admin Create logic.
func (h *UserInstanceHandlers) Create(w http.ResponseWriter, r *http.Request) {
	user, _ := UserFromContext(r.Context())
	h.Admin.CreateForOwner(w, r, user.ID)
}

// Delete behaves like the admin Delete but only when the row's owner_id
// matches the caller. Mismatch surfaces as 404 to avoid enumerate.
func (h *UserInstanceHandlers) Delete(w http.ResponseWriter, r *http.Request) {
	user, _ := UserFromContext(r.Context())
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_id", err.Error())
		return
	}
	inst, err := h.Admin.Instances.Get(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "instance not found")
		return
	}
	if !ownsInstance(inst, user) {
		writeError(w, http.StatusNotFound, "not_found", "instance not found")
		return
	}

	if inst.State == instance.StateStopped || inst.State == instance.StateFailed {
		if h.Admin.Slots != nil {
			_, _ = h.Admin.Slots.ReleaseForInstance(r.Context(), id)
		}
		if err := h.Admin.Instances.Delete(r.Context(), id); err != nil {
			writeError(w, http.StatusInternalServerError, "delete_failed", err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if _, err := h.Admin.Instances.Transition(r.Context(), id, instance.StateStopping, instance.TransitionOptions{
		Reason: "user_delete",
	}); err != nil && !errors.Is(err, instance.ErrInvalidTransition) {
		writeError(w, http.StatusInternalServerError, "transition_failed", err.Error())
		return
	}

	msg := &agentv1.ControlMessage{
		Payload: &agentv1.ControlMessage_DestroyInstance{
			DestroyInstance: &agentv1.DestroyInstance{InstanceId: inst.ID.String()},
		},
	}
	if err := h.Admin.Dispatcher.Send(inst.NodeID, msg); err != nil && !errors.Is(err, grpcsrv.ErrAgentNotConnected) {
		writeError(w, http.StatusBadGateway, "dispatch_failed", err.Error())
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// ownsInstance reports whether inst.OwnerID == user.ID. Admins also pass.
func ownsInstance(inst dbstore.Instance, user dbstore.User) bool {
	if user.IsAdmin {
		return true
	}
	return inst.OwnerID.Valid && inst.OwnerID.UUID == user.ID
}
