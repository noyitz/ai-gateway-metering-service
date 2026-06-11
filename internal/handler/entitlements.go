package handler

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/noyitz/ai-gateway-metering-service/internal/storage"
)

type EntitlementsHandler struct {
	store *storage.Store
}

func NewEntitlementsHandler(store *storage.Store) *EntitlementsHandler {
	return &EntitlementsHandler{store: store}
}

func (h *EntitlementsHandler) HandleEntitlement(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	parts := strings.Split(r.URL.Path, "/")
	var username string
	for i, p := range parts {
		if p == "customers" && i+1 < len(parts) {
			username = parts[i+1]
			break
		}
	}
	if username == "" {
		http.Error(w, "missing customer ID", http.StatusBadRequest)
		return
	}

	model := r.URL.Query().Get("model")

	stats, err := h.store.GetMonthlyUsage(r.Context(), username, model)
	if err != nil {
		slog.Error("failed to get usage", "error", err, "user", username)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(stats)
}
