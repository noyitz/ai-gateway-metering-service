package handler

import (
	"encoding/json"
	"net/http"

	"github.com/noyitz/ai-gateway-metering-service/internal/storage"
)

type TeamUsageHandler struct {
	store *storage.Store
}

func NewTeamUsageHandler(store *storage.Store) *TeamUsageHandler {
	return &TeamUsageHandler{store: store}
}

func (h *TeamUsageHandler) HandleTeamUsage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	group := r.URL.Query().Get("group")
	if group == "" {
		group = "ron-team"
	}

	rows, err := h.store.GetTeamUsage(r.Context(), group)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	json.NewEncoder(w).Encode(rows)
}
