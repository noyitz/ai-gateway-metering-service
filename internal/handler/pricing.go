package handler

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/noyitz/ai-gateway-metering-service/internal/pricing"
	"github.com/noyitz/ai-gateway-metering-service/internal/storage"
)

type PricingRefreshHandler struct {
	store *storage.Store
}

func NewPricingRefreshHandler(store *storage.Store) *PricingRefreshHandler {
	return &PricingRefreshHandler{store: store}
}

type refreshResponse struct {
	Updated     int      `json:"updated"`
	Total       int      `json:"total"`
	Source      string   `json:"source"`
	Changed     []string `json:"changed,omitempty"`
	ListUpdated int      `json:"list_updated,omitempty"`
	Error       string   `json:"error,omitempty"`
}

type pricingCatalogEntry struct {
	Model             string  `json:"model"`
	Provider          string  `json:"provider"`
	Hosted            bool    `json:"hosted"`
	InputCost         float64 `json:"input_cost_per_mtok"`
	OutputCost        float64 `json:"output_cost_per_mtok"`
	CacheReadCost     float64 `json:"cache_read_cost_per_mtok"`
	CacheWriteCost    float64 `json:"cache_write_cost_per_mtok"`
	ListInputCost     float64 `json:"list_input_cost_per_mtok,omitempty"`
	ListOutputCost    float64 `json:"list_output_cost_per_mtok,omitempty"`
	ListCacheReadCost float64 `json:"list_cache_read_cost_per_mtok,omitempty"`
	ListCacheWriteCost float64 `json:"list_cache_write_cost_per_mtok,omitempty"`
}

// HandleList serves the rate card behind the dashboard pricing modal. Any
// logged-in user may read it — these are the same per-token rates every
// request is billed at and contain no user data. Hosted rows come first;
// pass ?used=0 to include catalog models with no observed usage yet.
func (h *PricingRefreshHandler) HandleList(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	prices, err := h.store.GetPricingCatalog(r.Context(), r.URL.Query().Get("used") != "0")
	if err != nil {
		slog.Error("pricing catalog query failed", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	models := make([]pricingCatalogEntry, 0, len(prices))
	for _, p := range prices {
		models = append(models, pricingCatalogEntry{
			Model: p.Model, Provider: p.Provider,
			Hosted:         p.Provider == "vllm" || p.Provider == "qwen",
			InputCost:      p.InputCost,
			OutputCost:     p.OutputCost,
			CacheReadCost:  p.CacheReadCost,
			CacheWriteCost: p.CacheWriteCost,
			ListInputCost:  p.ListInputCost, ListOutputCost: p.ListOutputCost,
			ListCacheReadCost: p.ListCacheReadCost, ListCacheWriteCost: p.ListCacheWriteCost,
		})
	}
	json.NewEncoder(w).Encode(map[string]any{"models": models})
}

func (h *PricingRefreshHandler) HandleRefresh(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		json.NewEncoder(w).Encode(refreshResponse{Error: "POST required"})
		return
	}

	ctx := r.Context()

	// Get current pricing from DB for comparison
	currentDB, err := h.store.GetCurrentPricing(ctx)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(refreshResponse{Error: "failed to read current pricing"})
		return
	}

	currentPricing := make([]pricing.ModelPrice, len(currentDB))
	for i, p := range currentDB {
		currentPricing[i] = pricing.ModelPrice{
			Model: p.Model, Provider: p.Provider,
			InputCost: p.InputCost, OutputCost: p.OutputCost,
			CacheWriteCost: p.CacheWriteCost, CacheReadCost: p.CacheReadCost,
		}
	}

	// Fetch latest and diff
	prices, changed, source, fetchErr := pricing.RefreshPrices(ctx, currentPricing)
	if fetchErr != nil {
		json.NewEncoder(w).Encode(refreshResponse{
			Source: source,
			Error:  fetchErr.Error(),
		})
		return
	}

	// Upsert into DB
	storePrices := make([]storage.ModelPrice, len(prices))
	for i, p := range prices {
		storePrices[i] = storage.ModelPrice{
			Model: p.Model, Provider: p.Provider,
			InputCost: p.InputCost, OutputCost: p.OutputCost,
			CacheWriteCost: p.CacheWriteCost, CacheReadCost: p.CacheReadCost,
		}
	}

	updated, seedErr := h.store.SeedPricing(ctx, storePrices)
	if seedErr != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(refreshResponse{Error: "seed failed"})
		return
	}

	// List prices for the cost-saved column. Best-effort on top of the main
	// refresh: a list-price fetch failure logs but doesn't fail the refresh
	// (SeedListPricing is UPDATE-only, so a stale set stays in place).
	listUpdated := 0
	if listPrices, listErr := pricing.LoadListPrices(ctx); listErr != nil {
		slog.Warn("pricing refresh: list prices fetch failed — cost-saved column may be stale", "error", listErr)
	} else {
		listSeed := make([]storage.ModelPrice, 0, len(listPrices)+len(pricing.LocalListPrices()))
		for _, p := range append(append([]pricing.ModelPrice{}, listPrices...), pricing.LocalListPrices()...) {
			listSeed = append(listSeed, storage.ModelPrice{
				Model: p.Model, Provider: p.Provider,
				ListInputCost: p.ListInputCost, ListOutputCost: p.ListOutputCost,
				ListCacheWriteCost: p.ListCacheWriteCost, ListCacheReadCost: p.ListCacheReadCost,
			})
		}
		listUpdated, seedErr = h.store.SeedListPricing(ctx, listSeed)
		if seedErr != nil {
			slog.Warn("pricing refresh: list pricing seed failed", "error", seedErr)
		}
	}

	json.NewEncoder(w).Encode(refreshResponse{
		Updated:     updated,
		Total:       len(prices),
		Source:      source,
		Changed:     changed,
		ListUpdated: listUpdated,
	})
}
