package storage

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	_ "github.com/lib/pq"
)

type UsageEvent struct {
	EventID             string
	Timestamp           time.Time
	Username            string
	GroupName           string
	Subscription        string
	Provider            string
	Model               string
	PromptTokens        int
	CompletionTokens    int
	TotalTokens         int
	CachedInputTokens   int
	CacheCreationTokens int
	ReasoningTokens     int
	Source              string
	UserAgent           string
	// StatusCode is the upstream HTTP status for the request. nil means
	// unknown (a row ingested before this column existed).
	StatusCode *int
}

type UsageStats struct {
	HasAccess bool    `json:"hasAccess"`
	Balance   float64 `json:"balance"`
	Usage     float64 `json:"usage"`
	Overage   float64 `json:"overage"`
}

type Store struct {
	db *sql.DB

	// tokenQuota is the per-user monthly token budget enforced by the
	// entitlement endpoint. A value <= 0 means unlimited: the service
	// reports usage but does not gate access. Real quota enforcement
	// belongs in the gateway (see praxis-proxy/ai#121), not here.
	tokenQuota int64
}

func New(databaseURL string, tokenQuota int64) (*Store, error) {
	db, err := sql.Open("postgres", databaseURL)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(14400 * time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("ping database: %w", err)
	}

	s := &Store{db: db, tokenQuota: tokenQuota}
	if err := s.migrate(ctx); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}

	return s, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) InsertEvent(ctx context.Context, e UsageEvent) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO usage_events (event_id, timestamp, username, group_name, subscription, provider, model, prompt_tokens, completion_tokens, total_tokens, cached_input_tokens, cache_creation_tokens, reasoning_tokens, source, user_agent, status_code)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)`,
		e.EventID, e.Timestamp, e.Username, e.GroupName, e.Subscription, e.Provider, e.Model,
		e.PromptTokens, e.CompletionTokens, e.TotalTokens, e.CachedInputTokens, e.CacheCreationTokens, e.ReasoningTokens, e.Source, e.UserAgent, e.StatusCode,
	)
	return err
}

type TeamUserUsage struct {
	Username         string       `json:"username"`
	Requests         int          `json:"requests"`
	PromptTokens     int          `json:"prompt_tokens"`
	CompletionTokens int          `json:"completion_tokens"`
	TotalTokens      int          `json:"total_tokens"`
	CostUSD          float64      `json:"cost_usd"`
	Models           []ModelUsage `json:"models"`
}

type ModelUsage struct {
	Model       string  `json:"model"`
	Provider    string  `json:"provider"`
	Requests    int     `json:"requests"`
	TotalTokens int     `json:"total_tokens"`
	CostUSD     float64 `json:"cost_usd"`
}

// costUSDExpr is the SQL arithmetic for per-row inference cost, shared by
// every cost query so the model lives in exactly one place.
//
// Provider usage fields are disjoint: prompt_tokens already includes the
// cache-read and cache-creation tokens, so BOTH are subtracted here to
// bill the uncached remainder at the base input rate exactly once.
// Cache-read and cache-creation tokens are then billed once each at their
// own rates. (Subtracting only cache-read — the earlier form — billed
// cache-creation tokens at the input rate AND the cache-write rate,
// overstating cache-miss turns by up to ~1.8x.)
//
// The cache-write fallback is 18.75 (1.25x the 15 input fallback, the
// standard cache-write premium) so an unpriced model with cache-creation
// tokens is estimated, not billed at $0 — subtracting those tokens from
// the uncached term means a 0 fallback here would drop them entirely.
//
// Requires usage_events aliased as `e` and model_pricing as `p`. See
// perRequestCostUSD in postgres_test.go for the executable reference model.
const costUSDExpr = `GREATEST(e.prompt_tokens - COALESCE(e.cached_input_tokens, 0) - COALESCE(e.cache_creation_tokens, 0), 0) * COALESCE(p.input_cost_per_mtok, 15)/1000000.0 +
			COALESCE(e.cached_input_tokens, 0) * COALESCE(p.cache_read_cost_per_mtok, 0.5)/1000000.0 +
			COALESCE(e.cache_creation_tokens, 0) * COALESCE(p.cache_write_cost_per_mtok, 18.75)/1000000.0 +
			e.completion_tokens * COALESCE(p.output_cost_per_mtok, 75)/1000000.0`

// listCostUSDExpr mirrors costUSDExpr but prices each term at the vendor
// list rate (model_pricing.list_*), with a two-tier fallback per term:
//   - list rate = 0 means "no list price seeded" for this model — fall back
//     to the exact effective gateway rate (its seeded value, else the same
//     hardcoded default), so saved_usd is exactly 0 for unseeded models
//     instead of a fabricated difference.
//
// Same disjoint-field arithmetic and same aliases (e / p) as costUSDExpr.
const listCostUSDExpr = `GREATEST(e.prompt_tokens - COALESCE(e.cached_input_tokens, 0) - COALESCE(e.cache_creation_tokens, 0), 0) * COALESCE(NULLIF(p.list_input_cost_per_mtok, 0), COALESCE(p.input_cost_per_mtok, 15))/1000000.0 +
			COALESCE(e.cached_input_tokens, 0) * COALESCE(NULLIF(p.list_cache_read_cost_per_mtok, 0), COALESCE(p.cache_read_cost_per_mtok, 0.5))/1000000.0 +
			COALESCE(e.cache_creation_tokens, 0) * COALESCE(NULLIF(p.list_cache_write_cost_per_mtok, 0), COALESCE(p.cache_write_cost_per_mtok, 18.75))/1000000.0 +
			e.completion_tokens * COALESCE(NULLIF(p.list_output_cost_per_mtok, 0), COALESCE(p.output_cost_per_mtok, 75))/1000000.0`

func (s *Store) GetTeamUsage(ctx context.Context, groupName string) ([]TeamUserUsage, error) {
	query := fmt.Sprintf(`
		SELECT e.username, e.model, e.provider,
			COUNT(*) as requests,
			SUM(e.prompt_tokens) as prompt_tokens,
			SUM(e.completion_tokens) as completion_tokens,
			SUM(e.total_tokens) as total_tokens,
			ROUND(SUM(%s)::numeric, 4) as cost_usd
		FROM usage_events e
		LEFT JOIN model_pricing p ON e.model = p.model
		WHERE e.group_name = $1
		GROUP BY e.username, e.model, e.provider
		ORDER BY e.username, total_tokens DESC`, costUSDExpr)
	rows, err := s.db.QueryContext(ctx, query, groupName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	userMap := make(map[string]*TeamUserUsage)
	var order []string

	for rows.Next() {
		var username, model, provider string
		var requests, promptTokens, completionTokens, totalTokens int
		var costUSD float64
		if err := rows.Scan(&username, &model, &provider, &requests, &promptTokens, &completionTokens, &totalTokens, &costUSD); err != nil {
			return nil, err
		}

		u, ok := userMap[username]
		if !ok {
			u = &TeamUserUsage{Username: username}
			userMap[username] = u
			order = append(order, username)
		}
		u.Requests += requests
		u.PromptTokens += promptTokens
		u.CompletionTokens += completionTokens
		u.TotalTokens += totalTokens
		u.CostUSD += costUSD
		u.Models = append(u.Models, ModelUsage{
			Model: model, Provider: provider,
			Requests: requests, TotalTokens: totalTokens, CostUSD: costUSD,
		})
	}

	result := make([]TeamUserUsage, 0, len(order))
	for _, name := range order {
		result = append(result, *userMap[name])
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Store) GetMonthlyUsage(ctx context.Context, username, model string) (UsageStats, error) {
	var used int64
	row := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(total_tokens), 0) FROM usage_events
		 WHERE username = $1 AND timestamp >= date_trunc('month', NOW())`,
		username,
	)
	if err := row.Scan(&used); err != nil {
		return UsageStats{}, err
	}

	return computeUsageStats(used, s.tokenQuota), nil
}

// computeUsageStats derives entitlement stats from token usage and a quota.
// A quota <= 0 means unlimited: usage is reported but access is never gated.
// Quota enforcement is the gateway's responsibility (praxis-proxy/ai#121).
func computeUsageStats(used, tokenQuota int64) UsageStats {
	usage := float64(used)

	if tokenQuota <= 0 {
		return UsageStats{HasAccess: true, Usage: usage}
	}

	quota := float64(tokenQuota)
	balance := quota - usage
	overage := float64(0)
	if balance < 0 {
		overage = -balance
		balance = 0
	}

	return UsageStats{
		HasAccess: usage < quota,
		Balance:   balance,
		Usage:     usage,
		Overage:   overage,
	}
}

// Dashboard types

type DashboardOverview struct {
	TotalRequests         int     `json:"total_requests"`
	TotalPromptTokens     int64   `json:"total_prompt_tokens"`
	TotalCompletionTokens int64   `json:"total_completion_tokens"`
	TotalTokens           int64   `json:"total_tokens"`
	ActiveUsers           int     `json:"active_users"`
	TotalCostUSD          float64 `json:"total_cost_usd"`
}

type GroupSummary struct {
	GroupName   string  `json:"group_name"`
	Requests    int     `json:"requests"`
	TotalTokens int64   `json:"total_tokens"`
	UserCount   int     `json:"user_count"`
	CostUSD     float64 `json:"cost_usd"`
}

type UserSummary struct {
	Username         string  `json:"username"`
	GroupName        string  `json:"group_name"`
	Requests         int     `json:"requests"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
	CostUSD          float64 `json:"cost_usd"`
	// SavedUSD is what the same tokens would cost at the vendor list price
	// minus what the gateway actually charged. 0 for models without a
	// seeded list price (the list expression falls back to the effective
	// gateway rate, making the difference exactly zero).
	SavedUSD float64 `json:"saved_usd"`
}

type ModelSummary struct {
	Model               string  `json:"model"`
	Provider            string  `json:"provider"`
	Requests            int     `json:"requests"`
	TotalTokens         int64   `json:"total_tokens"`
	PromptTokens        int64   `json:"prompt_tokens"`
	CompletionTokens    int64   `json:"completion_tokens"`
	CachedInputTokens   int64   `json:"cached_input_tokens"`
	CacheCreationTokens int64   `json:"cache_creation_tokens"`
	CostUSD             float64 `json:"cost_usd"`
}

type TimelineBucket struct {
	Bucket      time.Time `json:"bucket"`
	Series      string    `json:"series"`
	TotalTokens int64     `json:"total_tokens"`
	Requests    int       `json:"requests"`
}

func (s *Store) GetDashboardOverview(ctx context.Context, since, until time.Time, group, user, model string) (DashboardOverview, error) {
	var o DashboardOverview
	err := s.db.QueryRowContext(ctx, fmt.Sprintf(`
		SELECT COUNT(*),
			COALESCE(SUM(e.prompt_tokens),0),
			COALESCE(SUM(e.completion_tokens),0),
			COALESCE(SUM(e.total_tokens),0),
			COUNT(DISTINCT e.username),
			COALESCE(ROUND(SUM(%s)::numeric, 2), 0)
		FROM usage_events e
		LEFT JOIN model_pricing p ON e.model = p.model
		WHERE e.timestamp >= $1 AND e.timestamp < $2 AND ($3 = '' OR e.group_name = $3) AND ($4 = '' OR e.username = ANY(string_to_array($4, ','))) AND ($5 = '' OR e.model = $5)`, costUSDExpr),
		since, until, group, user, model).Scan(
		&o.TotalRequests, &o.TotalPromptTokens, &o.TotalCompletionTokens,
		&o.TotalTokens, &o.ActiveUsers, &o.TotalCostUSD)
	return o, err
}

func (s *Store) GetDashboardGroups(ctx context.Context, since, until time.Time, group, user, model string) ([]GroupSummary, error) {
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT COALESCE(e.group_name, 'unknown'),
			COUNT(*),
			COALESCE(SUM(e.total_tokens),0),
			COUNT(DISTINCT e.username),
			COALESCE(ROUND(SUM(%s)::numeric, 2), 0)
		FROM usage_events e
		LEFT JOIN model_pricing p ON e.model = p.model
		WHERE e.timestamp >= $1 AND e.timestamp < $2 AND ($3 = '' OR e.group_name = $3) AND ($4 = '' OR e.username = ANY(string_to_array($4, ','))) AND ($5 = '' OR e.model = $5)
		GROUP BY COALESCE(e.group_name, 'unknown')
		ORDER BY SUM(e.total_tokens) DESC`, costUSDExpr), since, until, group, user, model)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []GroupSummary
	for rows.Next() {
		var g GroupSummary
		if err := rows.Scan(&g.GroupName, &g.Requests, &g.TotalTokens, &g.UserCount, &g.CostUSD); err != nil {
			return nil, err
		}
		result = append(result, g)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Store) GetDashboardUsers(ctx context.Context, since, until time.Time, group, user, model, sortCol, sortOrder string, limit int) ([]UserSummary, error) {
	validSorts := map[string]string{
		"total_tokens": "total_tokens", "cost_usd": "cost_usd", "saved_usd": "saved_usd",
		"requests": "requests", "username": "e.username",
		"prompt_tokens": "prompt_tokens", "completion_tokens": "completion_tokens",
	}
	sortExpr, ok := validSorts[sortCol]
	if !ok {
		sortExpr = "total_tokens"
	}
	direction := "DESC"
	if sortOrder == "asc" {
		direction = "ASC"
	}
	if limit <= 0 || limit > 200 {
		limit = 100
	}

	query := fmt.Sprintf(`
		SELECT e.username,
			COALESCE(e.group_name, ''),
			COUNT(*) as requests,
			COALESCE(SUM(e.prompt_tokens),0) as prompt_tokens,
			COALESCE(SUM(e.completion_tokens),0) as completion_tokens,
			COALESCE(SUM(e.total_tokens),0) as total_tokens,
			COALESCE(ROUND(SUM(%s)::numeric, 2), 0) as cost_usd,
			COALESCE(ROUND((SUM(%s) - SUM(%s))::numeric, 2), 0) as saved_usd
		FROM usage_events e
		LEFT JOIN model_pricing p ON e.model = p.model
		WHERE e.timestamp >= $1 AND e.timestamp < $2 AND ($3 = '' OR e.group_name = $3) AND ($4 = '' OR e.username = ANY(string_to_array($4, ','))) AND ($5 = '' OR e.model = $5)
		GROUP BY e.username, COALESCE(e.group_name, '')
		ORDER BY %s %s
		LIMIT $6`, costUSDExpr, listCostUSDExpr, costUSDExpr, sortExpr, direction)

	rows, err := s.db.QueryContext(ctx, query, since, until, group, user, model, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []UserSummary
	for rows.Next() {
		var u UserSummary
		if err := rows.Scan(&u.Username, &u.GroupName, &u.Requests, &u.PromptTokens, &u.CompletionTokens, &u.TotalTokens, &u.CostUSD, &u.SavedUSD); err != nil {
			return nil, err
		}
		result = append(result, u)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Store) GetDashboardModels(ctx context.Context, since, until time.Time, group, user, model string) ([]ModelSummary, error) {
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT e.model, COALESCE(e.provider, ''),
			COUNT(*),
			COALESCE(SUM(e.total_tokens),0),
			COALESCE(SUM(e.prompt_tokens),0),
			COALESCE(SUM(e.completion_tokens),0),
			COALESCE(SUM(e.cached_input_tokens),0),
			COALESCE(SUM(e.cache_creation_tokens),0),
			COALESCE(ROUND(SUM(%s)::numeric, 2), 0)
		FROM usage_events e
		LEFT JOIN model_pricing p ON e.model = p.model
		WHERE e.timestamp >= $1 AND e.timestamp < $2 AND ($3 = '' OR e.group_name = $3) AND ($4 = '' OR e.username = ANY(string_to_array($4, ','))) AND ($5 = '' OR e.model = $5)
		GROUP BY e.model, COALESCE(e.provider, '')
		ORDER BY SUM(e.total_tokens) DESC`, costUSDExpr), since, until, group, user, model)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []ModelSummary
	for rows.Next() {
		var m ModelSummary
		if err := rows.Scan(&m.Model, &m.Provider, &m.Requests, &m.TotalTokens, &m.PromptTokens, &m.CompletionTokens, &m.CachedInputTokens, &m.CacheCreationTokens, &m.CostUSD); err != nil {
			return nil, err
		}
		result = append(result, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Store) GetDashboardTimeline(ctx context.Context, since, until time.Time, group, user, model, groupBy string) ([]TimelineBucket, error) {
	hours := until.Sub(since).Hours()
	truncInterval := "day"
	if hours <= 48 {
		truncInterval = "hour"
	}

	seriesCol := "e.model"
	if groupBy == "user" {
		seriesCol = "e.username"
	}

	query := fmt.Sprintf(`
		SELECT date_trunc('%s', e.timestamp) as bucket,
			%s as series,
			COALESCE(SUM(e.total_tokens),0),
			COUNT(*)
		FROM usage_events e
		WHERE e.timestamp >= $1 AND e.timestamp < $2 AND ($3 = '' OR e.group_name = $3) AND ($4 = '' OR e.username = ANY(string_to_array($4, ','))) AND ($5 = '' OR e.model = $5)
		GROUP BY bucket, series
		ORDER BY bucket, series`, truncInterval, seriesCol)

	rows, err := s.db.QueryContext(ctx, query, since, until, group, user, model)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []TimelineBucket
	for rows.Next() {
		var t TimelineBucket
		if err := rows.Scan(&t.Bucket, &t.Series, &t.TotalTokens, &t.Requests); err != nil {
			return nil, err
		}
		result = append(result, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

type RecentEvent struct {
	Timestamp           time.Time `json:"timestamp"`
	Username            string    `json:"username"`
	GroupName           string    `json:"group_name"`
	Model               string    `json:"model"`
	Provider            string    `json:"provider"`
	PromptTokens        int       `json:"prompt_tokens"`
	CompletionTokens    int       `json:"completion_tokens"`
	TotalTokens         int       `json:"total_tokens"`
	CachedInputTokens   int       `json:"cached_input_tokens"`
	CacheCreationTokens int       `json:"cache_creation_tokens"`
	CostUSD             float64   `json:"cost_usd"`
	UserAgent           string    `json:"user_agent"`
	// StatusCode is the upstream HTTP status; nil (JSON null) when unknown.
	StatusCode *int `json:"status_code"`
}

func (s *Store) GetRecentEvents(ctx context.Context, limit int, group, user, model string) ([]RecentEvent, error) {
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT e.timestamp, e.username, COALESCE(e.group_name,''), e.model, COALESCE(e.provider,''),
			e.prompt_tokens, e.completion_tokens, e.total_tokens,
			COALESCE(e.cached_input_tokens, 0), COALESCE(e.cache_creation_tokens, 0),
			COALESCE(ROUND((%s)::numeric, 4), 0),
			COALESCE(e.user_agent, ''),
			e.status_code
		FROM usage_events e
		LEFT JOIN model_pricing p ON e.model = p.model
		WHERE ($2 = '' OR e.group_name = $2) AND ($3 = '' OR e.username = ANY(string_to_array($3, ','))) AND ($4 = '' OR e.model = $4)
		ORDER BY e.timestamp DESC
		LIMIT $1`, costUSDExpr), limit, group, user, model)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []RecentEvent
	for rows.Next() {
		var r RecentEvent
		if err := rows.Scan(&r.Timestamp, &r.Username, &r.GroupName, &r.Model, &r.Provider, &r.PromptTokens, &r.CompletionTokens, &r.TotalTokens, &r.CachedInputTokens, &r.CacheCreationTokens, &r.CostUSD, &r.UserAgent, &r.StatusCode); err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Store) migrate(ctx context.Context) error {
	slog.Info("running database migrations")
	for _, stmt := range migrations {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("migration failed: %w", err)
		}
	}
	slog.Info("database migrations complete")
	return nil
}

var migrations = []string{
	`CREATE TABLE IF NOT EXISTS usage_events (
		id BIGSERIAL PRIMARY KEY,
		event_id TEXT NOT NULL,
		timestamp TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		username TEXT NOT NULL,
		group_name TEXT,
		subscription TEXT,
		provider TEXT NOT NULL DEFAULT '',
		model TEXT NOT NULL,
		prompt_tokens INTEGER NOT NULL DEFAULT 0,
		completion_tokens INTEGER NOT NULL DEFAULT 0,
		total_tokens INTEGER NOT NULL DEFAULT 0,
		source TEXT DEFAULT 'maas-gateway',
		cached_input_tokens INTEGER NOT NULL DEFAULT 0,
		cache_creation_tokens INTEGER NOT NULL DEFAULT 0,
		reasoning_tokens INTEGER NOT NULL DEFAULT 0
	)`,
	`ALTER TABLE usage_events ADD COLUMN IF NOT EXISTS cached_input_tokens INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE usage_events ADD COLUMN IF NOT EXISTS cache_creation_tokens INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE usage_events ADD COLUMN IF NOT EXISTS reasoning_tokens INTEGER NOT NULL DEFAULT 0`,
	`CREATE INDEX IF NOT EXISTS idx_usage_events_timestamp ON usage_events (timestamp)`,
	`CREATE INDEX IF NOT EXISTS idx_usage_events_username ON usage_events (username)`,
	`CREATE INDEX IF NOT EXISTS idx_usage_events_group ON usage_events (group_name)`,
	`CREATE INDEX IF NOT EXISTS idx_usage_events_model ON usage_events (model)`,
	`CREATE INDEX IF NOT EXISTS idx_usage_events_ts_user_model ON usage_events (timestamp, username, model)`,
	`ALTER TABLE usage_events ADD COLUMN IF NOT EXISTS user_agent TEXT NOT NULL DEFAULT ''`,
	// Nullable on purpose: rows ingested before this column existed have an
	// unknown status (rendered neutral), while every new row carries a
	// concrete code (200 on success, the upstream code on error).
	`ALTER TABLE usage_events ADD COLUMN IF NOT EXISTS status_code INTEGER`,
	`CREATE TABLE IF NOT EXISTS model_pricing (
		model TEXT PRIMARY KEY,
		provider TEXT NOT NULL,
		input_cost_per_mtok NUMERIC(10,4) NOT NULL DEFAULT 0,
		output_cost_per_mtok NUMERIC(10,4) NOT NULL DEFAULT 0,
		cache_write_cost_per_mtok NUMERIC(10,4) NOT NULL DEFAULT 0,
		cache_read_cost_per_mtok NUMERIC(10,4) NOT NULL DEFAULT 0,
		list_input_cost_per_mtok NUMERIC(10,4) NOT NULL DEFAULT 0,
		list_output_cost_per_mtok NUMERIC(10,4) NOT NULL DEFAULT 0,
		list_cache_write_cost_per_mtok NUMERIC(10,4) NOT NULL DEFAULT 0,
		list_cache_read_cost_per_mtok NUMERIC(10,4) NOT NULL DEFAULT 0
	)`,
	// Idempotent for pre-existing databases (CREATE TABLE IF NOT EXISTS does
	// not add columns to a table that already exists).
	`ALTER TABLE model_pricing ADD COLUMN IF NOT EXISTS list_input_cost_per_mtok NUMERIC(10,4) NOT NULL DEFAULT 0`,
	`ALTER TABLE model_pricing ADD COLUMN IF NOT EXISTS list_output_cost_per_mtok NUMERIC(10,4) NOT NULL DEFAULT 0`,
	`ALTER TABLE model_pricing ADD COLUMN IF NOT EXISTS list_cache_write_cost_per_mtok NUMERIC(10,4) NOT NULL DEFAULT 0`,
	`ALTER TABLE model_pricing ADD COLUMN IF NOT EXISTS list_cache_read_cost_per_mtok NUMERIC(10,4) NOT NULL DEFAULT 0`,
}

// SeedPricing upserts model pricing from an external source (e.g., LiteLLM).
func (s *Store) SeedPricing(ctx context.Context, prices []ModelPrice) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	updated := 0
	for _, p := range prices {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO model_pricing (model, provider, input_cost_per_mtok, output_cost_per_mtok, cache_write_cost_per_mtok, cache_read_cost_per_mtok)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (model) DO UPDATE SET
				provider = EXCLUDED.provider,
				input_cost_per_mtok = EXCLUDED.input_cost_per_mtok,
				output_cost_per_mtok = EXCLUDED.output_cost_per_mtok,
				cache_write_cost_per_mtok = EXCLUDED.cache_write_cost_per_mtok,
				cache_read_cost_per_mtok = EXCLUDED.cache_read_cost_per_mtok`,
			p.Model, p.Provider, p.InputCost, p.OutputCost, p.CacheWriteCost, p.CacheReadCost)
		if err != nil {
			return 0, fmt.Errorf("upsert %s: %w", p.Model, err)
		}
		updated++
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit transaction: %w", err)
	}
	return updated, nil
}

// SeedListPricing sets only the vendor list price (list_* columns) on
// existing model_pricing rows. UPDATE-only by design: it never creates rows,
// so a list-price entry for a model the main seed doesn't know about is a
// no-op rather than a zero-rate row, and it can never clobber actual
// (gateway) rates. Entries whose list rates are all zero are skipped — 0 is
// the "no list price" sentinel the cost query relies on.
func (s *Store) SeedListPricing(ctx context.Context, prices []ModelPrice) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	updated := 0
	for _, p := range prices {
		if p.ListInputCost == 0 && p.ListOutputCost == 0 {
			continue
		}
		res, err := tx.ExecContext(ctx, `
			UPDATE model_pricing SET
				list_input_cost_per_mtok = $2,
				list_output_cost_per_mtok = $3,
				list_cache_write_cost_per_mtok = $4,
				list_cache_read_cost_per_mtok = $5
			WHERE model = $1`,
			p.Model, p.ListInputCost, p.ListOutputCost, p.ListCacheWriteCost, p.ListCacheReadCost)
		if err != nil {
			return 0, fmt.Errorf("update list price %s: %w", p.Model, err)
		}
		n, _ := res.RowsAffected()
		updated += int(n)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit transaction: %w", err)
	}
	return updated, nil
}

// ModelPrice is imported from the pricing package. Re-declared here to avoid
// a circular import — the storage layer doesn't depend on internal/pricing.
// List* fields carry the vendor list price (per MTok). Zero means "no list
// price seeded" — the cost query's NULLIF fallback treats 0 as absent and
// falls back to the effective gateway rate, so saved_usd is exactly 0.
type ModelPrice struct {
	Model              string
	Provider           string
	InputCost          float64
	OutputCost         float64
	CacheWriteCost     float64
	CacheReadCost      float64
	ListInputCost      float64
	ListOutputCost     float64
	ListCacheWriteCost float64
	ListCacheReadCost  float64
}

// GetCurrentPricing returns all model pricing from the database.
func (s *Store) GetCurrentPricing(ctx context.Context) ([]ModelPrice, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT model, provider, input_cost_per_mtok, output_cost_per_mtok, cache_write_cost_per_mtok, cache_read_cost_per_mtok FROM model_pricing ORDER BY model`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var prices []ModelPrice
	for rows.Next() {
		var p ModelPrice
		if err := rows.Scan(&p.Model, &p.Provider, &p.InputCost, &p.OutputCost, &p.CacheWriteCost, &p.CacheReadCost); err != nil {
			return nil, err
		}
		prices = append(prices, p)
	}
	return prices, nil
}
