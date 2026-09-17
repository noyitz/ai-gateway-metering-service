package handler

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/noyitz/ai-gateway-metering-service/internal/config"
	"github.com/noyitz/ai-gateway-metering-service/internal/storage"
)

type EntitlementsHandler struct {
	store *storage.Store
	cfg   config.Config
}

func NewEntitlementsHandler(store *storage.Store, cfg config.Config) *EntitlementsHandler {
	return &EntitlementsHandler{store: store, cfg: cfg}
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

	// Operators never gate themselves: a quota misfire must not lock the
	// people who can fix it out of their own dogfood keys.
	exempt := IsSuperAdminUsername(h.cfg, username)

	stats, err := h.store.GetMonthlyUsage(r.Context(), username, model, exempt)
	if err != nil {
		slog.Error("failed to get usage", "error", err, "user", username)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(stats)

	// Count the denial AFTER the answer is on the wire: this runs on the
	// gateway's synchronous 5s path, and a hasAccess:false here is exactly
	// the request the client sees as a 429 (fail_open means no denial when
	// we are unreachable, so there is no 429 this counter could miss).
	// Detached context — the request context dies when the handler returns.
	if !stats.HasAccess {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := h.store.RecordQuotaDenial(ctx, username, model); err != nil {
				slog.Warn("quota denial record failed", "error", err, "user", username)
			}
		}()
	}
}
