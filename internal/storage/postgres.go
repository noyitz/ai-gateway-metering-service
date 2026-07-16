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
	EventID          string
	Timestamp        time.Time
	Username         string
	GroupName        string
	Subscription     string
	Provider         string
	Model            string
	PromptTokens     int
	CompletionTokens    int
	TotalTokens         int
	CachedInputTokens   int
	CacheCreationTokens int
	ReasoningTokens     int
	Source              string
}

type UsageStats struct {
	HasAccess bool    `json:"hasAccess"`
	Balance   float64 `json:"balance"`
	Usage     float64 `json:"usage"`
	Overage   float64 `json:"overage"`
}

type Store struct {
	db *sql.DB
}

func New(databaseURL string) (*Store, error) {
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

	s := &Store{db: db}
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
		`INSERT INTO usage_events (event_id, timestamp, username, group_name, subscription, provider, model, prompt_tokens, completion_tokens, total_tokens, cached_input_tokens, cache_creation_tokens, reasoning_tokens, source)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
		e.EventID, e.Timestamp, e.Username, e.GroupName, e.Subscription, e.Provider, e.Model,
		e.PromptTokens, e.CompletionTokens, e.TotalTokens, e.CachedInputTokens, e.CacheCreationTokens, e.ReasoningTokens, e.Source,
	)
	return err
}

type TeamUserUsage struct {
	Username         string             `json:"username"`
	Requests         int                `json:"requests"`
	PromptTokens     int                `json:"prompt_tokens"`
	CompletionTokens int                `json:"completion_tokens"`
	TotalTokens      int                `json:"total_tokens"`
	CostUSD          float64            `json:"cost_usd"`
	Models           []ModelUsage       `json:"models"`
}

type ModelUsage struct {
	Model            string  `json:"model"`
	Provider         string  `json:"provider"`
	Requests         int     `json:"requests"`
	TotalTokens      int     `json:"total_tokens"`
	CostUSD          float64 `json:"cost_usd"`
}

func (s *Store) GetTeamUsage(ctx context.Context, groupName string) ([]TeamUserUsage, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT e.username, e.model, e.provider,
			COUNT(*) as requests,
			SUM(e.prompt_tokens) as prompt_tokens,
			SUM(e.completion_tokens) as completion_tokens,
			SUM(e.total_tokens) as total_tokens,
			ROUND(SUM(GREATEST(e.prompt_tokens - COALESCE(e.cached_input_tokens, 0), 0) * COALESCE(p.input_cost_per_mtok, 15)/1000000.0 +
				COALESCE(e.cached_input_tokens, 0) * COALESCE(p.cache_read_cost_per_mtok, 0.5)/1000000.0 +
				COALESCE(e.cache_creation_tokens, 0) * COALESCE(p.cache_write_cost_per_mtok, 0)/1000000.0 +
				e.completion_tokens * COALESCE(p.output_cost_per_mtok, 75)/1000000.0)::numeric, 4) as cost_usd
		FROM usage_events e
		LEFT JOIN model_pricing p ON e.model = p.model
		WHERE e.group_name = $1
		GROUP BY e.username, e.model, e.provider
		ORDER BY e.username, total_tokens DESC`, groupName)
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
	if err := rows.Err(); err != nil { return nil, err }
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

	quota := float64(100000000)
	usage := float64(used)
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
	}, nil
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
}

type ModelSummary struct {
	Model              string  `json:"model"`
	Provider           string  `json:"provider"`
	Requests           int     `json:"requests"`
	TotalTokens        int64   `json:"total_tokens"`
	PromptTokens       int64   `json:"prompt_tokens"`
	CompletionTokens   int64   `json:"completion_tokens"`
	CachedInputTokens  int64   `json:"cached_input_tokens"`
	CacheCreationTokens int64  `json:"cache_creation_tokens"`
	CostUSD            float64 `json:"cost_usd"`
}

type TimelineBucket struct {
	Bucket      time.Time `json:"bucket"`
	Series      string    `json:"series"`
	TotalTokens int64     `json:"total_tokens"`
	Requests    int       `json:"requests"`
}

func (s *Store) GetDashboardOverview(ctx context.Context, since time.Time) (DashboardOverview, error) {
	var o DashboardOverview
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*),
			COALESCE(SUM(e.prompt_tokens),0),
			COALESCE(SUM(e.completion_tokens),0),
			COALESCE(SUM(e.total_tokens),0),
			COUNT(DISTINCT e.username),
			COALESCE(ROUND(SUM(
				GREATEST(e.prompt_tokens - COALESCE(e.cached_input_tokens, 0), 0) * COALESCE(p.input_cost_per_mtok, 15)/1000000.0 +
				COALESCE(e.cached_input_tokens, 0) * COALESCE(p.cache_read_cost_per_mtok, 0.5)/1000000.0 +
				COALESCE(e.cache_creation_tokens, 0) * COALESCE(p.cache_write_cost_per_mtok, 0)/1000000.0 +
				e.completion_tokens * COALESCE(p.output_cost_per_mtok, 75)/1000000.0
			)::numeric, 2), 0)
		FROM usage_events e
		LEFT JOIN model_pricing p ON e.model = p.model
		WHERE e.timestamp >= $1`, since).Scan(
		&o.TotalRequests, &o.TotalPromptTokens, &o.TotalCompletionTokens,
		&o.TotalTokens, &o.ActiveUsers, &o.TotalCostUSD)
	return o, err
}

func (s *Store) GetDashboardGroups(ctx context.Context, since time.Time) ([]GroupSummary, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT COALESCE(e.group_name, 'unknown'),
			COUNT(*),
			COALESCE(SUM(e.total_tokens),0),
			COUNT(DISTINCT e.username),
			COALESCE(ROUND(SUM(
				GREATEST(e.prompt_tokens - COALESCE(e.cached_input_tokens, 0), 0) * COALESCE(p.input_cost_per_mtok, 15)/1000000.0 +
				COALESCE(e.cached_input_tokens, 0) * COALESCE(p.cache_read_cost_per_mtok, 0.5)/1000000.0 +
				COALESCE(e.cache_creation_tokens, 0) * COALESCE(p.cache_write_cost_per_mtok, 0)/1000000.0 +
				e.completion_tokens * COALESCE(p.output_cost_per_mtok, 75)/1000000.0
			)::numeric, 2), 0)
		FROM usage_events e
		LEFT JOIN model_pricing p ON e.model = p.model
		WHERE e.timestamp >= $1
		GROUP BY COALESCE(e.group_name, 'unknown')
		ORDER BY SUM(e.total_tokens) DESC`, since)
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
	if err := rows.Err(); err != nil { return nil, err }
	return result, nil
}

func (s *Store) GetDashboardUsers(ctx context.Context, since time.Time, group, user, model, sortCol, sortOrder string, limit int) ([]UserSummary, error) {
	validSorts := map[string]string{
		"total_tokens": "total_tokens", "cost_usd": "cost_usd",
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
			COALESCE(ROUND(SUM(
				GREATEST(e.prompt_tokens - COALESCE(e.cached_input_tokens, 0), 0) * COALESCE(p.input_cost_per_mtok, 15)/1000000.0 +
				COALESCE(e.cached_input_tokens, 0) * COALESCE(p.cache_read_cost_per_mtok, 0.5)/1000000.0 +
				COALESCE(e.cache_creation_tokens, 0) * COALESCE(p.cache_write_cost_per_mtok, 0)/1000000.0 +
				e.completion_tokens * COALESCE(p.output_cost_per_mtok, 75)/1000000.0
			)::numeric, 2), 0) as cost_usd
		FROM usage_events e
		LEFT JOIN model_pricing p ON e.model = p.model
		WHERE e.timestamp >= $1 AND ($2 = '' OR e.group_name = $2) AND ($4 = '' OR e.username = $4) AND ($5 = '' OR e.model = $5)
		GROUP BY e.username, COALESCE(e.group_name, '')
		ORDER BY %s %s
		LIMIT $3`, sortExpr, direction)

	rows, err := s.db.QueryContext(ctx, query, since, group, limit, user, model)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []UserSummary
	for rows.Next() {
		var u UserSummary
		if err := rows.Scan(&u.Username, &u.GroupName, &u.Requests, &u.PromptTokens, &u.CompletionTokens, &u.TotalTokens, &u.CostUSD); err != nil {
			return nil, err
		}
		result = append(result, u)
	}
	if err := rows.Err(); err != nil { return nil, err }
	return result, nil
}

func (s *Store) GetDashboardModels(ctx context.Context, since time.Time, username string) ([]ModelSummary, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT e.model, COALESCE(e.provider, ''),
			COUNT(*),
			COALESCE(SUM(e.total_tokens),0),
			COALESCE(SUM(e.prompt_tokens),0),
			COALESCE(SUM(e.completion_tokens),0),
			COALESCE(SUM(e.cached_input_tokens),0),
			COALESCE(SUM(e.cache_creation_tokens),0),
			COALESCE(ROUND(SUM(
				GREATEST(e.prompt_tokens - COALESCE(e.cached_input_tokens, 0), 0) * COALESCE(p.input_cost_per_mtok, 15)/1000000.0 +
				COALESCE(e.cached_input_tokens, 0) * COALESCE(p.cache_read_cost_per_mtok, 0.5)/1000000.0 +
				COALESCE(e.cache_creation_tokens, 0) * COALESCE(p.cache_write_cost_per_mtok, 0)/1000000.0 +
				e.completion_tokens * COALESCE(p.output_cost_per_mtok, 75)/1000000.0
			)::numeric, 2), 0)
		FROM usage_events e
		LEFT JOIN model_pricing p ON e.model = p.model
		WHERE e.timestamp >= $1 AND ($2 = '' OR e.username = $2)
		GROUP BY e.model, COALESCE(e.provider, '')
		ORDER BY SUM(e.total_tokens) DESC`, since, username)
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
	if err := rows.Err(); err != nil { return nil, err }
	return result, nil
}

func (s *Store) GetDashboardTimeline(ctx context.Context, since time.Time, groupBy string) ([]TimelineBucket, error) {
	hours := time.Since(since).Hours()
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
		WHERE e.timestamp >= $1
		GROUP BY bucket, series
		ORDER BY bucket, series`, truncInterval, seriesCol)

	rows, err := s.db.QueryContext(ctx, query, since)
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
	if err := rows.Err(); err != nil { return nil, err }
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
}

func (s *Store) GetRecentEvents(ctx context.Context, limit int) ([]RecentEvent, error) {
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT e.timestamp, e.username, COALESCE(e.group_name,''), e.model, COALESCE(e.provider,''),
			e.prompt_tokens, e.completion_tokens, e.total_tokens,
			COALESCE(e.cached_input_tokens, 0), COALESCE(e.cache_creation_tokens, 0),
			COALESCE(ROUND((
				GREATEST(e.prompt_tokens - COALESCE(e.cached_input_tokens, 0), 0) * COALESCE(p.input_cost_per_mtok, 15)/1000000.0 +
				COALESCE(e.cached_input_tokens, 0) * COALESCE(p.cache_read_cost_per_mtok, 0.5)/1000000.0 +
				COALESCE(e.cache_creation_tokens, 0) * COALESCE(p.cache_write_cost_per_mtok, 0)/1000000.0 +
				e.completion_tokens * COALESCE(p.output_cost_per_mtok, 75)/1000000.0
			)::numeric, 4), 0)
		FROM usage_events e
		LEFT JOIN model_pricing p ON e.model = p.model
		ORDER BY e.timestamp DESC
		LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []RecentEvent
	for rows.Next() {
		var r RecentEvent
		if err := rows.Scan(&r.Timestamp, &r.Username, &r.GroupName, &r.Model, &r.Provider, &r.PromptTokens, &r.CompletionTokens, &r.TotalTokens, &r.CachedInputTokens, &r.CacheCreationTokens, &r.CostUSD); err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	if err := rows.Err(); err != nil { return nil, err }
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
	`CREATE TABLE IF NOT EXISTS model_pricing (
		model TEXT PRIMARY KEY,
		provider TEXT NOT NULL,
		input_cost_per_mtok NUMERIC(10,4) NOT NULL DEFAULT 0,
		output_cost_per_mtok NUMERIC(10,4) NOT NULL DEFAULT 0,
		cache_write_cost_per_mtok NUMERIC(10,4) NOT NULL DEFAULT 0,
		cache_read_cost_per_mtok NUMERIC(10,4) NOT NULL DEFAULT 0
	)`,
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

// ModelPrice is imported from the pricing package. Re-declared here to avoid
// a circular import — the storage layer doesn't depend on internal/pricing.
type ModelPrice struct {
	Model          string
	Provider       string
	InputCost      float64
	OutputCost     float64
	CacheWriteCost float64
	CacheReadCost  float64
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
