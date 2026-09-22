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

const (
	metaCostWatermark   = "cost_backfill_max_id"
	metaRollupWatermark = "rollup_rebuilt_through_hour" // last fully rebuilt hour, inclusive
	metaRollupsReady    = "rollups_ready"
	metaParityHealthy   = "parity_healthy"
	metaParityCheckedAt = "parity_checked_at"
	costBackfillStep    = 50000
)

// ---- Hour locks (part B) ----
//
// A periodic refreshRecentHours rewrites an hour from raw and, done naively,
// can overwrite a live insert-path upsert that commits while the rebuild's
// aggregate snapshot was already taken (the rebuild's INSERT..SELECT never
// saw the row; its DELETE+INSERT then drops the live +1). One-shot during
// part A's backfill and parity-gated; continuous in part B with reads
// depending on it, so both writers on an hour bucket must serialize on an
// advisory transaction lock keyed to that bucket:
//
//   - upsertRollup takes the lock for its own hour, inside the event's tx;
//   - rebuild/refresh take the locks for their whole [from,to) range, in
//     ascending hour order, inside their tx.
//
// Deadlock-freedom: upserters hold exactly one hour lock; refreshers
// acquire multiple strictly ascending. A cycle would need some upser to
// wait for a lock a refresher holds while the refresher waits further up
// the same ascending chain past that upser's single lock — impossible.
// The bucket is rendered in UTC explicitly ("AT TIME ZONE 'UTC'") so the
// key can never depend on a session TimeZone; both statements below must
// keep the identical render expression (a test guards the drift).

const upsertHourLockSQL = `SELECT pg_advisory_xact_lock(hashtext('usage_hourly:' || to_char(date_trunc('hour', $1::timestamptz) AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24')))`

const rebuildHourLockSQL = `SELECT pg_advisory_xact_lock(hashtext('usage_hourly:' || to_char(h AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24'))) FROM generate_series($1::timestamptz, $2::timestamptz - interval '1 hour', interval '1 hour') h ORDER BY h`

// upsertRollup adds one event's contribution to its hour bucket. costUSD
// is the NUMERIC text the insert statement returned (or "0" for denial
// rows) — passing it as text keeps exact NUMERIC semantics end to end.
// Must be called inside a transaction (both insert sites do): the hour
// lock is transaction-scoped and would be a no-op on the autocommit pool.
func upsertRollup(ctx context.Context, ex execer, ts time.Time, username, group, model, provider string,
	requests, prompt, completion, total, cached, cacheCreation int, costUSD string) error {
	if _, err := ex.ExecContext(ctx, upsertHourLockSQL, ts); err != nil {
		return err
	}
	_, err := ex.ExecContext(ctx, upsertRollupSQL,
		ts, username, group, model, provider,
		requests, prompt, completion, total, cached, cacheCreation, costUSD)
	return err
}

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
	if _, err := tx.ExecContext(ctx, rebuildHourLockSQL, hour, hour.Add(time.Hour)); err != nil {
		_ = tx.Rollback()
		return false, fmt.Errorf("lock hour %s: %w", hour.Format(time.RFC3339), err)
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

// refreshRecentHours rewrites the last three hours from raw. It closes
// the final backfill gap before the ready flag lands, and part B runs it
// on the maintenance ticker to keep recent buckets exactly reconciled.
// The range lock (ascending hour order) serializes it against live
// insert-path upserts so a rebuild can never drop a concurrent increment.
func (s *Store) refreshRecentHours(ctx context.Context) error {
	now := time.Now().UTC()
	from := now.Add(-3 * time.Hour).Truncate(time.Hour)
	to := now.Add(time.Hour).Truncate(time.Hour)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, rebuildHourLockSQL, from, to); err != nil {
		return fmt.Errorf("lock recent hours: %w", err)
	}
	if _, err := tx.ExecContext(ctx, rebuildHourSQL, from, to); err != nil {
		return fmt.Errorf("refresh recent hours: %w", err)
	}
	return tx.Commit()
}

// ---- Read gating and standing parity (part B) ----

// UseRollups applies the DASHBOARD_USE_ROLLUPS read switch. It is
// deliberately independent of deployment: maintenance keeps the table
// reconciled and parity warm while the flag is off, so enabling is a
// one-knob flip over a table that has been green all along — and the
// one-knob rollback (plan rev2, Noy's two-decisions rule).
func (s *Store) UseRollups(on bool) {
	s.useRollups.Store(on)
	slog.Info("rollup read switch set", "enabled", on)
}

// rollupsLive is the read path's single decision point: flag on, backfill
// complete, standing parity green. Any of the three failing serves raw —
// the conservative source — and a change while the flag is on is logged
// once, not per request. Readiness is the CACHED flag, never metaGet:
// this runs on every dashboard request, and the maintenance loop (and
// the backfill landing) keep the flag true within one tick of a restart.
func (s *Store) rollupsLive() bool {
	flag := s.useRollups.Load()
	ready := s.rollupsReadyNow.Load()
	healthy := s.parityHealthy.Load()
	live := flag && ready && healthy
	if s.servingRollups.Swap(live) != live {
		switch {
		case live:
			slog.Info("dashboard aggregations serving from usage_hourly")
		case flag:
			slog.Warn("dashboard rollup reads inactive — serving raw", "ready", ready, "parity_healthy", healthy)
		}
	}
	return live
}

// RollupServing / ParityHealthy / RollupFlag report live state for the
// admin status endpoint.
func (s *Store) RollupServing() bool { return s.servingRollups.Load() }
func (s *Store) ParityHealthy() bool { return s.parityHealthy.Load() }
func (s *Store) RollupFlag() bool    { return s.useRollups.Load() }

// RunRollupMaintenance runs the part B loop: reconcile recent hours, then
// the standing parity check (plan rev2, Noy's conditions 2 and 3). It runs
// REGARDLESS of the read flag — the table stays exactly reconciled and the
// parity signal stays warm so the flag flip is risk-free and a red check
// falls reads back to raw within one tick, without anyone touching a knob.
// The first pass runs immediately; deep (7d) parity runs hourly.
func (s *Store) RunRollupMaintenance(ctx context.Context, interval time.Duration) {
	if interval < time.Minute {
		interval = time.Minute
	}
	deepTicks := int(time.Hour / interval)
	if deepTicks < 1 {
		deepTicks = 1
	}
	s.maintainOnce(ctx, true)
	t := time.NewTicker(interval)
	defer t.Stop()
	ticks := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			ticks++
			s.maintainOnce(ctx, ticks%deepTicks == 0)
		}
	}
}

func (s *Store) maintainOnce(ctx context.Context, deep bool) {
	if !s.RollupsReady() {
		return // the backfill goroutine owns the table until it lands ready
	}
	if err := s.refreshRecentHours(ctx); err != nil {
		slog.Error("rollup refresh failed", "error", err)
		return
	}
	window := 24 * time.Hour
	if deep {
		window = 7 * 24 * time.Hour
	}
	// Until excludes the in-flight hour: live traffic and replication are
	// still moving it, and red there would be measurement, not drift.
	until := time.Now().UTC().Truncate(time.Hour)
	report, err := s.ParityReport(ctx, until.Add(-window), until)
	if err != nil {
		slog.Error("rollup parity check failed", "window", window, "error", err)
		return
	}
	red := false
	for _, ps := range report {
		if !ps.Match && !red {
			red = true
		}
		if ps.Note != "" && deep {
			slog.Warn("rollup parity cost note", "shape", ps.Shape, "key", ps.Key,
				"raw_cost", ps.RawCostUSD, "rollup_cost", ps.RollCostUSD)
		}
	}
	prev := s.parityHealthy.Swap(!red)
	healthyStr, checkedAt := "false", time.Now().UTC().Format(time.RFC3339)
	if !red {
		healthyStr = "true"
	}
	_ = s.metaSet(ctx, metaParityHealthy, healthyStr)
	_ = s.metaSet(ctx, metaParityCheckedAt, checkedAt)
	if red {
		slog.Error("ROLLUP PARITY RED — dashboard aggregations falling back to raw", "window", window, "shapes", report)
	} else if deep {
		slog.Info("rollup parity green", "window", window)
	} else if !prev {
		slog.Info("rollup parity recovered — dashboard reads return to usage_hourly")
	}
}

// ---- Parity gate (plan rev2 pt.5): pre-switch gate AND standing check. ----

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
// usage_events) against usage_hourly for a window, at several keyings:
// one total, per-model, per-group, per-user and per-day. Request counts
// must match exactly — that is the automatic gate, and in part B it is
// standing: a red check flips reads back to raw until it recovers. A
// cost-only difference is the expected signature of a model_pricing
// change inside the window (raw re-prices at today's table, the rollup
// holds insert-time prices); it is reported as a note, not a red, and a
// human reads it — auto-falling-back on every catalog change would keep
// the table permanently switched off. The per-group keying intentionally
// renders raw's NULL-to-'unknown'/empty labels and the rollup's
// collapse-to-empty differently; if legacy empty-string-versus-NULL group
// rows ever make that shape red, it is a real divergence to look at, not
// to paper over. The whole report runs inside ONE REPEATABLE READ
// transaction on the reader connection: the invariant only holds within
// one snapshot — a raw query at WAL position P and the rollup query at
// a later P' would diff every event that replicated in between.
func (s *Store) ParityReport(ctx context.Context, since, until time.Time) ([]ParityShape, error) {
	// Both sides must share HOUR boundaries: usage_hourly buckets are
	// hour-aligned, so raw must be truncated to the same boundaries or a
	// non-aligned window would drop the edge buckets on one side and the
	// gate would diff red from measurement, not drift.
	since = since.Truncate(time.Hour)
	until = until.Truncate(time.Hour).Add(time.Hour)

	tx, err := s.reader().BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("parity snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	type acc struct {
		rawReq, rollReq   int64
		rawCost, rollCost float64
	}
	collect := func(query string, raw bool, into map[string]*acc) error {
		rows, err := tx.QueryContext(ctx, query, since, until)
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
	shapes := parityShapes()

	out := make([]ParityShape, 0)
	for _, sh := range shapes {
		side := map[string]*acc{}
		if err := collect(sh.RawSQL, true, side); err != nil {
			return nil, fmt.Errorf("parity raw %s: %w", sh.Name, err)
		}
		if err := collect(sh.RollSQL, false, side); err != nil {
			return nil, fmt.Errorf("parity rollup %s: %w", sh.Name, err)
		}
		keys := make([]string, 0, len(side))
		for k := range side {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			a := side[k]
			ps := ParityShape{Shape: sh.Name, Key: k, RawRequests: a.rawReq, RollRequests: a.rollReq,
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

// parityShapes returns the raw-vs-rollup query pairs for every keying the
// switched readers use. Package-level so the live-schema pre-flight can
// PREPARE every one of them — the part-B first boot found
// "GROUP BY 'total'" (Postgres rejects a non-integer constant in GROUP
// BY) shipped in part A's ParityReport unexecuted: the endpoint was never
// hit and the pre-flight had deliberately skipped parity SQL. Parity SQL
// is now READ-PATH-CRITICAL code and must parse before any of it ships.
// The total key groups by OUTPUT POSITION (1), not by a string literal,
// on both sides.
type parityShapeSQL struct {
	Name    string
	RawSQL  string
	RollSQL string
}

func parityShapes() []parityShapeSQL {
	return []parityShapeSQL{
		{"total",
			fmt.Sprintf(`SELECT 'total', COUNT(*), COALESCE(ROUND(SUM(%s)::numeric,2),0)::float8
				FROM usage_events e LEFT JOIN model_pricing p ON e.model = p.model
				WHERE e.timestamp >= $1 AND e.timestamp < $2 GROUP BY 1`, costUSDExpr),
			`SELECT 'total', COALESCE(SUM(requests),0), COALESCE(ROUND(SUM(cost_usd)::numeric,2),0)::float8
				FROM usage_hourly WHERE hour >= $1 AND hour < $2 GROUP BY 1`},
		{"per_model",
			fmt.Sprintf(`SELECT e.model, COUNT(*), COALESCE(ROUND(SUM(%s)::numeric,2),0)::float8
				FROM usage_events e LEFT JOIN model_pricing p ON e.model = p.model
				WHERE e.timestamp >= $1 AND e.timestamp < $2 GROUP BY e.model`, costUSDExpr),
			`SELECT model, COALESCE(SUM(requests),0), COALESCE(ROUND(SUM(cost_usd)::numeric,2),0)::float8
				FROM usage_hourly WHERE hour >= $1 AND hour < $2 GROUP BY model`},
		{"per_group",
			fmt.Sprintf(`SELECT COALESCE(e.group_name, 'unknown'), COUNT(*), COALESCE(ROUND(SUM(%s)::numeric,2),0)::float8
				FROM usage_events e LEFT JOIN model_pricing p ON e.model = p.model
				WHERE e.timestamp >= $1 AND e.timestamp < $2
				GROUP BY COALESCE(e.group_name, 'unknown')`, costUSDExpr),
			`SELECT COALESCE(NULLIF(group_name, ''), 'unknown'), COALESCE(SUM(requests),0), COALESCE(ROUND(SUM(cost_usd)::numeric,2),0)::float8
				FROM usage_hourly WHERE hour >= $1 AND hour < $2
				GROUP BY COALESCE(NULLIF(group_name, ''), 'unknown')`},
		{"per_user",
			fmt.Sprintf(`SELECT e.username, COUNT(*), COALESCE(ROUND(SUM(%s)::numeric,2),0)::float8
				FROM usage_events e LEFT JOIN model_pricing p ON e.model = p.model
				WHERE e.timestamp >= $1 AND e.timestamp < $2 GROUP BY e.username`, costUSDExpr),
			`SELECT username, COALESCE(SUM(requests),0), COALESCE(ROUND(SUM(cost_usd)::numeric,2),0)::float8
				FROM usage_hourly WHERE hour >= $1 AND hour < $2 GROUP BY username`},
		{"per_day",
			fmt.Sprintf(`SELECT date_trunc('day', e.timestamp)::text, COUNT(*), COALESCE(ROUND(SUM(%s)::numeric,2),0)::float8
				FROM usage_events e LEFT JOIN model_pricing p ON e.model = p.model
				WHERE e.timestamp >= $1 AND e.timestamp < $2
				GROUP BY date_trunc('day', e.timestamp)::text`, costUSDExpr),
			`SELECT date_trunc('day', hour)::text, COALESCE(SUM(requests),0), COALESCE(ROUND(SUM(cost_usd)::numeric,2),0)::float8
				FROM usage_hourly WHERE hour >= $1 AND hour < $2
				GROUP BY date_trunc('day', hour)::text`},
	}
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

// ---- Rollup read variants (part B) ----
//
// Each constant below is the exact raw-query shape with three swaps and
// nothing else: the source (usage_hourly, alias e preserved), the window
// column (e.hour, so bucket and window are the same value — no trunc()
// per row), and cost (the stored cost_usd instead of the live-priced
// expression). Counts are SUM(requests), never COUNT(*) — one hourly row
// is N events. The switch is invisible except for the two known,
// documented deltas: cost is frozen at event-time instead of re-priced
// at read, and recent hours lag by the refresh interval; both fall back
// automatically when parity goes red. The raw variants in postgres.go
// are untouched text — grep-guarded by tests in both directions.

const rollupOverviewSQL = `
	SELECT COALESCE(SUM(e.requests),0),
		COALESCE(SUM(e.prompt_tokens),0),
		COALESCE(SUM(e.completion_tokens),0),
		COALESCE(SUM(e.total_tokens),0),
		COUNT(DISTINCT e.username),
		COALESCE(ROUND(SUM(e.cost_usd)::numeric, 2), 0)
	FROM usage_hourly e
	WHERE e.hour >= $1 AND e.hour < $2 AND ($3 = '' OR e.group_name = $3) AND ($4 = '' OR e.username = ANY(string_to_array($4, ','))) AND ($5 = '' OR e.model = $5)`

const rollupGroupsSQL = `
	SELECT COALESCE(NULLIF(e.group_name, ''), 'unknown'),
		COALESCE(SUM(e.requests),0),
		COALESCE(SUM(e.total_tokens),0),
		COUNT(DISTINCT e.username),
		COALESCE(ROUND(SUM(e.cost_usd)::numeric, 2), 0)
	FROM usage_hourly e
	WHERE e.hour >= $1 AND e.hour < $2 AND ($3 = '' OR e.group_name = $3) AND ($4 = '' OR e.username = ANY(string_to_array($4, ','))) AND ($5 = '' OR e.model = $5)
	GROUP BY COALESCE(NULLIF(e.group_name, ''), 'unknown')
	ORDER BY SUM(e.total_tokens) DESC`

const rollupTeamUsageSQL = `
	SELECT e.username, e.model, e.provider,
		SUM(e.requests),
		SUM(e.prompt_tokens),
		SUM(e.completion_tokens),
		SUM(e.total_tokens),
		ROUND(SUM(e.cost_usd)::numeric, 4)
	FROM usage_hourly e
	WHERE e.group_name = $1
	GROUP BY e.username, e.model, e.provider
	ORDER BY e.username, SUM(e.total_tokens) DESC`

// rollupModelsSQL must go through Sprintf: hostedProviderCond carries a
// doubled LIKE wildcard that only un-doubles when the text passes through
// a format pass, exactly like the raw variant's Sprintf.
var rollupModelsSQL = fmt.Sprintf(`
	SELECT e.model, COALESCE(e.provider, ''),
		COALESCE(SUM(e.requests),0),
		COALESCE(SUM(e.total_tokens),0),
		COALESCE(SUM(e.prompt_tokens),0),
		COALESCE(SUM(e.completion_tokens),0),
		COALESCE(SUM(e.cached_input_tokens),0),
		COALESCE(SUM(e.cache_creation_tokens),0),
		COALESCE(ROUND(SUM(e.cost_usd)::numeric, 2), 0),
		bool_or(%s),
		COALESCE(MAX(p.input_cost_per_mtok), 0),
		COALESCE(MAX(p.output_cost_per_mtok), 0),
		COALESCE(MAX(p.cache_read_cost_per_mtok), 0),
		COALESCE(MAX(p.cache_write_cost_per_mtok), 0)
	FROM usage_hourly e
	LEFT JOIN model_pricing p ON e.model = p.model
	WHERE e.hour >= $1 AND e.hour < $2 AND ($3 = '' OR e.group_name = $3) AND ($4 = '' OR e.username = ANY(string_to_array($4, ','))) AND ($5 = '' OR e.model = $5)
	GROUP BY e.model, COALESCE(e.provider, '')
	ORDER BY SUM(e.total_tokens) DESC`, hostedProviderCond)

// rollupTimelineSQL mirrors the raw timeline shape: bucket truncation and
// series column are interpolated the same way the raw builder does.
func rollupTimelineSQL(truncInterval, seriesCol string) string {
	return fmt.Sprintf(`
		SELECT date_trunc('%s', e.hour) as bucket,
			%s as series,
			COALESCE(SUM(e.total_tokens),0),
			COALESCE(SUM(e.requests),0)
		FROM usage_hourly e
		WHERE e.hour >= $1 AND e.hour < $2 AND ($3 = '' OR e.group_name = $3) AND ($4 = '' OR e.username = ANY(string_to_array($4, ','))) AND ($5 = '' OR e.model = $5)
		GROUP BY bucket, series
		ORDER BY bucket, series`, truncInterval, seriesCol)
}

// rollupUsersSelect is the GetDashboardUsers tail SELECT over the same
// savings CTE (rendered rollup-side by hostedSavingsWithSQL), mirroring
// the raw tail's columns, display-name and sort handling exactly.
var rollupUsersSelect = `
		SELECT e.username,
			%s,
			COALESCE(e.group_name, ''),
			COALESCE(SUM(e.requests),0) as requests,
			COALESCE(SUM(e.prompt_tokens),0) as prompt_tokens,
			COALESCE(SUM(e.completion_tokens),0) as completion_tokens,
			COALESCE(SUM(e.total_tokens),0) as total_tokens,
			COALESCE(ROUND(SUM(e.cost_usd)::numeric, 2), 0) as cost_usd,
			COALESCE(ROUND(MAX(sv.saved)::numeric, 2), 0) as saved_usd
		FROM usage_hourly e
		LEFT JOIN user_profiles up ON up.username = e.username
		LEFT JOIN sv ON sv.username = e.username
		WHERE e.hour >= $1 AND e.hour < $2 AND ($3 = '' OR e.group_name = $3) AND ($4 = '' OR e.username = ANY(string_to_array($4, ','))) AND ($5 = '' OR e.model = $5)
		GROUP BY e.username, %s, COALESCE(e.group_name, '')
		ORDER BY %s %s
		LIMIT $6`

// RecentModels returns the distinct model identifiers seen in the last 7
// days. Reads the hourly rollup when it is ready (~1k rows) and falls back
// to the raw ledger otherwise — the same gating the dashboard read switch
// uses, for the allowance editor's candidate list (issue #22).
func (s *Store) RecentModels(ctx context.Context) ([]string, error) {
	query := `SELECT DISTINCT model FROM usage_events
		WHERE timestamp >= NOW() - interval '7 days' AND model <> '' ORDER BY model`
	if s.RollupsReady() {
		query = `SELECT DISTINCT model FROM usage_hourly
			WHERE hour >= date_trunc('hour', NOW()) - interval '7 days' AND model <> '' ORDER BY model`
	}
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
