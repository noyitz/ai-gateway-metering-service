package handler

import (
	"net/http"
	"time"

	"github.com/noyitz/ai-gateway-metering-service/internal/storage"
)

// RollupHandler exposes Phase 3 state for the read-switch gate and the
// parity check, super-admin gated at registration.
type RollupHandler struct {
	store *storage.Store
}

func NewRollupHandler(store *storage.Store) *RollupHandler {
	return &RollupHandler{store: store}
}

// HandleStatus returns backfill readiness; with ?parity=<window> it also
// runs the raw-vs-rollup parity diff (total and per-model) for that
// window. The parity query scans raw aggregates — super-admin only, and
// the window is clamped to 90 days so it can't be weaponized into a
// full-table re-pricing on every page refresh.
func (h *RollupHandler) HandleStatus(w http.ResponseWriter, r *http.Request) {
	resp := map[string]any{
		"ready":          h.store.RollupsReady(),
		"use_rollups":    h.store.RollupFlag(),
		"parity_healthy": h.store.ParityHealthy(),
		"serving":        "raw",
	}
	if h.store.RollupServing() {
		resp["serving"] = "rollup"
	}
	if p := r.URL.Query().Get("parity"); p != "" {
		window, ok := parseTimeWindowNamed(p)
		if !ok {
			http.Error(w, "bad parity window (use 24h, 7d, 30d, 90d)", http.StatusBadRequest)
			return
		}
		until := time.Now()
		report, err := h.store.ParityReport(r.Context(), until.Add(-window), until)
		if err != nil {
			http.Error(w, "parity report failed", http.StatusInternalServerError)
			return
		}
		resp["parity_window"] = p
		resp["parity"] = report
	}
	writeJSON(w, resp)
}

func parseTimeWindowNamed(name string) (time.Duration, bool) {
	switch name {
	case "24h":
		return 24 * time.Hour, true
	case "7d":
		return 7 * 24 * time.Hour, true
	case "30d":
		return 30 * 24 * time.Hour, true
	case "90d":
		return 90 * 24 * time.Hour, true
	}
	return 0, false
}
