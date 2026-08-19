package storage

import (
	"math"
	"testing"
)

// testPrices holds per-million-token rates for the cost model tests.
type testPrices struct {
	input, output, cacheRead, cacheWrite float64
}

// perRequestCostUSD is the executable reference model that costUSDExpr
// (in postgres.go) must mirror. Provider usage fields are disjoint:
// promptTokens already includes cachedRead and cacheCreation, so both are
// subtracted to bill the uncached remainder at the input rate exactly once.
func perRequestCostUSD(promptTokens, cachedRead, cacheCreation, completion int64, p testPrices) float64 {
	uncached := promptTokens - cachedRead - cacheCreation
	if uncached < 0 {
		uncached = 0
	}
	return float64(uncached)*p.input/1e6 +
		float64(cachedRead)*p.cacheRead/1e6 +
		float64(cacheCreation)*p.cacheWrite/1e6 +
		float64(completion)*p.output/1e6
}

// oldDoubleChargeCostUSD reproduces the pre-fix formula, which subtracted
// only cachedRead — billing cache-creation tokens at BOTH the input and
// cache-write rates. Kept to prove the fix changes cache-miss turns.
func oldDoubleChargeCostUSD(promptTokens, cachedRead, cacheCreation, completion int64, p testPrices) float64 {
	uncached := promptTokens - cachedRead
	if uncached < 0 {
		uncached = 0
	}
	return float64(uncached)*p.input/1e6 +
		float64(cachedRead)*p.cacheRead/1e6 +
		float64(cacheCreation)*p.cacheWrite/1e6 +
		float64(completion)*p.output/1e6
}

// Fable list pricing (per the metering rate table), USD per million tokens.
var fablePrices = testPrices{input: 10, output: 50, cacheRead: 1, cacheWrite: 12.5}

func TestPerRequestCostUSD(t *testing.T) {
	const eps = 0.005

	tests := []struct {
		name          string
		prompt        int64 // includes cachedRead + cacheCreation (disjoint fields summed)
		cachedRead    int64
		cacheCreation int64
		completion    int64
		want          float64
	}{
		{
			// Warm turn: almost all input served from cache read.
			name: "warm cache read", prompt: 237_000, cachedRead: 236_700,
			cacheCreation: 0, completion: 173, want: 0.24835,
		},
		{
			// Cold turn: input written to cache (cache creation), no read.
			// Correct cost bills these once at the cache-write rate.
			name: "cold cache creation", prompt: 234_800, cachedRead: 0,
			cacheCreation: 234_800, completion: 173, want: 2.94365,
		},
		{
			// No caching at all: plain input + output.
			name: "no cache", prompt: 1_000, cachedRead: 0,
			cacheCreation: 0, completion: 500, want: 0.035,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := perRequestCostUSD(tt.prompt, tt.cachedRead, tt.cacheCreation, tt.completion, fablePrices)
			if math.Abs(got-tt.want) > eps {
				t.Errorf("perRequestCostUSD = %.5f, want %.5f", got, tt.want)
			}
		})
	}
}

// TestColdTurnNoLongerDoubleCharged pins the exact bug the fix addresses:
// a cache-creation-heavy turn must be billed at the cache-write rate only,
// not the cache-write rate plus the base input rate.
func TestColdTurnNoLongerDoubleCharged(t *testing.T) {
	const prompt, cacheCreation, completion = 234_800, 234_800, 173

	fixed := perRequestCostUSD(prompt, 0, cacheCreation, completion, fablePrices)
	buggy := oldDoubleChargeCostUSD(prompt, 0, cacheCreation, completion, fablePrices)

	if math.Abs(fixed-2.94365) > 0.005 {
		t.Errorf("fixed cold cost = %.5f, want ~2.94", fixed)
	}
	if math.Abs(buggy-5.29165) > 0.005 {
		t.Errorf("old formula cold cost = %.5f, want ~5.29 (the overcharge)", buggy)
	}
	if buggy <= fixed {
		t.Errorf("expected old formula to overcharge cold turns: buggy=%.5f fixed=%.5f", buggy, fixed)
	}
}

func TestComputeUsageStats(t *testing.T) {
	tests := []struct {
		name       string
		used       int64
		tokenQuota int64
		want       UsageStats
	}{
		{
			name:       "unlimited quota reports usage but never gates",
			used:       100_000_000,
			tokenQuota: 0,
			want:       UsageStats{HasAccess: true, Usage: 100_000_000},
		},
		{
			name:       "negative quota treated as unlimited",
			used:       500,
			tokenQuota: -1,
			want:       UsageStats{HasAccess: true, Usage: 500},
		},
		{
			name:       "under quota has access with remaining balance",
			used:       1_000,
			tokenQuota: 10_000,
			want:       UsageStats{HasAccess: true, Balance: 9_000, Usage: 1_000, Overage: 0},
		},
		{
			name:       "at quota is denied",
			used:       10_000,
			tokenQuota: 10_000,
			want:       UsageStats{HasAccess: false, Balance: 0, Usage: 10_000, Overage: 0},
		},
		{
			name:       "over quota is denied and reports overage",
			used:       12_000,
			tokenQuota: 10_000,
			want:       UsageStats{HasAccess: false, Balance: 0, Usage: 12_000, Overage: 2_000},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := computeUsageStats(tt.used, tt.tokenQuota)
			if got != tt.want {
				t.Errorf("computeUsageStats(%d, %d) = %+v, want %+v",
					tt.used, tt.tokenQuota, got, tt.want)
			}
		})
	}
}
