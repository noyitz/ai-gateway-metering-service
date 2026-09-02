package pricing

import (
	"context"
	"encoding/json"
	"math"
	"testing"
)

func TestParseBundled(t *testing.T) {
	prices, err := parseBundled(bundledPricing)
	if err != nil {
		t.Fatalf("parseBundled: %v", err)
	}
	if len(prices) == 0 {
		t.Fatal("parseBundled returned 0 prices")
	}

	// Check that known models exist with correct pricing
	found := make(map[string]ModelPrice)
	for _, p := range prices {
		found[p.Model] = p
	}

	tests := []struct {
		model      string
		wantInput  float64
		wantOutput float64
	}{
		{"claude-opus-4-8", 5.0, 25.0},
		{"claude-sonnet-4-6", 3.0, 15.0},
		{"gpt-5.5", 5.0, 30.0},
	}
	for _, tt := range tests {
		p, ok := found[tt.model]
		if !ok {
			t.Errorf("model %q not found in bundled pricing", tt.model)
			continue
		}
		if math.Abs(p.InputCost-tt.wantInput) > 0.01 {
			t.Errorf("%s input: got %.2f, want %.2f", tt.model, p.InputCost, tt.wantInput)
		}
		if math.Abs(p.OutputCost-tt.wantOutput) > 0.01 {
			t.Errorf("%s output: got %.2f, want %.2f", tt.model, p.OutputCost, tt.wantOutput)
		}
	}
}

func TestParseLiteLLMRaw(t *testing.T) {
	raw := map[string]liteLLMEntry{
		"claude-opus-4-8": {
			InputCostPerToken:           0.000005,
			OutputCostPerToken:          0.000025,
			CacheReadInputTokenCost:     0.0000005,
			CacheCreationInputTokenCost: 0.00000625,
			Provider:                    "anthropic",
		},
		"vertex_ai/claude-sonnet-4-6": {
			InputCostPerToken:  0.000003,
			OutputCostPerToken: 0.000015,
			Provider:           "vertex_ai",
		},
		"some-irrelevant-model": {
			InputCostPerToken:  0.001,
			OutputCostPerToken: 0.002,
			Provider:           "unknown",
		},
		"sample_spec": {},
	}
	data, _ := json.Marshal(raw)

	prices, err := parseLiteLLMRaw(data)
	if err != nil {
		t.Fatalf("parseLiteLLMRaw: %v", err)
	}

	found := make(map[string]ModelPrice)
	for _, p := range prices {
		found[p.Model] = p
	}

	// claude-opus-4-8 should be present
	if p, ok := found["claude-opus-4-8"]; !ok {
		t.Error("claude-opus-4-8 not found")
	} else {
		if math.Abs(p.InputCost-5.0) > 0.01 {
			t.Errorf("opus input: got %.2f, want 5.00", p.InputCost)
		}
		if math.Abs(p.OutputCost-25.0) > 0.01 {
			t.Errorf("opus output: got %.2f, want 25.00", p.OutputCost)
		}
		if math.Abs(p.CacheReadCost-0.5) > 0.01 {
			t.Errorf("opus cache_read: got %.2f, want 0.50", p.CacheReadCost)
		}
		if math.Abs(p.CacheWriteCost-6.25) > 0.01 {
			t.Errorf("opus cache_write: got %.2f, want 6.25", p.CacheWriteCost)
		}
		if p.Provider != "anthropic" {
			t.Errorf("opus provider: got %q, want anthropic", p.Provider)
		}
	}

	// vertex_ai/claude-sonnet-4-6 should be stored as "claude-sonnet-4-6" with vertex provider
	if p, ok := found["claude-sonnet-4-6"]; !ok {
		t.Error("claude-sonnet-4-6 not found (should strip vertex_ai/ prefix)")
	} else if p.Provider != "vertex" {
		t.Errorf("sonnet provider: got %q, want vertex", p.Provider)
	}

	// Irrelevant model should be filtered out
	if _, ok := found["some-irrelevant-model"]; ok {
		t.Error("irrelevant model should be filtered out")
	}

	// sample_spec should be skipped
	if _, ok := found["sample_spec"]; ok {
		t.Error("sample_spec should be skipped")
	}
}

func TestParseLiteLLMRaw_DedupPrefersCanonical(t *testing.T) {
	raw := map[string]liteLLMEntry{
		"claude-opus-4-8": {
			InputCostPerToken:  0.000005,
			OutputCostPerToken: 0.000025,
			Provider:           "anthropic",
		},
		"vertex_ai/claude-opus-4-8": {
			InputCostPerToken:  0.000005,
			OutputCostPerToken: 0.000025,
			Provider:           "vertex_ai",
		},
	}
	data, _ := json.Marshal(raw)

	prices, err := parseLiteLLMRaw(data)
	if err != nil {
		t.Fatalf("parseLiteLLMRaw: %v", err)
	}

	count := 0
	for _, p := range prices {
		if p.Model == "claude-opus-4-8" {
			count++
			if p.Provider != "anthropic" {
				t.Errorf("duplicate model should keep canonical provider, got %q", p.Provider)
			}
		}
	}
	if count != 1 {
		t.Errorf("expected 1 entry for claude-opus-4-8, got %d", count)
	}
}

func TestEntryToModelPrice_ZeroCost(t *testing.T) {
	p := entryToModelPrice("test-model", liteLLMEntry{})
	if p.Model != "" {
		t.Error("zero-cost entry should return empty ModelPrice")
	}
}

func TestNormalizeProvider(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"vertex_ai", "vertex"},
		{"vertex_ai_claude", "vertex"},
		{"anthropic", "anthropic"},
		{"openai", "openai"},
		{"google", "vertex"},
		{"", "unknown"},
		{"deepseek", "deepseek"},
	}
	for _, tt := range tests {
		got := normalizeProvider(tt.input)
		if got != tt.want {
			t.Errorf("normalizeProvider(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestIsRelevantModel(t *testing.T) {
	tests := []struct {
		key  string
		want bool
	}{
		{"claude-opus-4-8", true},
		{"gpt-5.5", true},
		{"gemini-2.0-flash", true},
		{"vertex_ai/claude-sonnet-4-6", true},
		{"some-random-model", false},
		{"llama-3", false},
	}
	for _, tt := range tests {
		got := isRelevantModel(tt.key)
		if got != tt.want {
			t.Errorf("isRelevantModel(%q) = %v, want %v", tt.key, got, tt.want)
		}
	}
}

func TestLoadPrices_UsesBundledWhenFetchFails(t *testing.T) {
	// Use a cancelled context to force fetch failure
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	prices, source := LoadPrices(ctx)
	if source != "bundled" {
		t.Errorf("source = %q, want bundled", source)
	}
	if len(prices) == 0 {
		t.Error("bundled fallback returned 0 prices")
	}
}

func TestPricesEqual(t *testing.T) {
	a := ModelPrice{InputCost: 5.0, OutputCost: 25.0, CacheReadCost: 0.5, CacheWriteCost: 6.25}
	b := ModelPrice{InputCost: 5.0, OutputCost: 25.0, CacheReadCost: 0.5, CacheWriteCost: 6.25}
	if !pricesEqual(a, b) {
		t.Error("identical prices should be equal")
	}

	c := ModelPrice{InputCost: 5.0, OutputCost: 30.0, CacheReadCost: 0.5, CacheWriteCost: 6.25}
	if pricesEqual(a, c) {
		t.Error("different prices should not be equal")
	}
}

func TestLocalPrices_ZeroCostAndExactIDs(t *testing.T) {
	prices := LocalPrices()
	if len(prices) == 0 {
		t.Fatal("LocalPrices returned no entries")
	}

	// The self-hosted Qwen id must match billed traffic exactly, or model_pricing
	// (keyed on model name) misses and the row reprices at the paid default.
	var haveQwen bool
	for _, p := range prices {
		if p.Model == "" {
			t.Errorf("local price with empty model: %+v", p)
		}
		if p.InputCost != 0 || p.OutputCost != 0 || p.CacheReadCost != 0 || p.CacheWriteCost != 0 {
			t.Errorf("local model %q must be $0, got %+v", p.Model, p)
		}
		if p.Model == "Qwen3.8-27B-FP8" {
			haveQwen = true
		}
	}
	if !haveQwen {
		t.Error(`missing self-hosted "Qwen3.8-27B-FP8" entry`)
	}
}

func TestParseListRaw_NativeEntriesOnly(t *testing.T) {
	raw := map[string]liteLLMEntry{
		// Vendor-native: bare key + native provider → list price kept.
		"claude-opus-4-8": {
			InputCostPerToken:           0.000005,
			OutputCostPerToken:          0.000025,
			CacheReadInputTokenCost:     0.0000005,
			CacheCreationInputTokenCost: 0.00000625,
			Provider:                    "anthropic",
		},
		"gpt-5.4": {
			InputCostPerToken:  0.00000175,
			OutputCostPerToken: 0.000014,
			Provider:           "openai",
		},
		// Reseller route: prefixed key → excluded (reseller price ≠ list).
		"vertex_ai/claude-sonnet-5": {
			InputCostPerToken:  0.000002,
			OutputCostPerToken: 0.00001,
			Provider:           "vertex_ai",
		},
		"azure/eu.gpt-5.2": {
			InputCostPerToken:  0.000001,
			OutputCostPerToken: 0.00001,
			Provider:           "azure",
		},
		// Bare key but non-listed (reseller) provider → excluded.
		"us.anthropic.claude-fable-5": {
			InputCostPerToken:  0.00001,
			OutputCostPerToken: 0.00005,
			Provider:           "bedrock_converse",
		},
		// Bare key, native provider, but not a model we track → excluded.
		"gemini-2.0-flash": {
			InputCostPerToken:  0.0000001,
			OutputCostPerToken: 0.0000004,
			Provider:           "google",
		},
		"sample_spec": {},
	}
	data, _ := json.Marshal(raw)

	prices, err := parseListRaw(data)
	if err != nil {
		t.Fatalf("parseListRaw: %v", err)
	}
	if len(prices) != 2 {
		t.Fatalf("expected 2 list entries, got %d: %+v", len(prices), prices)
	}

	found := make(map[string]ModelPrice)
	for _, p := range prices {
		found[p.Model] = p
	}

	p, ok := found["claude-opus-4-8"]
	if !ok {
		t.Fatal("claude-opus-4-8 not found")
	}
	if math.Abs(p.ListInputCost-5.0) > 0.01 || math.Abs(p.ListOutputCost-25.0) > 0.01 {
		t.Errorf("opus list: got %.2f/%.2f, want 5.00/25.00", p.ListInputCost, p.ListOutputCost)
	}
	if math.Abs(p.ListCacheReadCost-0.5) > 0.01 || math.Abs(p.ListCacheWriteCost-6.25) > 0.01 {
		t.Errorf("opus list cache: got %.2f/%.2f, want 0.50/6.25", p.ListCacheReadCost, p.ListCacheWriteCost)
	}
	if p.InputCost != 0 || p.OutputCost != 0 {
		t.Error("list entry must not carry actual (gateway) rates")
	}

	if _, ok := found["gpt-5.4"]; !ok {
		t.Error("gpt-5.4 not found")
	}
	for _, excluded := range []string{"claude-sonnet-5", "gpt-5.2", "claude-fable-5", "gemini-2.0-flash"} {
		if _, ok := found[excluded]; ok {
			t.Errorf("%q should be excluded from list prices", excluded)
		}
	}
}

func TestLocalListPrices_MirrorsLocalModels(t *testing.T) {
	l := LocalListPrices()
	local := LocalPrices()
	if len(l) != len(local) {
		t.Fatalf("LocalListPrices has %d entries, LocalPrices has %d", len(l), len(local))
	}
	for i := range local {
		if l[i].Model != local[i].Model {
			t.Errorf("entry %d: model %q, want %q", i, l[i].Model, local[i].Model)
		}
		if l[i].InputCost != 0 || l[i].OutputCost != 0 {
			t.Errorf("entry %d: list entry must not carry actual rates", i)
		}
		if l[i].ListInputCost == 0 || l[i].ListOutputCost == 0 {
			t.Errorf("entry %d: list entry needs non-zero list rates", i)
		}
	}
}
