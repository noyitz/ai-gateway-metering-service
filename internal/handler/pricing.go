package handler

import (
	"encoding/json"
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
	Updated   int      `json:"updated"`
	Total     int      `json:"total"`
	Source    string   `json:"source"`
	Changed  []string `json:"changed,omitempty"`
	Error    string   `json:"error,omitempty"`
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
		json.NewEncoder(w).Encode(refreshResponse{Error: "failed to read current pricing: " + err.Error()})
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
		json.NewEncoder(w).Encode(refreshResponse{Error: "seed failed: " + seedErr.Error()})
		return
	}

	json.NewEncoder(w).Encode(refreshResponse{
		Updated: updated,
		Total:   len(prices),
		Source:  source,
		Changed: changed,
	})
}
