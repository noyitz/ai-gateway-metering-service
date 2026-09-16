package handler

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/noyitz/ai-gateway-metering-service/internal/config"
)

func TestServeWelcomeSubstitution(t *testing.T) {
	h := NewDashboardHandler(nil, config.Config{
		Welcome: config.Welcome{
			UnifiedURL:   "https://unified.test",
			OpenAIURL:    "https://openai.test/v1",
			DashboardURL: "https://dash.test",
		},
		MonthlyTokenQuota: 10_000_000_000,
	})
	req := httptest.NewRequest(http.MethodGet, "/welcome", nil)
	rec := httptest.NewRecorder()
	h.ServeWelcome(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"https://unified.test",
		"https://openai.test/v1",
		"https://dash.test",
		"10B monthly allowance",
		"Inferact/Qwen3.8-Flash-Next-NVFP4",
		"effortLevel",
		"Set up Hermes CLI",
		"~/.hermes/config.yaml",
		"missing API key",
		"extra_headers",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %q", want)
		}
	}
	// Configured URLs must suppress the example fallbacks, and no
	// placeholder may survive substitution.
	for _, absent := range []string{"gateway.example.com", "{{"} {
		if strings.Contains(body, absent) {
			t.Errorf("page unexpectedly contains %q", absent)
		}
	}
}

var anyPlaceholder = regexp.MustCompile(`\{\{[A-Z_]+\}\}`)

func TestServeWelcomeFallbacks(t *testing.T) {
	h := NewDashboardHandler(nil, config.Config{MonthlyTokenQuota: 100_000_000})
	req := httptest.NewRequest(http.MethodGet, "/welcome", nil)
	rec := httptest.NewRecorder()
	h.ServeWelcome(rec, req)

	body := rec.Body.String()
	if m := anyPlaceholder.FindString(body); m != "" {
		t.Errorf("unsubstituted placeholder %s in served page", m)
	}
	for _, want := range []string{
		welcomeUnifiedFallback, welcomeOpenAIFallback, welcomeDashboardFallback,
		"100M monthly allowance",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("fallback page missing %q", want)
		}
	}
}

func TestQuotaLabel(t *testing.T) {
	cases := map[float64]string{
		10_000_000_000: "10B",
		100_000_000:    "100M",
		1_500_000:      "1.5M",
		20_000:         "20K",
		999:            "999",
	}
	for in, want := range cases {
		if got := quotaLabel(in); got != want {
			t.Errorf("quotaLabel(%v) = %q, want %q", in, got, want)
		}
	}
}
