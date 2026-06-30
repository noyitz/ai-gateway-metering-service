package handler

import (
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/noyitz/ai-gateway-metering-service/internal/dashboard"
	"github.com/noyitz/ai-gateway-metering-service/internal/storage"
)

type DashboardHandler struct {
	store *storage.Store
}

func NewDashboardHandler(store *storage.Store) *DashboardHandler {
	return &DashboardHandler{store: store}
}

func (h *DashboardHandler) ServeDashboard(w http.ResponseWriter, r *http.Request) {
	user := r.Header.Get("X-Forwarded-User")
	page := "dashboard.html"
	if user != "" && user != "admin" {
		page = "user-dashboard.html"
	}
	data, err := fs.ReadFile(dashboard.FS, page)
	if err != nil {
		data, _ = fs.ReadFile(dashboard.FS, "dashboard.html")
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(data)
}

func (h *DashboardHandler) HandleOverview(w http.ResponseWriter, r *http.Request) {
	since := parseTimeRange(r)
	result, err := h.store.GetDashboardOverview(r.Context(), since)
	if err != nil {
		slog.Error("dashboard query failed", "error", err); http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, result)
}

func (h *DashboardHandler) HandleGroups(w http.ResponseWriter, r *http.Request) {
	since := parseTimeRange(r)
	result, err := h.store.GetDashboardGroups(r.Context(), since)
	if err != nil {
		slog.Error("dashboard query failed", "error", err); http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if result == nil {
		result = []storage.GroupSummary{}
	}
	writeJSON(w, result)
}

func (h *DashboardHandler) HandleUsers(w http.ResponseWriter, r *http.Request) {
	since := parseTimeRange(r)
	group := r.URL.Query().Get("group")
	user := r.URL.Query().Get("user")
	model := r.URL.Query().Get("model")
	sortCol := r.URL.Query().Get("sort")
	sortOrder := r.URL.Query().Get("order")
	limit := 100
	if l, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && l > 0 {
		limit = l
	}
	result, err := h.store.GetDashboardUsers(r.Context(), since, group, user, model, sortCol, sortOrder, limit)
	if err != nil {
		slog.Error("dashboard query failed", "error", err); http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if result == nil {
		result = []storage.UserSummary{}
	}
	writeJSON(w, result)
}

func (h *DashboardHandler) HandleModels(w http.ResponseWriter, r *http.Request) {
	since := parseTimeRange(r)
	user := r.URL.Query().Get("user")
	result, err := h.store.GetDashboardModels(r.Context(), since, user)
	if err != nil {
		slog.Error("dashboard query failed", "error", err); http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if result == nil {
		result = []storage.ModelSummary{}
	}
	writeJSON(w, result)
}

func (h *DashboardHandler) HandleTimeline(w http.ResponseWriter, r *http.Request) {
	since := parseTimeRange(r)
	groupBy := r.URL.Query().Get("group_by")
	if groupBy != "user" {
		groupBy = "model"
	}
	result, err := h.store.GetDashboardTimeline(r.Context(), since, groupBy)
	if err != nil {
		slog.Error("dashboard query failed", "error", err); http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if result == nil {
		result = []storage.TimelineBucket{}
	}
	writeJSON(w, result)
}

func (h *DashboardHandler) HandleRecent(w http.ResponseWriter, r *http.Request) {
	limit := 20
	if l, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && l > 0 {
		limit = l
	}
	result, err := h.store.GetRecentEvents(r.Context(), limit)
	if err != nil {
		slog.Error("dashboard query failed", "error", err); http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if result == nil {
		result = []storage.RecentEvent{}
	}
	writeJSON(w, result)
}

func parseTimeRange(r *http.Request) time.Time {
	switch r.URL.Query().Get("range") {
	case "24h":
		return time.Now().Add(-24 * time.Hour)
	case "30d":
		return time.Now().Add(-30 * 24 * time.Hour)
	default:
		return time.Now().Add(-7 * 24 * time.Hour)
	}
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	json.NewEncoder(w).Encode(v)
}
