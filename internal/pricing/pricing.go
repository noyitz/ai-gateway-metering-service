package pricing

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"time"

	_ "embed"
)

//go:embed model_prices.json
var bundledPricing []byte

const liteLLMRawURL = "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"

// ModelPrice represents pricing for a single model in our schema.
type ModelPrice struct {
	Model          string  `json:"model"`
	Provider       string  `json:"provider"`
	InputCost      float64 `json:"input_cost_per_mtok"`
	OutputCost     float64 `json:"output_cost_per_mtok"`
	CacheWriteCost float64 `json:"cache_write_cost_per_mtok"`
	CacheReadCost  float64 `json:"cache_read_cost_per_mtok"`
	// Vendor list price (per MTok) — what the model costs billed directly at
	// the vendor. Zero means "no list price" (absent from the response too,
	// so a list-only entry doesn't serialize as $0 and read as free).
	ListInputCost      float64 `json:"list_input_cost_per_mtok,omitempty"`
	ListOutputCost     float64 `json:"list_output_cost_per_mtok,omitempty"`
	ListCacheWriteCost float64 `json:"list_cache_write_cost_per_mtok,omitempty"`
	ListCacheReadCost  float64 `json:"list_cache_read_cost_per_mtok,omitempty"`
}

type liteLLMEntry struct {
	InputCostPerToken           float64 `json:"input_cost_per_token"`
	OutputCostPerToken          float64 `json:"output_cost_per_token"`
	CacheReadInputTokenCost     float64 `json:"cache_read_input_token_cost"`
	CacheCreationInputTokenCost float64 `json:"cache_creation_input_token_cost"`
	Provider                    string  `json:"litellm_provider"`
}

type bundledFile struct {
	Metadata struct {
		FetchedAt   string `json:"fetched_at"`
		TotalModels int    `json:"total_models"`
	} `json:"_metadata"`
	Models map[string]liteLLMEntry `json:"models"`
}

// relevantPrefixes defines which models to import from LiteLLM's full database.
var relevantPrefixes = []string{
	"claude-", "gpt-", "gemini-", "o1-", "o3-", "o4-",
}

var relevantProviderPrefixes = []string{
	"vertex_ai/", "anthropic/", "openai/", "google/",
}

// LoadPrices tries to fetch latest pricing from LiteLLM, falls back to bundled.
func LoadPrices(ctx context.Context) ([]ModelPrice, string) {
	fetched, err := FetchLatest(ctx)
	if err == nil {
		prices, parseErr := parseLiteLLMRaw(fetched)
		if parseErr == nil && len(prices) > 0 {
			return prices, "fetched"
		}
		slog.Warn("pricing: fetched file parse issue, falling back to bundled", "models", len(prices), "error", parseErr)
	} else {
		slog.Warn("pricing: fetch failed, using bundled pricing", "error", err)
	}

	prices, err := parseBundled(bundledPricing)
	if err != nil {
		slog.Warn("pricing: bundled parse failed", "error", err)
		return nil, "error"
	}
	return prices, "bundled"
}

// LocalPrices returns pricing for self-hosted / on-prem models that LiteLLM's
// catalog does not carry. These run on our own GPUs at no per-token cost, so
// they price at $0 and appear as "free" in the dashboard's savings view.
//
// They must be seeded explicitly for two reasons: LiteLLM never emits them (its
// $0 entries are dropped in entryToModelPrice), and without a model_pricing row
// the cost query falls back to the default paid rate (~$15/M in), silently
// turning free traffic into phantom spend. Seeding here makes the rows
// reproducible across DB rebuilds instead of relying on manual INSERTs.
//
// Model strings MUST match exactly what the gateway records in
// usage_events.model — model_pricing keys on the model name alone.
func LocalPrices() []ModelPrice {
	return []ModelPrice{
		// Self-hosted Qwen on vLLM — the model id billed traffic records.
		{Model: "Qwen3.8-27B-FP8", Provider: "vllm"},
		// Qwen3.8-Flash-Next on external cluster, proxied via qwen-flash-proxy.
		{Model: "Inferact/Qwen3.8-Flash-Next-NVFP4", Provider: "vllm"},
		// Legacy alias: older events recorded model="qwen" before the id above
		// was adopted. Kept at $0 so historical rows don't reprice to the paid
		// default. Safe to drop once no events with model="qwen" remain.
		{Model: "qwen", Provider: "qwen"},
	}
}

// LocalListPrices gives the self-hosted models a list baseline for the
// cost-saved column: the price of an equivalent hosted model. Qwen3.8-27B is
// a 27B MoE serving at ~$0, so the honest comparison is a hosted Sonnet-class
// model (Claude Sonnet list: $3 / $15, cache read $0.30, cache write $3.75
// per MTok). Without these, LocalPrices()'s $0 rows have no list price and
// the column would show $0 savings for the traffic that actually saves money.
func LocalListPrices() []ModelPrice {
	sonnetList := ModelPrice{
		Provider:           "anthropic",
		ListInputCost:      3.0,
		ListOutputCost:     15.0,
		ListCacheReadCost:  0.3,
		ListCacheWriteCost: 3.75,
	}
	var out []ModelPrice
	for _, m := range LocalPrices() {
		p := sonnetList
		p.Model = m.Model
		out = append(out, p)
	}
	return out
}

// listListedProviders are the vendors where the vendor-direct list rate is
// the meaningful baseline for the cost-saved column (we route Claude and
// GPT traffic; Gemini goes through Vertex whose rate already matches the
// vendor's public price, so a Gemini "savings" would be noise).
var listListedProviders = map[string]bool{
	"anthropic": true,
	"openai":    true,
}

// LoadListPrices fetches the same LiteLLM file as LoadPrices but extracts the
// vendor-native list rates: bare-key entries (no provider prefix) whose
// litellm_provider is a listed vendor. Reseller routes (vertex_ai/, azure/,
// bedrock region variants) are deliberately excluded — a reseller's price is
// not the vendor's list price.
func LoadListPrices(ctx context.Context) ([]ModelPrice, error) {
	data, err := FetchLatest(ctx)
	if err != nil {
		return nil, err
	}
	return parseListRaw(data)
}

func parseListRaw(data []byte) ([]ModelPrice, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("unmarshal raw: %w", err)
	}

	seen := make(map[string]ModelPrice)
	for key, rawEntry := range raw {
		if key == "sample_spec" {
			continue
		}
		// Vendor-native entries are bare keys; any "/" means a reseller route.
		if strings.Contains(key, "/") {
			continue
		}
		var entry liteLLMEntry
		if err := json.Unmarshal(rawEntry, &entry); err != nil {
			continue
		}
		if !listListedProviders[normalizeProvider(entry.Provider)] {
			continue
		}
		if !isRelevantModel(key) {
			continue
		}
		if entry.InputCostPerToken == 0 && entry.OutputCostPerToken == 0 {
			continue
		}
		seen[key] = ModelPrice{
			Model:              key,
			Provider:           normalizeProvider(entry.Provider),
			ListInputCost:      entry.InputCostPerToken * 1e6,
			ListOutputCost:     entry.OutputCostPerToken * 1e6,
			ListCacheWriteCost: entry.CacheCreationInputTokenCost * 1e6,
			ListCacheReadCost:  entry.CacheReadInputTokenCost * 1e6,
		}
	}

	prices := make([]ModelPrice, 0, len(seen))
	for _, p := range seen {
		prices = append(prices, p)
	}
	return prices, nil
}

// FetchLatest downloads the current LiteLLM pricing file from GitHub.
func FetchLatest(ctx context.Context) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, liteLLMRawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status: %d", resp.StatusCode)
	}

	const maxResponseBytes = 10 << 20 // 10 MB
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	return data, nil
}

// RefreshPrices fetches latest and returns the parsed prices with a diff.
func RefreshPrices(ctx context.Context, current []ModelPrice) (updated []ModelPrice, changed []string, source string, err error) {
	fetched, fetchErr := FetchLatest(ctx)
	if fetchErr != nil {
		return nil, nil, "bundled (fetch failed)", fetchErr
	}

	prices, parseErr := parseLiteLLMRaw(fetched)
	if parseErr != nil {
		return nil, nil, "bundled (parse failed)", parseErr
	}

	currentMap := make(map[string]ModelPrice, len(current))
	for _, p := range current {
		currentMap[p.Model] = p
	}

	for _, p := range prices {
		if existing, ok := currentMap[p.Model]; ok {
			if !pricesEqual(existing, p) {
				changed = append(changed, p.Model)
			}
		} else {
			changed = append(changed, p.Model+" (new)")
		}
	}

	return prices, changed, "fetched", nil
}

func pricesEqual(a, b ModelPrice) bool {
	const epsilon = 0.001
	return math.Abs(a.InputCost-b.InputCost) < epsilon &&
		math.Abs(a.OutputCost-b.OutputCost) < epsilon &&
		math.Abs(a.CacheReadCost-b.CacheReadCost) < epsilon &&
		math.Abs(a.CacheWriteCost-b.CacheWriteCost) < epsilon
}

func parseBundled(data []byte) ([]ModelPrice, error) {
	var f bundledFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("unmarshal bundled: %w", err)
	}

	var prices []ModelPrice
	for key, entry := range f.Models {
		p := entryToModelPrice(key, entry)
		if p.Model != "" {
			prices = append(prices, p)
		}
	}
	return prices, nil
}

func parseLiteLLMRaw(data []byte) ([]ModelPrice, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("unmarshal raw: %w", err)
	}

	seen := make(map[string]ModelPrice)
	for key, rawEntry := range raw {
		if key == "sample_spec" {
			continue
		}
		if !isRelevantModel(key) {
			continue
		}

		var entry liteLLMEntry
		if err := json.Unmarshal(rawEntry, &entry); err != nil {
			continue
		}

		p := entryToModelPrice(key, entry)
		if p.Model == "" {
			continue
		}

		if _, ok := seen[p.Model]; ok {
			if strings.Contains(key, "/") {
				continue
			}
		}
		seen[p.Model] = p
	}

	prices := make([]ModelPrice, 0, len(seen))
	for _, p := range seen {
		prices = append(prices, p)
	}
	return prices, nil
}

func entryToModelPrice(key string, entry liteLLMEntry) ModelPrice {
	model := key
	provider := normalizeProvider(entry.Provider)

	// For provider-prefixed keys like "vertex_ai/claude-opus-4-8",
	// extract the model name and set provider from the prefix.
	for _, prefix := range relevantProviderPrefixes {
		if strings.HasPrefix(key, prefix) {
			model = strings.TrimPrefix(key, prefix)
			if provider == "" {
				provider = normalizeProvider(strings.TrimSuffix(prefix, "/"))
			}
			break
		}
	}

	if entry.InputCostPerToken == 0 && entry.OutputCostPerToken == 0 {
		return ModelPrice{}
	}

	return ModelPrice{
		Model:          model,
		Provider:       provider,
		InputCost:      entry.InputCostPerToken * 1e6,
		OutputCost:     entry.OutputCostPerToken * 1e6,
		CacheWriteCost: entry.CacheCreationInputTokenCost * 1e6,
		CacheReadCost:  entry.CacheReadInputTokenCost * 1e6,
	}
}

func normalizeProvider(p string) string {
	switch strings.ToLower(p) {
	case "vertex_ai", "vertex_ai_claude", "vertex_ai_anthropic":
		return "vertex"
	case "anthropic":
		return "anthropic"
	case "openai":
		return "openai"
	case "google", "gemini":
		return "vertex"
	default:
		if p == "" {
			return "unknown"
		}
		return strings.ToLower(p)
	}
}

func isRelevantModel(key string) bool {
	name := key
	for _, prefix := range relevantProviderPrefixes {
		name = strings.TrimPrefix(name, prefix)
	}
	for _, prefix := range relevantPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}
