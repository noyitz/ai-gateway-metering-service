package handler

import (
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/noyitz/ai-gateway-metering-service/internal/config"
	"github.com/noyitz/ai-gateway-metering-service/internal/dashboard"
	"github.com/noyitz/ai-gateway-metering-service/internal/storage"
)

type DashboardHandler struct {
	store *storage.Store
	cfg   config.Config
}

func NewDashboardHandler(store *storage.Store, cfg config.Config) *DashboardHandler {
	return &DashboardHandler{store: store, cfg: cfg}
}

func (h *DashboardHandler) ServeDashboard(w http.ResponseWriter, r *http.Request) {
	data, err := fs.ReadFile(dashboard.FS, "dashboard.html")
	if err != nil {
		http.Error(w, "dashboard not found", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(data)
}

func (h *DashboardHandler) HandleOverview(w http.ResponseWriter, r *http.Request) {
	since, until := parseTimeWindow(r)
	group, user, model := parseFilters(r)
	var ok bool
	if user, ok = ApplyScope(w, r, h.store, h.cfg, user); !ok {
		return
	}
	result, err := h.store.GetDashboardOverview(r.Context(), since, until, group, user, model)
	if err != nil {
		slog.Error("dashboard query failed", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	// "Saved · Hosted Models" KPI rides along server-side: the same CTE the
	// user table uses, so the card is the column's sum by construction.
	// A savings-query failure leaves the fields at zero rather than failing
	// the whole overview — cost/token KPIs still render.
	saved, ratio, applied, serr := h.store.GetHostedSavings(r.Context(), since, until, group, user, model, r.URL.Query().Get("ref"))
	if serr != nil {
		slog.Error("hosted savings query failed", "error", serr)
	} else {
		result.SavedUSD, result.SavingsRatio, result.RatioApplied = saved, ratio, applied
	}
	writeJSON(w, result)
}

func (h *DashboardHandler) HandleGroups(w http.ResponseWriter, r *http.Request) {
	since, until := parseTimeWindow(r)
	group, user, model := parseFilters(r)
	var ok bool
	if user, ok = ApplyScope(w, r, h.store, h.cfg, user); !ok {
		return
	}
	result, err := h.store.GetDashboardGroups(r.Context(), since, until, group, user, model)
	if err != nil {
		slog.Error("dashboard query failed", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if result == nil {
		result = []storage.GroupSummary{}
	}
	writeJSON(w, result)
}

func (h *DashboardHandler) HandleUsers(w http.ResponseWriter, r *http.Request) {
	since, until := parseTimeWindow(r)
	group, user, model := parseFilters(r)
	var ok bool
	if user, ok = ApplyScope(w, r, h.store, h.cfg, user); !ok {
		return
	}
	sortCol := r.URL.Query().Get("sort")
	sortOrder := r.URL.Query().Get("order")
	limit := 100
	if l, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && l > 0 {
		limit = l
	}
	// ref selects the reference model for the SavedUSD counterfactual; the
	// dashboard passes its savings-ref selection so the user-table column
	// and the Saved · Hosted Models KPI stay in lockstep.
	result, err := h.store.GetDashboardUsers(r.Context(), since, until, group, user, model, sortCol, sortOrder, limit, r.URL.Query().Get("ref"))
	if err != nil {
		slog.Error("dashboard query failed", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if result == nil {
		result = []storage.UserSummary{}
	}
	writeJSON(w, result)
}

func (h *DashboardHandler) HandleModels(w http.ResponseWriter, r *http.Request) {
	since, until := parseTimeWindow(r)
	group, user, model := parseFilters(r)
	var ok bool
	if user, ok = ApplyScope(w, r, h.store, h.cfg, user); !ok {
		return
	}
	result, err := h.store.GetDashboardModels(r.Context(), since, until, group, user, model)
	if err != nil {
		slog.Error("dashboard query failed", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if result == nil {
		result = []storage.ModelSummary{}
	}
	writeJSON(w, result)
}

func (h *DashboardHandler) HandleTimeline(w http.ResponseWriter, r *http.Request) {
	since, until := parseTimeWindow(r)
	group, user, model := parseFilters(r)
	var ok bool
	if user, ok = ApplyScope(w, r, h.store, h.cfg, user); !ok {
		return
	}
	groupBy := r.URL.Query().Get("group_by")
	if groupBy != "user" {
		groupBy = "model"
	}
	result, err := h.store.GetDashboardTimeline(r.Context(), since, until, group, user, model, groupBy)
	if err != nil {
		slog.Error("dashboard query failed", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
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
	group, user, model := parseFilters(r)
	var ok bool
	if user, ok = ApplyScope(w, r, h.store, h.cfg, user); !ok {
		return
	}
	result, err := h.store.GetRecentEvents(r.Context(), limit, group, user, model)
	if err != nil {
		slog.Error("dashboard query failed", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if result == nil {
		result = []storage.RecentEvent{}
	}
	writeJSON(w, result)
}

// parseTimeWindow resolves the [since, until) time window from the request.
// until defaults to now. A "custom" range reads from/to date params
// (YYYY-MM-DD); to is treated as inclusive end-of-day (to + 24h). Any
// missing/unparseable custom bound falls back to the last-7-days window.
func parseTimeWindow(r *http.Request) (since, until time.Time) {
	now := time.Now()
	switch r.URL.Query().Get("range") {
	case "24h":
		return now.Add(-24 * time.Hour), now
	case "mtd":
		// Month to date: calendar month, matching the monthly quota window.
		y, m, _ := now.Date()
		return time.Date(y, m, 1, 0, 0, 0, 0, now.Location()), now
	case "30d":
		return now.Add(-30 * 24 * time.Hour), now
	case "custom":
		const layout = "2006-01-02"
		from, errFrom := time.Parse(layout, r.URL.Query().Get("from"))
		to, errTo := time.Parse(layout, r.URL.Query().Get("to"))
		if errFrom != nil || errTo != nil {
			return now.Add(-7 * 24 * time.Hour), now
		}
		return from, to.Add(24 * time.Hour)
	default:
		return now.Add(-7 * 24 * time.Hour), now
	}
}

// parseFilters extracts the common group/user/model filter params. The user
// param may be a comma-separated list of usernames (multi-select); an empty
// value means no user filter. The storage layer matches it via string_to_array.
func parseFilters(r *http.Request) (group, user, model string) {
	return r.URL.Query().Get("group"), r.URL.Query().Get("user"), r.URL.Query().Get("model")
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	json.NewEncoder(w).Encode(v)
}
