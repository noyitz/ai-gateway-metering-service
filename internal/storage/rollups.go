package storage

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sort"
	"time"

	_ "github.com/lib/pq"
)

// Hourly rollup maintenance and the one-time backfill (Phase 3 of
// docs/dashboard-scaling-plan.md). Design notes that matter:
//
//   - The rollup is upserted inside each event's insert transaction, by
//     BOTH insert sites (InsertEvent and RecordQuotaDenial). No async
//     worker, no window where raw and rollup disagree while being read.
//   - The backfill is resumable across restarts via rollup_meta
//     watermarks, and idempotent: costs fill only NULL rows; the rollup
//     rebuild rewrites whole hours with values recomputed from raw
//     (ON CONFLICT DO UPDATE = EXCLUDED), so a crash mid-rebuild leaves
//     a re-runnable, never-double-counted state. Stages run in order
//     (all costs, then all hours, then recent hours, then ready) — the
//     ready flag can never land on a half-filled table.
//   - Denials (zero-usage 429 rows) count in `requests` everywhere —
//     inserts, rebuild and parity — per the plan's explicit parity
//     definition.
//   - group_name is coalesced to '' in the rollup key; the live inserts
//     already store '' (Go's empty string), so live and backfilled rows
//     key identically.

// execer lets insert paths and helpers run on either a *sql.DB or the
// *sql.Tx a caller opened — the rollup must join the caller's tx.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

const upsertRollupSQL = `
	INSERT INTO usage_hourly (hour, username, group_name, model, provider,
		requests, prompt_tokens, completion_tokens, total_tokens,
		cached_input_tokens, cache_creation_tokens, cost_usd)
	VALUES (date_trunc('hour', $1::timestamptz), $2, $3, $4, $5,
		$6::bigint, $7::bigint, $8::bigint, $9::bigint, $10::bigint, $11::bigint, $12::numeric)
	ON CONFLICT (hour, username, group_name, model, provider) DO UPDATE SET
		requests = usage_hourly.requests + EXCLUDED.requests,
		prompt_tokens = usage_hourly.prompt_tokens + EXCLUDED.prompt_tokens,
		completion_tokens = usage_hourly.completion_tokens + EXCLUDED.completion_tokens,
		total_tokens = usage_hourly.total_tokens + EXCLUDED.total_tokens,
		cached_input_tokens = usage_hourly.cached_input_tokens + EXCLUDED.cached_input_tokens,
		cache_creation_tokens = usage_hourly.cache_creation_tokens + EXCLUDED.cache_creation_tokens,
		cost_usd = usage_hourly.cost_usd + EXCLUDED.cost_usd`

// upsertRollup adds one event's contribution to its hour bucket. costUSD
// is the NUMERIC text the insert statement returned (or "0" for denial
// rows) — passing it as text keeps exact NUMERIC semantics end to end.
func upsertRollup(ctx context.Context, ex execer, ts time.Time, username, group, model, provider string,
	requests, prompt, completion, total, cached, cacheCreation int, costUSD string) error {
	_, err := ex.ExecContext(ctx, upsertRollupSQL,
		ts, username, group, model, provider,
		requests, prompt, completion, total, cached, cacheCreation, costUSD)
	return err
}

const (
	metaCostWatermark   = "cost_backfill_max_id"
	metaRollupWatermark = "rollup_rebuilt_through_hour" // last fully rebuilt hour, inclusive
	metaRollupsReady    = "rollups_ready"
	costBackfillStep    = 50000
)

// costBackfillSQL fills cost_usd for one id window. The inner SELECT uses
// costUSDExpr verbatim with the canonical e/p aliases; the UPDATE target
// carries a different alias (u) so nothing shadows or drifts.
var costBackfillSQL = fmt.Sprintf(`
	UPDATE usage_events u SET cost_usd = c.calc
	FROM (
		SELECT e.id, %s AS calc
		FROM usage_events e
		LEFT JOIN model_pricing p ON p.model = e.model
		WHERE e.id > $1 AND e.id <= $2 AND e.cost_usd IS NULL
	) c
	WHERE u.id = c.id`, costUSDExpr)

// rebuildHourSQL rewrites one hour's rollup rows from raw. Overwriting
// with EXCLUDED (not adding) is what makes a crashed rebuild safe to
// redo: recomputed-from-raw values replace whatever partial state exists.
var rebuildHourSQL = `
	INSERT INTO usage_hourly (hour, username, group_name, model, provider,
		requests, prompt_tokens, completion_tokens, total_tokens,
		cached_input_tokens, cache_creation_tokens, cost_usd)
	SELECT date_trunc('hour', e.timestamp), e.username, COALESCE(e.group_name, ''), e.model, e.provider,
		COUNT(*), SUM(e.prompt_tokens), SUM(e.completion_tokens), SUM(e.total_tokens),
		SUM(e.cached_input_tokens), SUM(e.cache_creation_tokens), COALESCE(SUM(e.cost_usd), 0)
	FROM usage_events e
	WHERE e.timestamp >= $1 AND e.timestamp < $2
	GROUP BY date_trunc('hour', e.timestamp), e.username, COALESCE(e.group_name, ''), e.model, e.provider
	ON CONFLICT (hour, username, group_name, model, provider) DO UPDATE SET
		requests = EXCLUDED.requests,
		prompt_tokens = EXCLUDED.prompt_tokens,
		completion_tokens = EXCLUDED.completion_tokens,
		total_tokens = EXCLUDED.total_tokens,
		cached_input_tokens = EXCLUDED.cached_input_tokens,
		cache_creation_tokens = EXCLUDED.cache_creation_tokens,
		cost_usd = EXCLUDED.cost_usd`

func (s *Store) metaGet(ctx context.Context, key string) (string, bool) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM rollup_meta WHERE key = $1`, key).Scan(&v)
	return v, err == nil
}

func (s *Store) metaSet(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO rollup_meta (key, value) VALUES ($1, $2) ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`,
		key, value)
	return err
}

// RollupsReady reports whether usage_hourly is trustworthy for reads —
// the gate for the Phase 3 read switch (PR B). Memory flag first; meta
// after, so a restarted pod with a completed backfill is instantly ready.
func (s *Store) RollupsReady() bool {
	if s.rollupsReadyNow.Load() {
		return true
	}
	if v, ok := s.metaGet(context.Background(), metaRollupsReady); ok && v == "true" {
		s.rollupsReadyNow.Store(true)
		return true
	}
	return false
}

// EnsureRollupsBackfilled runs the one-time cost backfill and rollup
// rebuild to completion, resuming from watermarks. Safe to launch on
// every boot: in steady state it reads one meta row and exits.
func (s *Store) EnsureRollupsBackfilled(ctx context.Context) {
	if s.RollupsReady() {
		return
	}
	for {
		done, err := s.backfillPass(ctx)
		if err == nil && done {
			return
		}
		if err != nil {
			slog.Error("rollups backfill pass failed, retrying in 60s", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(60 * time.Second):
		}
	}
}

// backfillPass drives the stages to completion in dependency order:
// every historical row gets a cost BEFORE any hour is rebuilt (rebuild
// sums cost_usd), and the ready flag lands only after all of it. Each
// stage is resumable, so a restart mid-stage picks up at the watermark.
func (s *Store) backfillPass(ctx context.Context) (bool, error) {
	for {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		done, err := s.costStep(ctx)
		if err != nil {
			return false, err
		}
		if !done {
			continue
		}
		break
	}
	for {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		done, err := s.rebuildStep(ctx)
		if err != nil {
			return false, err
		}
		if !done {
			continue
		}
		break
	}
	if err := s.refreshRecentHours(ctx); err != nil {
		return false, err
	}
	if err := s.metaSet(ctx, metaRollupsReady, "true"); err != nil {
		return false, err
	}
	s.rollupsReadyNow.Store(true)
	slog.Info("rollups backfill complete — usage_hourly is authoritative for dashboard reads")
	return true, nil
}

// costStep fills one window of NULL cost_usd rows and advances the
// watermark. New live rows already carry a cost, so the watermark simply
// sweeps past them. Returns done when it has passed MAX(id).
func (s *Store) costStep(ctx context.Context) (bool, error) {
	var maxID int64
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM usage_events`).Scan(&maxID); err != nil {
		return false, fmt.Errorf("max id: %w", err)
	}
	wm := int64(0)
	if v, ok := s.metaGet(ctx, metaCostWatermark); ok {
		if _, err := fmt.Sscanf(v, "%d", &wm); err != nil {
			wm = 0
		}
	}
	if wm >= maxID {
		return true, nil
	}
	next := minInt64(wm+costBackfillStep, maxID)
	res, err := s.db.ExecContext(ctx, costBackfillSQL, wm, next)
	if err != nil {
		return false, fmt.Errorf("cost backfill (%d,%d]: %w", wm, next, err)
	}
	n, _ := res.RowsAffected()
	if err := s.metaSet(ctx, metaCostWatermark, fmt.Sprint(next)); err != nil {
		return false, err
	}
	slog.Info("cost backfill progress", "through_id", next, "max_id", maxID, "rows_costed", n)
	// Yield between batches: this competes with live traffic on pg-1 and
	// with replication to the read replicas.
	time.Sleep(100 * time.Millisecond)
	return next >= maxID, nil
}

// rebuildStep rewrites the oldest not-yet-rebuilt hour strictly before
// the current hour (the live path already maintains current-hour rows;
// refreshRecentHours closes the final gap). Returns done when every hour
// before the current one is rebuilt.
func (s *Store) rebuildStep(ctx context.Context) (bool, error) {
	cutoff := time.Now().UTC().Truncate(time.Hour) // current hour — excluded
	wm := time.Time{}
	if v, ok := s.metaGet(ctx, metaRollupWatermark); ok {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			wm = t
		}
	}
	hour := wm.Add(time.Hour) // next hour to rebuild
	if wm.IsZero() {
		var minHour sql.NullTime
		if err := s.db.QueryRowContext(ctx,
			`SELECT MIN(date_trunc('hour', timestamp)) FROM usage_events`).Scan(&minHour); err != nil {
			return false, fmt.Errorf("min hour: %w", err)
		}
		if !minHour.Valid {
			return true, nil // empty table
		}
		hour = minHour.Time
	}
	if !hour.Before(cutoff) {
		return true, nil // all completed hours rebuilt
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, rebuildHourSQL, hour, hour.Add(time.Hour)); err != nil {
		_ = tx.Rollback()
		return false, fmt.Errorf("rebuild hour %s: %w", hour.Format(time.RFC3339), err)
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	if err := s.metaSet(ctx, metaRollupWatermark, hour.Format(time.RFC3339)); err != nil {
		return false, err
	}
	// Same courtesy as the cost sweep: one small tx per hour, but the
	// loop must not saturate pg-1 or replication lag while it drains.
	time.Sleep(50 * time.Millisecond)
	return hour.Add(time.Hour).After(cutoff) || hour.Add(time.Hour).Equal(cutoff), nil
}

// refreshRecentHours rewrites the last three hours from raw as the final
// pre-ready step: it catches anything that raced the hour-by-hour
// rebuild, in one serialized transaction. After the ready flag lands,
// live insert-path upserts alone maintain the current hour.
func (s *Store) refreshRecentHours(ctx context.Context) error {
	now := time.Now().UTC()
	from := now.Add(-3 * time.Hour).Truncate(time.Hour)
	to := now.Add(time.Hour).Truncate(time.Hour)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, rebuildHourSQL, from, to); err != nil {
		return fmt.Errorf("refresh recent hours: %w", err)
	}
	return tx.Commit()
}

// ---- Parity gate (plan rev2 pt.5): run before the read switch. ----

type ParityShape struct {
	Shape        string  `json:"shape"`
	Key          string  `json:"key"`
	RawRequests  int64   `json:"raw_requests"`
	RollRequests int64   `json:"rollup_requests"`
	RawCostUSD   float64 `json:"raw_cost_usd"`
	RollCostUSD  float64 `json:"rollup_cost_usd"`
	Match        bool    `json:"match"`
	Note         string  `json:"note,omitempty"`
}

// ParityReport diffs raw (today's read path: costUSDExpr over
// usage_events) against usage_hourly for a window, at two keyings: one
// total and per-model. Request counts must match exactly — that is the
// gate. A cost-only difference is the expected signature of a
// model_pricing change inside the window (raw re-prices at today's
// table, the rollup holds insert-time prices); it is reported, not
// hidden, and must be read by a human before the read switch.
func (s *Store) ParityReport(ctx context.Context, since, until time.Time) ([]ParityShape, error) {
	// Both sides must share HOUR boundaries: usage_hourly buckets are
	// hour-aligned, so raw must be truncated to the same boundaries or a
	// non-aligned window would drop the edge buckets on one side and the
	// gate would diff red from measurement, not drift.
	since = since.Truncate(time.Hour)
	until = until.Truncate(time.Hour).Add(time.Hour)

	type acc struct {
		rawReq, rollReq   int64
		rawCost, rollCost float64
	}
	collect := func(query string, raw bool, into map[string]*acc) error {
		rows, err := s.reader().QueryContext(ctx, query, since, until)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var k string
			var req int64
			var cost float64
			if err := rows.Scan(&k, &req, &cost); err != nil {
				return err
			}
			a := into[k]
			if a == nil {
				a = &acc{}
				into[k] = a
			}
			if raw {
				a.rawReq, a.rawCost = req, cost
			} else {
				a.rollReq, a.rollCost = req, cost
			}
		}
		return rows.Err()
	}
	shapes := []struct {
		name    string
		rawSQL  string
		rollSQL string
	}{
		{"total",
			fmt.Sprintf(`SELECT 'total', COUNT(*), COALESCE(ROUND(SUM(%s)::numeric,2),0)::float8
				FROM usage_events e LEFT JOIN model_pricing p ON e.model = p.model
				WHERE e.timestamp >= $1 AND e.timestamp < $2 GROUP BY 1`, costUSDExpr),
			`SELECT 'total', COALESCE(SUM(requests),0), COALESCE(ROUND(SUM(cost_usd)::numeric,2),0)::float8
				FROM usage_hourly WHERE hour >= $1 AND hour < $2 GROUP BY 'total'`},
		{"per_model",
			fmt.Sprintf(`SELECT e.model, COUNT(*), COALESCE(ROUND(SUM(%s)::numeric,2),0)::float8
				FROM usage_events e LEFT JOIN model_pricing p ON e.model = p.model
				WHERE e.timestamp >= $1 AND e.timestamp < $2 GROUP BY e.model`, costUSDExpr),
			`SELECT model, COALESCE(SUM(requests),0), COALESCE(ROUND(SUM(cost_usd)::numeric,2),0)::float8
				FROM usage_hourly WHERE hour >= $1 AND hour < $2 GROUP BY model`},
	}

	out := make([]ParityShape, 0)
	for _, sh := range shapes {
		side := map[string]*acc{}
		if err := collect(sh.rawSQL, true, side); err != nil {
			return nil, fmt.Errorf("parity raw %s: %w", sh.name, err)
		}
		if err := collect(sh.rollSQL, false, side); err != nil {
			return nil, fmt.Errorf("parity rollup %s: %w", sh.name, err)
		}
		keys := make([]string, 0, len(side))
		for k := range side {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			a := side[k]
			ps := ParityShape{Shape: sh.name, Key: k, RawRequests: a.rawReq, RollRequests: a.rollReq,
				RawCostUSD: a.rawCost, RollCostUSD: a.rollCost, Match: a.rawReq == a.rollReq}
			d := a.rawCost - a.rollCost
			if d > 0.005 || d < -0.005 {
				ps.Note = "cost differs beyond cents tolerance — expected only if model_pricing changed inside the window (rollup holds insert-time prices)"
			}
			out = append(out, ps)
		}
	}
	return out, nil
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
