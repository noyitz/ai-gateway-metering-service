package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/noyitz/ai-gateway-metering-service/internal/config"
	"github.com/noyitz/ai-gateway-metering-service/internal/storage"
)

// TestOrgChartsScopedToSubtree guards the team page's analytics endpoint.
// The regression it pins: /manager used to borrow the self-scoped shared
// dashboard endpoints, so a manager with a busy team got an empty timeline
// (and a 403 the moment the drill-down named a teammate's usernames).
// Charts must therefore answer from the caller's subtree — and only it:
// whole team, drilled branch, 403 outside, self for a non-manager.
func TestOrgChartsScopedToSubtree(t *testing.T) {
	store, ctx := openClaimTestStore(t)

	mgr, err := store.CreatePerson(ctx, storage.Person{FullName: "Mia Manager", Email: "mgr@x.com", GroupName: "octo-eng", Active: true}, "test")
	if err != nil {
		t.Fatalf("create manager: %v", err)
	}
	rep, err := store.CreatePerson(ctx, storage.Person{FullName: "Ray Report", Email: "rep@x.com", GroupName: "octo-eng", Active: true, ManagerSlug: mgr.Slug}, "test")
	if err != nil {
		t.Fatalf("create report: %v", err)
	}
	other, err := store.CreatePerson(ctx, storage.Person{FullName: "Olivia Other", Email: "oth@x.com", GroupName: "octo-eng", Active: true}, "test")
	if err != nil {
		t.Fatalf("create other: %v", err)
	}
	for _, p := range []storage.Person{mgr, rep, other} {
		if err := store.LinkIdentity(ctx, p.Email, p.Slug, "test"); err != nil {
			t.Fatalf("link %s: %v", p.Email, err)
		}
	}
	now := time.Now()
	for i, u := range []string{"mgr@x.com", "rep@x.com", "oth@x.com"} {
		if err := store.InsertEvent(ctx, storage.UsageEvent{
			EventID: "chart-" + u, Username: u, GroupName: "octo-eng", Provider: "anthropic",
			Model: "claude-sonnet-4-6", PromptTokens: 100, CompletionTokens: 50,
			TotalTokens: 150, Timestamp: now.Add(-time.Duration(i) * time.Hour),
		}); err != nil {
			t.Fatalf("insert event for %s: %v", u, err)
		}
	}

	cfg := config.Config{UserHeader: "X-Test-User"}
	h := NewOrgHandler(store, cfg, nil)
	charts := func(user, query string) (int, string, struct {
		Overview storage.DashboardOverview `json:"overview"`
		Models   []storage.ModelSummary    `json:"models"`
		Users    []storage.UserSummary     `json:"users"`
		Timeline []storage.TimelineBucket  `json:"timeline"`
	}) {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/org/charts?"+query, nil)
		r.Header.Set("X-Test-User", user)
		w := httptest.NewRecorder()
		h.HandleOrgCharts(w, r)
		var out struct {
			Overview storage.DashboardOverview `json:"overview"`
			Models   []storage.ModelSummary    `json:"models"`
			Users    []storage.UserSummary     `json:"users"`
			Timeline []storage.TimelineBucket  `json:"timeline"`
		}
		if w.Code == http.StatusOK {
			if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
				t.Fatalf("decode charts body %s: %v", w.Body.String(), err)
			}
		}
		return w.Code, w.Body.String(), out
	}
	seen := func(out struct {
		Overview storage.DashboardOverview `json:"overview"`
		Models   []storage.ModelSummary    `json:"models"`
		Users    []storage.UserSummary     `json:"users"`
		Timeline []storage.TimelineBucket  `json:"timeline"`
	}) string {
		var s []string
		for _, u := range out.Users {
			s = append(s, u.Username)
		}
		for _, b := range out.Timeline {
			s = append(s, b.Series)
		}
		return strings.Join(s, ",")
	}

	// Whole team: manager + report, never the outsider.
	code, body, out := charts("mgr@x.com", "range=7d")
	if code != http.StatusOK {
		t.Fatalf("manager whole-team: code=%d body=%s", code, body)
	}
	if out.Overview.TotalRequests != 2 {
		t.Fatalf("manager whole-team requests=%d, want 2 (subtree only, not all 3)", out.Overview.TotalRequests)
	}
	if got := seen(out); strings.Contains(got, "oth@x.com") || !strings.Contains(got, "rep@x.com") {
		t.Fatalf("manager whole-team leaked or missed users: %q", got)
	}

	// Drill into the report's branch: only the report's numbers.
	code, body, out = charts("mgr@x.com", "range=7d&root="+rep.Slug)
	if code != http.StatusOK {
		t.Fatalf("drill: code=%d body=%s", code, body)
	}
	if out.Overview.TotalRequests != 1 || !strings.Contains(seen(out), "rep@x.com") {
		t.Fatalf("drill to %s: requests=%d seen=%q, want only the report", rep.Slug, out.Overview.TotalRequests, seen(out))
	}

	// A branch outside the caller's tree: 403, not an empty success.
	code, _, _ = charts("mgr@x.com", "range=7d&root="+other.Slug)
	if code != http.StatusForbidden {
		t.Fatalf("outside root: code=%d, want 403", code)
	}

	// Non-manager with a directory entry: exactly their own numbers.
	code, body, out = charts("oth@x.com", "range=7d")
	if code != http.StatusOK {
		t.Fatalf("non-manager: code=%d body=%s", code, body)
	}
	if out.Overview.TotalRequests != 1 || !strings.Contains(seen(out), "oth@x.com") {
		t.Fatalf("non-manager scope: requests=%d seen=%q, want only self", out.Overview.TotalRequests, seen(out))
	}
}
