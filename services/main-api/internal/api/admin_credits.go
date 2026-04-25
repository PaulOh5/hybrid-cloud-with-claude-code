package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"

	"hybridcloud/services/main-api/internal/credit"
	"hybridcloud/services/main-api/internal/db/dbstore"
)

// CreditPoster is the slice of *credit.Repo the HTTP layer calls.
type CreditPoster interface {
	Post(ctx context.Context, in credit.PostInput) (dbstore.CreditLedger, error)
	Balance(ctx context.Context, userID uuid.UUID) (int64, error)
	History(ctx context.Context, userID uuid.UUID, limit int32) ([]dbstore.CreditLedger, error)
}

// AdminCreditHandlers serves /admin/users/{id}/credits.
type AdminCreditHandlers struct {
	Credits CreditPoster
}

type adminRechargeRequest struct {
	DeltaMilli     int64           `json:"delta_milli"`
	Reason         string          `json:"reason"`
	IdempotencyKey string          `json:"idempotency_key"`
	Metadata       json.RawMessage `json:"metadata,omitempty"`
}

// CreditEntryView mirrors the JSON shape returned to operators / users.
type CreditEntryView struct {
	ID             int64           `json:"id"`
	DeltaMilli     int64           `json:"delta_milli"`
	Reason         string          `json:"reason"`
	IdempotencyKey string          `json:"idempotency_key"`
	InstanceID     *uuid.UUID      `json:"instance_id,omitempty"`
	Metadata       json.RawMessage `json:"metadata"`
	CreatedAt      time.Time       `json:"created_at"`
}

func toCreditEntryView(e dbstore.CreditLedger) CreditEntryView {
	v := CreditEntryView{
		ID:             e.ID,
		DeltaMilli:     e.DeltaMilli,
		Reason:         e.Reason,
		IdempotencyKey: e.IdempotencyKey,
		Metadata:       json.RawMessage(e.Metadata),
		CreatedAt:      e.CreatedAt.Time,
	}
	if e.InstanceID.Valid {
		id := e.InstanceID.UUID
		v.InstanceID = &id
	}
	return v
}

// Recharge serves POST /admin/users/{id}/credits. Body must include a
// distinct idempotency_key per intended charge — repeated requests with the
// same key are 409.
func (h *AdminCreditHandlers) Recharge(w http.ResponseWriter, r *http.Request) {
	defer func() { _ = r.Body.Close() }()
	userID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_user_id", err.Error())
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<14))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read_body", err.Error())
		return
	}
	var req adminRechargeRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	if req.DeltaMilli == 0 {
		writeError(w, http.StatusBadRequest, "zero_delta", "delta_milli must be non-zero")
		return
	}
	if req.Reason == "" {
		writeError(w, http.StatusBadRequest, "missing_reason", "reason is required")
		return
	}
	if req.IdempotencyKey == "" {
		writeError(w, http.StatusBadRequest, "missing_idempotency_key", "idempotency_key is required")
		return
	}

	entry, err := h.Credits.Post(r.Context(), credit.PostInput{
		UserID:         userID,
		DeltaMilli:     req.DeltaMilli,
		Reason:         req.Reason,
		IdempotencyKey: req.IdempotencyKey,
		Metadata:       req.Metadata,
	})
	if err != nil {
		if errors.Is(err, credit.ErrDuplicateIdempotency) {
			writeError(w, http.StatusConflict, "duplicate_idempotency", "ledger entry already exists for this key")
			return
		}
		writeError(w, http.StatusInternalServerError, "post_failed", err.Error())
		return
	}
	balance, err := h.Credits.Balance(r.Context(), userID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "balance_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"entry":         toCreditEntryView(entry),
		"balance_milli": balance,
	})
}

// Balance serves GET /admin/users/{id}/credits.
func (h *AdminCreditHandlers) Balance(w http.ResponseWriter, r *http.Request) {
	userID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_user_id", err.Error())
		return
	}
	balance, err := h.Credits.Balance(r.Context(), userID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "balance_failed", err.Error())
		return
	}
	entries, err := h.Credits.History(r.Context(), userID, 50)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "history_failed", err.Error())
		return
	}
	views := make([]CreditEntryView, 0, len(entries))
	for _, e := range entries {
		views = append(views, toCreditEntryView(e))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"balance_milli": balance,
		"ledger":        views,
	})
}
