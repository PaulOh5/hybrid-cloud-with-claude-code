package api

import "net/http"

// UserCreditHandlers serves GET /api/v1/credits — the authenticated user's
// own balance + recent ledger.
type UserCreditHandlers struct {
	Credits CreditPoster
}

// Balance returns balance + ledger for the caller.
func (h *UserCreditHandlers) Balance(w http.ResponseWriter, r *http.Request) {
	user, _ := UserFromContext(r.Context())
	balance, err := h.Credits.Balance(r.Context(), user.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "balance_failed", err.Error())
		return
	}
	entries, err := h.Credits.History(r.Context(), user.ID, 50)
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
