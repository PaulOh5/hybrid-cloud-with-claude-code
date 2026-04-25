package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"

	"hybridcloud/services/main-api/internal/db/dbstore"
	"hybridcloud/services/main-api/internal/sshkeys"
)

// SSHKeyStore is the slice of *sshkeys.Repo the HTTP layer uses.
type SSHKeyStore interface {
	Add(ctx context.Context, userID uuid.UUID, label, pubkey string) (dbstore.SshKey, error)
	List(ctx context.Context, userID uuid.UUID) ([]dbstore.SshKey, error)
	Delete(ctx context.Context, userID, id uuid.UUID) error
}

// UserSSHKeyHandlers serves /api/v1/ssh-keys for the authenticated user.
type UserSSHKeyHandlers struct {
	Keys SSHKeyStore
}

// SSHKeyView is the JSON shape returned to the dashboard. The pubkey body is
// included so the UI can show a fingerprint preview without re-fetching.
type SSHKeyView struct {
	ID          uuid.UUID `json:"id"`
	Label       string    `json:"label"`
	Pubkey      string    `json:"pubkey"`
	Fingerprint string    `json:"fingerprint"`
	CreatedAt   time.Time `json:"created_at"`
}

func toSSHKeyView(k dbstore.SshKey) SSHKeyView {
	return SSHKeyView{
		ID:          k.ID,
		Label:       k.Label,
		Pubkey:      k.Pubkey,
		Fingerprint: k.Fingerprint,
		CreatedAt:   k.CreatedAt.Time,
	}
}

type addSSHKeyRequest struct {
	Label  string `json:"label"`
	Pubkey string `json:"pubkey"`
}

// List serves GET /api/v1/ssh-keys.
func (h *UserSSHKeyHandlers) List(w http.ResponseWriter, r *http.Request) {
	user, _ := UserFromContext(r.Context())
	rows, err := h.Keys.List(r.Context(), user.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list_failed", err.Error())
		return
	}
	views := make([]SSHKeyView, 0, len(rows))
	for _, k := range rows {
		views = append(views, toSSHKeyView(k))
	}
	writeJSON(w, http.StatusOK, map[string]any{"ssh_keys": views})
}

// Add serves POST /api/v1/ssh-keys.
func (h *UserSSHKeyHandlers) Add(w http.ResponseWriter, r *http.Request) {
	user, _ := UserFromContext(r.Context())
	defer func() { _ = r.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<14))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read_body", err.Error())
		return
	}
	var req addSSHKeyRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	row, err := h.Keys.Add(r.Context(), user.ID, req.Label, req.Pubkey)
	if err != nil {
		switch {
		case errors.Is(err, sshkeys.ErrDuplicate):
			writeError(w, http.StatusConflict, "duplicate", "key already added")
		case errors.Is(err, sshkeys.ErrInvalidPubkey):
			writeError(w, http.StatusBadRequest, "invalid_pubkey", err.Error())
		default:
			writeError(w, http.StatusInternalServerError, "add_failed", err.Error())
		}
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"ssh_key": toSSHKeyView(row)})
}

// Delete serves DELETE /api/v1/ssh-keys/{id}.
func (h *UserSSHKeyHandlers) Delete(w http.ResponseWriter, r *http.Request) {
	user, _ := UserFromContext(r.Context())
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_id", err.Error())
		return
	}
	if err := h.Keys.Delete(r.Context(), user.ID, id); err != nil {
		// Not-found is the no-enumerate response — if the key belongs to
		// somebody else, the user should not be able to tell.
		if errors.Is(err, sshkeys.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "ssh key not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "delete_failed", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
