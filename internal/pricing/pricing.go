package pricing

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
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
}

type liteLLMEntry struct {
	InputCostPerToken            float64 `json:"input_cost_per_token"`
	OutputCostPerToken           float64 `json:"output_cost_per_token"`
	CacheReadInputTokenCost     float64 `json:"cache_read_input_token_cost"`
	CacheCreationInputTokenCost float64 `json:"cache_creation_input_token_cost"`
	Provider                     string  `json:"litellm_provider"`
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
		log.Printf("pricing: fetched file parsed %d models (parse error: %v), falling back to bundled", len(prices), parseErr)
	} else {
		log.Printf("pricing: fetch failed (%v), using bundled pricing", err)
	}

	prices, err := parseBundled(bundledPricing)
	if err != nil {
		log.Printf("pricing: bundled parse failed: %v", err)
		return nil, "error"
	}
	return prices, "bundled"
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

	data, err := io.ReadAll(resp.Body)
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

	var prices []ModelPrice
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
		if p.Model != "" {
			prices = append(prices, p)
		}
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
