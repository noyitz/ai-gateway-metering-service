package storage

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"sync"
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

// UsageStats is the entitlement endpoint's response body. The gateway's
// external_metering filter parses ONLY hasAccess (serde ignores every other
// field), so the quota fields below are additive: safe for the deployed
// filters, useful for the dashboard. A denial is a 429 in the gateway with a
// fixed plaintext body — this JSON never reaches the client.
type UsageStats struct {
	HasAccess bool    `json:"hasAccess"`
	Balance   float64 `json:"balance"`
	Usage     float64 `json:"usage"`
	Overage   float64 `json:"overage"`

	// Dollar quota (see quota.go). QuotaUSD is the effective monthly limit
	// including grants; SpendUSD the month-to-date spend on the same basis
	// the dashboard shows. MonthEnds is when the month's budget resets.
	QuotaUSD  float64 `json:"quotaUsd"`
	SpendUSD  float64 `json:"spendUsd"`
	MonthEnds string  `json:"monthEnds"`
}

type Store struct {
	db *sql.DB

	// readDB is the optional read-replica pool (Phase 2 of
	// docs/dashboard-scaling-plan.md). The allowlist is the set of
	// functions that call s.reader() — dashboard/report reads only.
	// Everything money-adjacent (GetMonthlyUsage, quota reads, the
	// entitlement path) and GetRecentEvents stay on s.db: a lagging or
	// pre-promotion replica must never serve an enforcement decision, and
	// Recent is a freshness feature, not a throughput one. Unset (nil)
	// means every read uses the primary, exactly as before.
	readDB *sql.DB

	// tokenQuota is the per-user monthly token budget enforced by the
	// entitlement endpoint. A value <= 0 means unlimited: the service
	// reports usage but does not gate access. It stays as an outer safety
	// net alongside the dollar quota in quota.go.
	tokenQuota int64

	// quotaCache memoises the entitlement decision per person for
	// quotaCacheTTL; see QuotaDecisionCached. Guarded by quotaMu.
	quotaMu    sync.Mutex
	quotaCache map[string]quotaCacheEntry
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
	if s.readDB != nil {
		_ = s.readDB.Close()
	}
	return s.db.Close()
}

// reader returns the replica pool when one is configured, the primary
// otherwise. See the readDB field comment for the allowlist rule.
func (s *Store) reader() *sql.DB {
	if s.readDB != nil {
		return s.readDB
	}
	return s.db
}

// UseReadReplica opens a second pool for the CNPG read service (the `-r`
// endpoint, which routes to healthy replicas and falls back to the
// primary during failover). It pings before accepting — a replica DSN
// that can't answer is a configuration error, not a reason to refuse to
// boot, so callers log and continue on the primary.
func (s *Store) UseReadReplica(databaseURL string) error {
	db, err := sql.Open("postgres", databaseURL)
	if err != nil {
		return fmt.Errorf("open read replica: %w", err)
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(14400 * time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return fmt.Errorf("ping read replica: %w", err)
	}
	s.readDB = db
	return nil
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

// hostedProviderCond marks self-hosted models — everything served over our
// own vLLM routes plus the legacy "qwen" alias row. Hosted traffic is
// identified by provider, not by price: it bills at OpenRouter parity now,
// so a $0 check would miss it. The metered side of the test (e.provider
// LIKE 'qwen-%') covers the live route labels (qwen-flash) so hosted traffic
// is still hosted even for a model variant missing its seeded pricing row.
// Requires usage_events aliased as `e` and model_pricing as `p`.
// Every query that splices this in goes through fmt.Sprintf, so the LIKE
// wildcard percent is doubled.
const hostedProviderCond = `COALESCE(p.provider,'') IN ('vllm','qwen') OR COALESCE(e.provider,'') LIKE 'qwen-%%'`

// displayNameExpr resolves a user's "First Last" from user_profiles, or
// NULL when the profile is missing or has no names — callers fall back to
// the username. Requires usage_events aliased as `e` and the
// `LEFT JOIN user_profiles up ON up.username = e.username` join.
const displayNameExpr = `NULLIF(TRIM(COALESCE(up.first_name, '') || ' ' || COALESCE(up.last_name, '')), '')`

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

// GetMonthlyUsage backs the gateway's blocking entitlement subrequest.
// hasAccess is the AND of two independent gates: the legacy token budget
// (a 10B-token outer safety net; tokenQuota <= 0 disables it) and the
// dollar quota from quota.go (inert while the policy's enforced flag is
// false, and never applied to exempt callers — super-admins). Errors
// propagate as a 5xx, which the gateway treats as metering unavailability
// and admits the request (fail_open) — so a metering outage disables
// enforcement rather than blocking traffic. That is the accepted tradeoff.
func (s *Store) GetMonthlyUsage(ctx context.Context, username, model string, exempt bool) (UsageStats, error) {
	var used int64
	row := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(total_tokens), 0) FROM usage_events
		 WHERE username = $1 AND timestamp >= date_trunc('month', NOW())`,
		username,
	)
	if err := row.Scan(&used); err != nil {
		return UsageStats{}, err
	}

	stats := computeUsageStats(used, s.tokenQuota)

	decision, err := s.QuotaDecisionCached(ctx, username, exempt)
	if err != nil {
		return UsageStats{}, err
	}
	stats.QuotaUSD = decision.LimitUSD
	stats.SpendUSD = decision.SpentUSD
	stats.MonthEnds = decision.MonthEnds.UTC().Format(time.RFC3339)
	stats.HasAccess = stats.HasAccess && decision.Allowed()
	return stats, nil
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
	// Saved · Hosted Models KPI, computed server-side as the exact sum of
	// the per-user saved_usd column (GetHostedSavings) so the card and the
	// table can never disagree. Ratio/RatioApplied back the sub-label.
	SavedUSD     float64 `json:"saved_usd"`
	SavingsRatio float64 `json:"savings_ratio"`
	RatioApplied bool    `json:"savings_ratio_applied"`
}

type GroupSummary struct {
	GroupName   string  `json:"group_name"`
	Requests    int     `json:"requests"`
	TotalTokens int64   `json:"total_tokens"`
	UserCount   int     `json:"user_count"`
	CostUSD     float64 `json:"cost_usd"`
}

type UserSummary struct {
	Username string `json:"username"`
	// DisplayName is "First Last" from user_profiles; empty when unknown,
	// in which case the UI renders the username itself.
	DisplayName      string  `json:"display_name,omitempty"`
	GroupName        string  `json:"group_name"`
	Requests         int     `json:"requests"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
	CostUSD          float64 `json:"cost_usd"`
	// SavedUSD is the per-user share of the "Saved · Hosted Models" KPI:
	// what this user's hosted-model traffic (provider vllm / legacy qwen)
	// would have cost on the reference model (default claude-opus-4-8),
	// priced cache-aware at the reference's seeded rates, MINUS what it was
	// actually billed (hosted models bill at OpenRouter parity, not $0),
	// floored at 0 per model. Hosted traffic that carries no cache telemetry
	// is split using the cache ratio observed on vendor traffic in the same
	// window (reference model first, all vendor paid as fallback). Computed
	// with the same arithmetic the dashboard KPI applies, so the user-table
	// column and the KPI card always agree.
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
	// Hosted is the server's verdict (hostedProviderCond) that this model's
	// traffic ran on our own routes. The metered Provider field is the route
	// label — e.g. qwen-flash — which drifts from the seeded pricing provider
	// (vllm), so anything that needs "is this hosted?" must trust this flag,
	// not Provider. (The Hosted-vs-Vendor pie learned this the hard way.)
	Hosted bool `json:"hosted"`
	// Seeded per-model rates ($/Mtok) from model_pricing. The dashboard's
	// savings KPI uses these instead of its hardcoded JS map so the card and
	// the server-computed per-user savings column share one price source.
	// 0 when the model has no seeded pricing row (client falls back to its map).
	InputPrice      float64 `json:"input_price_per_mtok"`
	OutputPrice     float64 `json:"output_price_per_mtok"`
	CacheReadPrice  float64 `json:"cache_read_price_per_mtok"`
	CacheWritePrice float64 `json:"cache_write_price_per_mtok"`
}

type TimelineBucket struct {
	Bucket      time.Time `json:"bucket"`
	Series      string    `json:"series"`
	TotalTokens int64     `json:"total_tokens"`
	Requests    int       `json:"requests"`
}

func (s *Store) GetDashboardOverview(ctx context.Context, since, until time.Time, group, user, model string) (DashboardOverview, error) {
	var o DashboardOverview
	err := s.reader().QueryRowContext(ctx, fmt.Sprintf(`
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
	rows, err := s.reader().QueryContext(ctx, fmt.Sprintf(`
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

// GetDashboardUsers returns per-user usage stats. refModel selects the
// reference model for the SavedUSD counterfactual (empty = claude-opus-4-8);
// keep it in sync with the dashboard's savings reference selector.
func (s *Store) GetDashboardUsers(ctx context.Context, since, until time.Time, group, user, model, sortCol, sortOrder string, limit int, refModel string) ([]UserSummary, error) {
	if refModel == "" {
		refModel = "claude-opus-4-8"
	}
	validSorts := map[string]string{
		"total_tokens": "total_tokens", "cost_usd": "cost_usd", "saved_usd": "saved_usd",
		"requests": "requests",
		// The user column sorts by display name (falling back to username),
		// matching what the table shows.
		"username":      `COALESCE(` + displayNameExpr + `, e.username)`,
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

	query := hostedSavingsWithSQL(7) + fmt.Sprintf(`
		SELECT e.username,
			%s,
			COALESCE(e.group_name, ''),
			COUNT(*) as requests,
			COALESCE(SUM(e.prompt_tokens),0) as prompt_tokens,
			COALESCE(SUM(e.completion_tokens),0) as completion_tokens,
			COALESCE(SUM(e.total_tokens),0) as total_tokens,
			COALESCE(ROUND(SUM(%s)::numeric, 2), 0) as cost_usd,
			COALESCE(ROUND(MAX(sv.saved)::numeric, 2), 0) as saved_usd
		FROM usage_events e
		LEFT JOIN model_pricing p ON e.model = p.model
		LEFT JOIN user_profiles up ON up.username = e.username
		LEFT JOIN sv ON sv.username = e.username
		WHERE e.timestamp >= $1 AND e.timestamp < $2 AND ($3 = '' OR e.group_name = $3) AND ($4 = '' OR e.username = ANY(string_to_array($4, ','))) AND ($5 = '' OR e.model = $5)
		GROUP BY e.username, %s, COALESCE(e.group_name, '')
		ORDER BY %s %s
		LIMIT $6`, displayNameExpr, costUSDExpr, displayNameExpr, sortExpr, direction)

	rows, err := s.reader().QueryContext(ctx, query, since, until, group, user, model, limit, refModel)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []UserSummary
	for rows.Next() {
		var u UserSummary
		var displayName sql.NullString
		if err := rows.Scan(&u.Username, &displayName, &u.GroupName, &u.Requests, &u.PromptTokens, &u.CompletionTokens, &u.TotalTokens, &u.CostUSD, &u.SavedUSD); err != nil {
			return nil, err
		}
		u.DisplayName = displayName.String
		result = append(result, u)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// hostedSavingsWithSQL is the single source of truth for the
// "Saved · Hosted Models" counterfactual. Both the per-user table column
// (GetDashboardUsers) and the org-wide KPI (GetHostedSavings) run THIS SQL,
// so the card can never disagree with the column beneath it — the split
// this replaced computed the KPI client-side over per-MODEL aggregates,
// where the "no cache telemetry" heuristic (apply the observed vendor cache
// ratio) fires or not depending on whether ANY user reported cache for a
// model, while the per-user column decided per user×model pair. A single
// 8,800-token cached row once swung the org KPI by thousands of dollars.
//
//	r_ref/r_all  observed cache ratio (cached/prompt) of VENDOR (paid,
//	             non-hosted) traffic in the window — range-only
//	             (unfiltered), like the client's unfiltered model
//	             catalog; reference model preferred, all vendor paid as
//	             fallback. Hosted providers (vllm / legacy qwen) are
//	             excluded: they carry no cache telemetry, so including
//	             them once they bill nonzero would drag the observed
//	             ratio toward 0.
//	pr           reference model rates from model_pricing (aggregated so
//	             the CTE always yields exactly one row).
//	model_cache  per-MODEL cache fraction across the whole window,
//	             unfiltered by group/user/model (same convention as
//	             r_ref/r_all) — whether a model reports cache telemetry AT
//	             ALL is a property of the model/route, not of any one
//	             user's traffic through it. Below cacheNoiseThreshold the
//	             model is treated as reporting NO cache telemetry, full
//	             stop, even for the rare user×model pair whose own sum
//	             happens to be nonzero. This is deliberate: one stray
//	             8,800-cached-token event out of 875M prompt tokens on
//	             Inferact/Qwen3.8-Flash-Next-NVFP4 (every one of that
//	             model's other ~9,800 events across 17 users reports
//	             exactly 0) used to flip the estimate off for that ONE
//	             user only, pricing their traffic at the full input rate
//	             while everyone else on the identical model got the 95%
//	             cache assumption — a few noise tokens costing that user
//	             ~$1,850 of counterfactual savings relative to a peer with
//	             materially identical usage. Deciding per model instead of
//	             per user×model closes that gap. Models that genuinely
//	             report cache (Qwen3.8-27B-FP8 at 51%, qwen38-flash-next
//	             at 83%) sit far above the threshold and keep using their
//	             real, literal numbers.
//	fm           per-user-per-model totals over the active filters, plus
//	             a `hosted` flag (provider vllm / qwen seed or the qwen-*
//	             route label) and the model's hasCache verdict. A model
//	             also counts as hosted when its metered cost is 0 with
//	             real tokens.
//	sv           per-user savings: for each hosted model, what its traffic
//	             would have cost on the reference rates minus what it was
//	             actually billed (hosted models bill at OpenRouter parity,
//	             not $0), floored at 0 per user×model so one model can
//	             never cancel another's saving. Cache-aware: cache-write
//	             tokens are deducted from the fresh-input term so each
//	             tier is priced once; a hosted row on a model with no real
//	             cache telemetry gets the observed ratio applied, no
//	             matter what that row's own literal cached count reads.
//
// Positional params: $1 since, $2 until, $3 group, $4 user, $5 model,
// refParamNum reference model — $7 for GetDashboardUsers (whose $6 is the
// user-table LIMIT), $6 for GetHostedSavings (which has no LIMIT). A
// placeholder number that never appears in the query text at all makes
// Postgres refuse the query outright ("could not determine data type of
// parameter"), which is why this can't just always say $7.
func hostedSavingsWithSQL(refParamNum int) string {
	ref := fmt.Sprintf("$%d", refParamNum)
	// Inlined as a literal (not a %v verb) so it can never land in the
	// wrong Sprintf slot the way a positional argument can — Sprintf
	// matches args to verbs in the order the verbs appear in the format
	// string, not in argument-list order, and this function already has
	// several %s verbs ahead of where the threshold is used.
	const cacheNoiseThresholdSQL = "0.01" // 1% of prompt tokens; see model_cache comment
	return fmt.Sprintf(`
		WITH r_ref AS (
			SELECT COALESCE(SUM(e.cached_input_tokens)::float / NULLIF(SUM(e.prompt_tokens), 0), 0) as r
			FROM usage_events e LEFT JOIN model_pricing p ON e.model = p.model
			WHERE e.timestamp >= $1 AND e.timestamp < $2 AND e.model = `+ref+` AND (%s) > 0
			  AND NOT (`+hostedProviderCond+`)
		),
		r_all AS (
			SELECT COALESCE(SUM(e.cached_input_tokens)::float / NULLIF(SUM(e.prompt_tokens), 0), 0) as r
			FROM usage_events e LEFT JOIN model_pricing p ON e.model = p.model
			WHERE e.timestamp >= $1 AND e.timestamp < $2 AND (%s) > 0
			  AND NOT (`+hostedProviderCond+`)
		),
		rat AS (
			SELECT CASE WHEN (SELECT r FROM r_ref) > 0 THEN (SELECT r FROM r_ref)
			            WHEN (SELECT r FROM r_all) > 0 THEN (SELECT r FROM r_all)
			            ELSE 0 END as r
		),
		pr AS (
			SELECT COALESCE(MAX(input_cost_per_mtok), 0) as i, COALESCE(MAX(output_cost_per_mtok), 0) as o,
			       COALESCE(MAX(cache_read_cost_per_mtok), 0) as cr, COALESCE(MAX(cache_write_cost_per_mtok), 0) as cw
			FROM model_pricing WHERE model = `+ref+`
		),
		model_cache AS (
			SELECT e.model,
			       (SUM(COALESCE(e.cached_input_tokens, 0) + COALESCE(e.cache_creation_tokens, 0))::float
			         / NULLIF(SUM(e.prompt_tokens), 0)) > `+cacheNoiseThresholdSQL+` AS has_cache
			FROM usage_events e
			WHERE e.timestamp >= $1 AND e.timestamp < $2
			GROUP BY e.model
		),
		fm AS (
			SELECT e.username, e.model,
				SUM(e.prompt_tokens) as prompt, SUM(e.completion_tokens) as completion,
				SUM(COALESCE(e.cached_input_tokens, 0)) as cached, SUM(COALESCE(e.cache_creation_tokens, 0)) as cwrite,
				SUM(e.total_tokens) as tot, SUM(%s) as cost,
				bool_or(`+hostedProviderCond+`) as hosted,
				COALESCE(bool_and(COALESCE(mc.has_cache, false)), false) as model_has_cache
			FROM usage_events e LEFT JOIN model_pricing p ON e.model = p.model
			LEFT JOIN model_cache mc ON mc.model = e.model
			WHERE e.timestamp >= $1 AND e.timestamp < $2 AND ($3 = '' OR e.group_name = $3) AND ($4 = '' OR e.username = ANY(string_to_array($4, ','))) AND ($5 = '' OR e.model = $5)
			GROUP BY e.username, e.model
		),
		sv AS (
			SELECT fm.username, SUM(GREATEST(
				(GREATEST(fm.prompt
					- CASE WHEN NOT fm.model_has_cache AND fm.prompt > 0 AND rat.r > 0
					       THEN LEAST(ROUND(fm.prompt * rat.r), fm.prompt)
					       ELSE fm.cached END
					- fm.cwrite, 0) * pr.i
				+ CASE WHEN NOT fm.model_has_cache AND fm.prompt > 0 AND rat.r > 0
				       THEN LEAST(ROUND(fm.prompt * rat.r), fm.prompt)
				       ELSE fm.cached END * pr.cr
				+ fm.cwrite * pr.cw
				+ fm.completion * pr.o) / 1000000.0
				- fm.cost
			, 0)) as saved
			FROM fm CROSS JOIN pr CROSS JOIN rat
			WHERE (fm.hosted OR fm.cost = 0) AND fm.tot > 0
			GROUP BY fm.username
		)`, costUSDExpr, costUSDExpr, costUSDExpr)
}

// GetHostedSavings returns the org-wide "Saved · Hosted Models" KPI: the
// exact sum of the per-user saved_usd column (same CTE, same floors) over
// the active filters, so the KPI card equals the table underneath it by
// construction. ratio/ratioApplied back the "N% input est. cached"
// sub-label — the estimate the counterfactual had to make for hosted
// traffic that reports no cache telemetry.
func (s *Store) GetHostedSavings(ctx context.Context, since, until time.Time, group, user, model, refModel string) (saved, ratio float64, ratioApplied bool, err error) {
	if refModel == "" {
		refModel = "claude-opus-4-8"
	}
	query := hostedSavingsWithSQL(6) + `,
		sa AS (SELECT COALESCE(SUM(saved), 0) as saved FROM sv),
		ra AS (SELECT COALESCE(bool_or(cached = 0 AND cwrite = 0 AND prompt > 0 AND (SELECT r FROM rat) > 0), false) as applied
		       FROM fm WHERE (hosted OR cost = 0) AND tot > 0)
		SELECT COALESCE(ROUND((SELECT saved FROM sa)::numeric, 2), 0)::float8,
		       (SELECT r FROM rat), (SELECT applied FROM ra)`
	err = s.reader().QueryRowContext(ctx, query, since, until, group, user, model, refModel).Scan(&saved, &ratio, &ratioApplied)
	return saved, ratio, ratioApplied, err
}

func (s *Store) GetDashboardModels(ctx context.Context, since, until time.Time, group, user, model string) ([]ModelSummary, error) {
	rows, err := s.reader().QueryContext(ctx, fmt.Sprintf(`
		SELECT e.model, COALESCE(e.provider, ''),
			COUNT(*),
			COALESCE(SUM(e.total_tokens),0),
			COALESCE(SUM(e.prompt_tokens),0),
			COALESCE(SUM(e.completion_tokens),0),
			COALESCE(SUM(e.cached_input_tokens),0),
			COALESCE(SUM(e.cache_creation_tokens),0),
			COALESCE(ROUND(SUM(%s)::numeric, 2), 0),
			bool_or(`+hostedProviderCond+`),
			COALESCE(MAX(p.input_cost_per_mtok), 0),
			COALESCE(MAX(p.output_cost_per_mtok), 0),
			COALESCE(MAX(p.cache_read_cost_per_mtok), 0),
			COALESCE(MAX(p.cache_write_cost_per_mtok), 0)
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
		if err := rows.Scan(&m.Model, &m.Provider, &m.Requests, &m.TotalTokens, &m.PromptTokens, &m.CompletionTokens, &m.CachedInputTokens, &m.CacheCreationTokens, &m.CostUSD,
			&m.Hosted, &m.InputPrice, &m.OutputPrice, &m.CacheReadPrice, &m.CacheWritePrice); err != nil {
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

	rows, err := s.reader().QueryContext(ctx, query, since, until, group, user, model)
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
	Timestamp time.Time `json:"timestamp"`
	Username  string    `json:"username"`
	// DisplayName is "First Last" from user_profiles; empty when unknown.
	DisplayName         string  `json:"display_name,omitempty"`
	GroupName           string  `json:"group_name"`
	Model               string  `json:"model"`
	Provider            string  `json:"provider"`
	PromptTokens        int     `json:"prompt_tokens"`
	CompletionTokens    int     `json:"completion_tokens"`
	TotalTokens         int     `json:"total_tokens"`
	CachedInputTokens   int     `json:"cached_input_tokens"`
	CacheCreationTokens int     `json:"cache_creation_tokens"`
	CostUSD             float64 `json:"cost_usd"`
	UserAgent           string  `json:"user_agent"`
	// StatusCode is the HTTP status on the row; nil (JSON null) when unknown.
	// Quota blocks are recorded here too — RecordQuotaDenial inserts a real
	// usage_events row with status 429, so denials flow through this feed like
	// any other error row.
	StatusCode *int `json:"status_code"`
}

func (s *Store) GetRecentEvents(ctx context.Context, limit int, group, user, model string) ([]RecentEvent, error) {
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT e.timestamp, e.username, %s, COALESCE(e.group_name,''), e.model, COALESCE(e.provider,''),
			e.prompt_tokens, e.completion_tokens, e.total_tokens,
			COALESCE(e.cached_input_tokens, 0), COALESCE(e.cache_creation_tokens, 0),
			COALESCE(ROUND((%s)::numeric, 4), 0),
			COALESCE(e.user_agent, ''),
			e.status_code
		FROM usage_events e
		LEFT JOIN model_pricing p ON e.model = p.model
		LEFT JOIN user_profiles up ON up.username = e.username
		WHERE ($2 = '' OR e.group_name = $2) AND ($3 = '' OR e.username = ANY(string_to_array($3, ','))) AND ($4 = '' OR e.model = $4)
		ORDER BY e.timestamp DESC
		LIMIT $1`, displayNameExpr, costUSDExpr), limit, group, user, model)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []RecentEvent
	for rows.Next() {
		var r RecentEvent
		var displayName sql.NullString
		if err := rows.Scan(&r.Timestamp, &r.Username, &displayName, &r.GroupName, &r.Model, &r.Provider, &r.PromptTokens, &r.CompletionTokens, &r.TotalTokens, &r.CachedInputTokens, &r.CacheCreationTokens, &r.CostUSD, &r.UserAgent, &r.StatusCode); err != nil {
			return nil, err
		}
		r.DisplayName = displayName.String
		result = append(result, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// UserProfile is the human identity behind a usage_events username.
type UserProfile struct {
	Username  string `json:"username"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
}

// DisplayName returns "First Last" (single part when only one is set), or
// "" when the user has no names on file.
func (p UserProfile) DisplayName() string {
	name := strings.TrimSpace(p.FirstName + " " + p.LastName)
	if name == "" {
		return ""
	}
	return name
}

// GetUserProfile looks up one profile by username. sql.ErrNoRows becomes a
// zero-valued profile with Username set, so callers can fall back to the
// username without an extra branch.
func (s *Store) GetUserProfile(ctx context.Context, username string) (UserProfile, error) {
	var p UserProfile
	err := s.db.QueryRowContext(ctx,
		`SELECT username, first_name, last_name FROM user_profiles WHERE username = $1`, username,
	).Scan(&p.Username, &p.FirstName, &p.LastName)
	if err == sql.ErrNoRows {
		p.Username = username
		return p, nil
	}
	return p, err
}

// UpsertUserProfiles inserts or overwrites the given profiles and returns
// how many were written.
func (s *Store) UpsertUserProfiles(ctx context.Context, profiles []UserProfile) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // committed or already rolled back

	for _, p := range profiles {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO user_profiles (username, first_name, last_name, updated_at)
			VALUES ($1, $2, $3, NOW())
			ON CONFLICT (username) DO UPDATE SET
				first_name = EXCLUDED.first_name,
				last_name = EXCLUDED.last_name,
				updated_at = NOW()`,
			p.Username, p.FirstName, p.LastName); err != nil {
			return 0, fmt.Errorf("upsert profile %s: %w", p.Username, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return len(profiles), nil
}

// ListUserProfiles returns every profile, username-ordered.
func (s *Store) ListUserProfiles(ctx context.Context) ([]UserProfile, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT username, first_name, last_name FROM user_profiles ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []UserProfile
	for rows.Next() {
		var p UserProfile
		if err := rows.Scan(&p.Username, &p.FirstName, &p.LastName); err != nil {
			return nil, err
		}
		result = append(result, p)
	}
	return result, rows.Err()
}

func (s *Store) migrate(ctx context.Context) error {
	slog.Info("running database migrations")
	for _, stmt := range migrations {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("migration failed: %w", err)
		}
	}
	if err := s.migrateOrg(ctx); err != nil {
		return err
	}
	// Last: the quota tables FK to people.
	if err := s.migrateQuota(ctx); err != nil {
		return err
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
	// Human display names for dashboard users. Keyed by the same username
	// string usage_events carries (the MaaS login identity). Empty names
	// mean "unknown" — the UI falls back to the username.
	`CREATE TABLE IF NOT EXISTS user_profiles (
		username TEXT PRIMARY KEY,
		first_name TEXT NOT NULL DEFAULT '',
		last_name TEXT NOT NULL DEFAULT '',
		updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
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
// GetPricingCatalog returns the rate card for the dashboard pricing modal.
// usedOnly=true limits rows to models that appear in usage_events plus the
// self-hosted rows (hosted models always show, even before their first
// event, so the table can explain the "hosted" badge from day one). Hosted
// rows sort first; list_* rates ride along where seeded so the modal can
// show the vendor-list baseline next to our rate.
func (s *Store) GetPricingCatalog(ctx context.Context, usedOnly bool) ([]ModelPrice, error) {
	q := `SELECT model, provider, input_cost_per_mtok, output_cost_per_mtok,
	             cache_write_cost_per_mtok, cache_read_cost_per_mtok,
	             list_input_cost_per_mtok, list_output_cost_per_mtok,
	             list_cache_write_cost_per_mtok, list_cache_read_cost_per_mtok
	      FROM model_pricing`
	if usedOnly {
		q += ` WHERE model IN (SELECT DISTINCT model FROM usage_events) OR provider IN ('vllm','qwen')`
	}
	q += ` ORDER BY CASE WHEN provider IN ('vllm','qwen') THEN 0 ELSE 1 END, model`

	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var prices []ModelPrice
	for rows.Next() {
		var p ModelPrice
		if err := rows.Scan(&p.Model, &p.Provider, &p.InputCost, &p.OutputCost,
			&p.CacheWriteCost, &p.CacheReadCost,
			&p.ListInputCost, &p.ListOutputCost, &p.ListCacheWriteCost, &p.ListCacheReadCost); err != nil {
			return nil, err
		}
		prices = append(prices, p)
	}
	return prices, rows.Err()
}

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
