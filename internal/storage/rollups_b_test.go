package storage

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// Part B structural guards: the read switch may only widen the use of
// usage_hourly, never the cost math, never enforcement reads, and never
// the raw-side query text that the parity gate is defined against.

func readSource(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

// funcBody extracts a function's text by scanning to the next top-level
// func — enough to assert which tables/gates a reader references.
func funcBody(t *testing.T, file, sig string) string {
	t.Helper()
	src := readSource(t, file)
	i := strings.Index(src, sig)
	if i < 0 {
		t.Fatalf("%s not found in %s", sig, file)
	}
	rest := src[i+len(sig):]
	if j := strings.Index(rest, "\nfunc "); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

// The advisory-lock contract: both writers key the same bucket the same
// way, UTC-rendered so no session TimeZone can ever fork the key space.
func TestHourLockKeysRenderIdentically(t *testing.T) {
	for _, want := range []string{
		`hashtext('usage_hourly:' || to_char(`,
		`AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24')`,
	} {
		if !strings.Contains(upsertHourLockSQL, want) {
			t.Errorf("upsert hour lock lost the shared key expression %s — its lock would key a different bucket than the rebuild's and the race reopens", want)
		}
		if !strings.Contains(rebuildHourLockSQL, want) {
			t.Errorf("rebuild hour lock lost the shared key expression %s", want)
		}
	}
	if !strings.Contains(upsertHourLockSQL, "date_trunc('hour', $1::timestamptz)") {
		t.Error("upsert lock must key the EVENT's hour bucket")
	}
	if !strings.Contains(rebuildHourLockSQL, "ORDER BY h") {
		t.Error("rebuild lock must acquire its range in ascending hour order (deadlock-freedom)")
	}
}

func TestRebuildPathsAcquireHourLocks(t *testing.T) {
	for _, fn := range []string{"func (s *Store) rebuildStep", "func (s *Store) refreshRecentHours"} {
		body := funcBody(t, "rollups.go", fn)
		if !strings.Contains(body, "rebuildHourLockSQL") {
			t.Errorf("%s rewrites hours without the hour lock — periodic refresh would race live upserts", fn)
		}
		if strings.Index(body, "rebuildHourLockSQL") > strings.Index(body, "rebuildHourSQL") {
			t.Errorf("%s must lock BEFORE rewriting the hour", fn)
		}
	}
	if !strings.Contains(readSource(t, "rollups.go"), "func upsertRollup(") ||
		!strings.Contains(funcBody(t, "rollups.go", "func upsertRollup("), "upsertHourLockSQL") {
		t.Error("upsertRollup must take the hour lock before the ON CONFLICT increment")
	}
}

func TestRollupReadVariantsReadOnlyHourlyTable(t *testing.T) {
	variants := map[string]string{
		"rollupOverviewSQL": rollupOverviewSQL,
		"rollupGroupsSQL":   rollupGroupsSQL,
		"rollupTeamUsage":   rollupTeamUsageSQL,
		"rollupModelsSQL":   rollupModelsSQL,
		"rollupTimeline":    rollupTimelineSQL("day", "e.model"),
		"rollupUsersTail":   fmt.Sprintf(rollupUsersSelect, displayNameExpr, displayNameExpr, "total_tokens", "DESC"),
		"rollupSavings":     hostedSavingsWithSQL(6, true),
	}
	for name, q := range variants {
		if !strings.Contains(q, "usage_hourly") {
			t.Errorf("%s does not read usage_hourly", name)
		}
		if strings.Contains(q, "usage_events") {
			t.Errorf("%s must not touch usage_events — a rollup variant mixing both tables double-counts", name)
		}
		if strings.Contains(q, "e.timestamp") {
			t.Errorf("%s must window on e.hour, not a row timestamp", name)
		}
	}
	// Counts on an hourly row are N events: SUM(requests), never COUNT(*).
	for name, q := range variants {
		if strings.Contains(q, "COUNT(*)") {
			t.Errorf("%s counts hourly ROWS not requests — must aggregate SUM(requests)", name)
		}
	}
	// The rollup variants must not resurrect a second cost formula: every
	// costUSDExpr pricing term multiplies by "* COALESCE(p.*_cost_per_mtok".
	for name, q := range variants {
		if strings.Contains(q, "* COALESCE(p.") {
			t.Errorf("%s embeds live price arithmetic — rollups serve the stored cost_usd", name)
		}
	}
}

// The switch must not have touched the raw text the parity gate
// measures against: every switched reader still contains its raw
// usage_events query AND its gate, and the savings CTE's raw rendering is
// the pre-part-B text.
func TestSwitchedReadersKeepRawTextAndGate(t *testing.T) {
	switched := map[string]string{
		"func (s *Store) GetDashboardOverview": "rollupOverviewSQL",
		"func (s *Store) GetDashboardGroups":   "rollupGroupsSQL",
		"func (s *Store) GetTeamUsage":         "rollupTeamUsageSQL",
		"func (s *Store) GetDashboardModels":   "rollupModelsSQL",
		"func (s *Store) GetDashboardTimeline": "rollupTimelineSQL",
		"func (s *Store) GetDashboardUsers":    "rollupUsersSelect",
		"func (s *Store) GetHostedSavings":     "rollupsLive",
	}
	for fn, want := range switched {
		body := funcBody(t, "postgres.go", fn)
		if !strings.Contains(body, "FROM usage_events e") && fn != "func (s *Store) GetHostedSavings" {
			t.Errorf("%s lost its raw usage_events query — the raw path must stay byte-identical for parity", fn)
		}
		if !strings.Contains(body, "s.rollupsLive()") && !strings.Contains(body, "rollup := s.rollupsLive()") {
			t.Errorf("%s no longer consults rollupsLive() — the read switch is ungated", fn)
		}
		if !strings.Contains(body, want) {
			t.Errorf("%s does not reference its rollup variant %s", fn, want)
		}
	}
	raw := hostedSavingsWithSQL(7, false)
	if !strings.Contains(raw, "usage_events e LEFT JOIN model_pricing p ON e.model = p.model") ||
		!strings.Contains(raw, "e.timestamp >= $1 AND e.timestamp < $2") ||
		!strings.Contains(raw, costUSDExpr) {
		t.Error("hostedSavingsWithSQL(rollup=false) no longer renders the pre-part-B raw query")
	}
	if strings.Contains(hostedSavingsWithSQL(7, true), "qwen-%%") {
		t.Error("rollup savings variant leaked an un-rendered wildcard escape")
	}
	if !strings.Contains(hostedSavingsWithSQL(7, true), "qwen-%'") {
		t.Error("rollup savings variant lost the rendered hosted-provider wildcard")
	}
}

// Freshness-critical reads must never switch, whatever the flag says.
func TestFreshnessReadsNeverSwitch(t *testing.T) {
	for _, fn := range []struct{ file, sig string }{
		{"postgres.go", "func (s *Store) GetRecentEvents"},
		{"postgres.go", "func (s *Store) GetMonthlyUsage"},
		{"rollups.go", "func (s *Store) ParityReport"},
	} {
		body := funcBody(t, fn.file, fn.sig)
		if strings.Contains(body, "rollupsLive") || strings.Contains(body, "rollupOverview") || strings.Contains(body, "rollupUsers") {
			t.Errorf("%s references the rollup read switch — freshness/enforcement reads serve raw unconditionally", fn.sig)
		}
	}
	q := readSource(t, "quota.go")
	if strings.Contains(q, "rollupsLive") || strings.Contains(q, "rollupOverview") || strings.Contains(q, "rollupUsers") {
		t.Error("quota.go references the rollup read switch — enforcement decisions serve raw unconditionally")
	}
}

func TestRollupsLiveRequiresAllThreeConditions(t *testing.T) {
	var s Store
	if s.rollupsLive() {
		t.Fatal("zero-value store must not serve rollups")
	}
	s.useRollups.Store(true)
	if s.rollupsLive() {
		t.Fatal("flag alone must not serve rollups (no backfill, no parity)")
	}
	s.rollupsReadyNow.Store(true)
	if s.rollupsLive() {
		t.Fatal("flag+ready must not serve rollups before the first parity check goes green")
	}
	s.parityHealthy.Store(true)
	if !s.rollupsLive() {
		t.Fatal("flag+ready+parity must serve rollups")
	}
	s.useRollups.Store(false)
	if s.rollupsLive() {
		t.Fatal("the flag must remain the kill switch")
	}
}

// The standing check gates on REQUEST COUNTS only; a cost-only delta is
// a human note (a pricing change must not black out the table).
func TestParityGatesOnCountsAndReportsCostNotes(t *testing.T) {
	ps := ParityShape{RawRequests: 10, RollRequests: 11, RawCostUSD: 1, RollCostUSD: 2}
	if ps.Match {
		t.Error("count mismatch must be reported as not matching (the automatic gate)")
	}
	src := readSource(t, "rollups.go")
	if !strings.Contains(src, "Match: a.rawReq == a.rollReq") {
		t.Error("the automatic gate must compare counts only — cost drift is a note, not a red")
	}
	if !strings.Contains(src, "expected only if model_pricing changed inside the window") {
		t.Error("cost deltas must carry the human-readable note")
	}
	for _, shape := range []string{`"total"`, `"per_model"`, `"per_group"`, `"per_user"`, `"per_day"`} {
		if !strings.Contains(src, shape) {
			t.Errorf("standing parity lost shape %s — the switch is defined against all reader keyings", shape)
		}
	}
	if !strings.Contains(src, "LevelRepeatableRead") {
		t.Error("parity must run in one snapshot — two queries across a WAL replay boundary diff red on live traffic")
	}
}
