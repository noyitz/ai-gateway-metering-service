package storage

import (
	"strings"
	"testing"
)

// These guard the SQL STRUCTURE of the Phase 3 machinery. The numeric
// equivalence itself is guaranteed by construction (one shared
// costUSDExpr string) and exercised against real Postgres by the parity
// gate; what CI can check cheaply is that nobody quietly edits one path
// and not the others.

func TestMigrationsIncludeRollupSchema(t *testing.T) {
	all := strings.Join(migrations, "\n")
	for _, want := range []string{
		"ALTER TABLE usage_events ADD COLUMN IF NOT EXISTS cost_usd",
		"CREATE TABLE IF NOT EXISTS usage_hourly",
		"CREATE UNIQUE INDEX IF NOT EXISTS usage_hourly_key ON usage_hourly (hour, username, group_name, model, provider)",
		"CREATE TABLE IF NOT EXISTS rollup_meta",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("migrations missing: %s", want)
		}
	}
}

func TestInsertEventSQLUsesCostExprVerbatim(t *testing.T) {
	for _, want := range []string{costUSDExpr, "LEFT JOIN model_pricing p ON p.model = e.model", "RETURNING cost_usd"} {
		if !strings.Contains(insertEventSQL, want) {
			t.Error("insertEventSQL lost a required clause — cost must be computed from the SHARED costUSDExpr:", want)
		}
	}
}

func TestCostBackfillSQLUsesCostExprVerbatim(t *testing.T) {
	if !strings.Contains(costBackfillSQL, costUSDExpr) {
		t.Error("cost backfill must reuse costUSDExpr verbatim — no second cost formula")
	}
	if !strings.Contains(costBackfillSQL, "e.cost_usd IS NULL") {
		t.Error("cost backfill must only touch NULL-cost rows (resumability)")
	}
}

func TestUpsertRollupAddsEverySummedColumn(t *testing.T) {
	for _, col := range []string{
		"requests", "prompt_tokens", "completion_tokens", "total_tokens",
		"cached_input_tokens", "cache_creation_tokens", "cost_usd",
	} {
		want := "usage_hourly." + col + " + EXCLUDED." + col
		if !strings.Contains(upsertRollupSQL, want) {
			t.Errorf("live upsert missing increment for %s — a dropped column silently undercounts forever", col)
		}
	}
}

func TestRebuildOverwritesFromRawDoesNotAdd(t *testing.T) {
	if strings.Contains(rebuildHourSQL, "usage_hourly.requests +") {
		t.Error("rebuild must REPLACE with raw-recomputed values (EXCLUDED), never add — adds double-count on replay")
	}
	if !strings.Contains(rebuildHourSQL, "requests = EXCLUDED.requests") {
		t.Error("rebuild must overwrite conflict rows from raw")
	}
	if !strings.Contains(rebuildHourSQL, "COUNT(*)") {
		t.Error("rebuild counts requests from raw rows (denials included, plan rev2 parity rule)")
	}
}
