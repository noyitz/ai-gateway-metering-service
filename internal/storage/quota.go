package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Lifecycle sentinels the handler maps to HTTP statuses. Validation stays
// in the handler; anything surfacing from this package past these is a
// server-side failure and gets a 500, not an echoed message.
var (
	// ErrQuotaAlreadyPending: the person has a live pending request (or a
	// stale pending from an earlier month, which still holds the slot).
	ErrQuotaAlreadyPending = errors.New("a pending request already exists")
	// ErrQuotaNoPending: the row exists but is no longer pending — decided,
	// cancelled, or gone. Decisions are atomic in WHERE status='pending',
	// so the loser of a double-click sees this, not a lost write.
	ErrQuotaNoPending = errors.New("request is no longer pending")
	// ErrQuotaNotInDirectory: the username has no people row, so there is
	// no slug to attach a request to and no manager to route it to.
	ErrQuotaNotInDirectory = errors.New("username not found in the directory")
)

// Quota management: monthly dollar budgets per user, resolved through the
// org directory, enforced at the gateway via the entitlement endpoint's
// hasAccess flag.
//
// The design guarantees that hold across this file:
//   - The calendar month is computed in SQL (date_trunc('month', NOW())),
//     never in Go, so the spend window and the grant month key can never
//     disagree across the app/server timezone boundary.
//   - Nothing here writes usage_events. Spend is derived at query time with
//     the shared costUSDExpr — the same number the dashboard shows — over
//     every login linked to the person.
//   - A request decision is atomic in `WHERE status = 'pending'` (the
//     ClaimInvite idiom): two approvers deciding at once yield one success
//     and one no-op, never a double grant.
//   - Approvals are ONE hop: a request routes to the requester's direct
//     manager at creation and never moves again. A manager who needs someone
//     else's sign-off settles it offline and still records approve/reject
//     here. People without a manager land on the super-admin backstop
//     (approver_slug NULL, visible to super-admins).

// quotaMigrations run after the org migrations (the quota tables FK to
// people). Appended-slice convention from org.go: every statement is
// idempotent.
var quotaMigrations = []string{
	// Single-row table: the boolean PK that must be true is the classic
	// Postgres singleton trick — there is exactly one row and no way to add
	// a second. Seeded dark (enforced=false) so the ship is inert until an
	// operator flips the flag deliberately.
	`CREATE TABLE IF NOT EXISTS quota_policy (
		id BOOLEAN PRIMARY KEY DEFAULT true CHECK (id),
		default_monthly_usd NUMERIC(12,2) NOT NULL DEFAULT 300,
		enforced BOOLEAN NOT NULL DEFAULT false,
		updated_by TEXT NOT NULL DEFAULT '',
		updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`,
	`INSERT INTO quota_policy (id) VALUES (true) ON CONFLICT DO NOTHING`,
	// Per-scope limits. User scope is keyed by person slug (stable identity,
	// covers all their logins); group scope by people.group_name. A NULL
	// lookup result is "no override" — resolution order user → group →
	// policy default happens in one query.
	`CREATE TABLE IF NOT EXISTS quota_overrides (
		scope TEXT NOT NULL CHECK (scope IN ('user','group')),
		principal TEXT NOT NULL,
		monthly_usd NUMERIC(12,2) NOT NULL,
		updated_by TEXT NOT NULL DEFAULT '',
		updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		PRIMARY KEY (scope, principal)
	)`,
	// Approval requests, one hop: approver_slug is stamped at creation from
	// the requester's manager (NULL = super-admin backstop) and never
	// re-pointed. decided_by/comment fill in at decision time.
	`CREATE TABLE IF NOT EXISTS quota_requests (
		id BIGSERIAL PRIMARY KEY,
		person_slug TEXT NOT NULL REFERENCES people(slug) ON DELETE CASCADE,
		month TEXT NOT NULL,
		asked_usd NUMERIC(12,2) NOT NULL,
		reason TEXT NOT NULL DEFAULT '',
		status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','approved','rejected','cancelled')),
		approver_slug TEXT REFERENCES people(slug) ON DELETE SET NULL,
		decided_by TEXT NOT NULL DEFAULT '',
		comment TEXT NOT NULL DEFAULT '',
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		decided_at TIMESTAMPTZ
	)`,
	// One pending request per person, in the database: every create path
	// (and any future one) inherits the rule.
	`CREATE UNIQUE INDEX IF NOT EXISTS uq_quota_requests_pending ON quota_requests (person_slug) WHERE status = 'pending'`,
	`CREATE INDEX IF NOT EXISTS idx_quota_requests_approver ON quota_requests (approver_slug, status)`,
	`CREATE INDEX IF NOT EXISTS idx_quota_requests_person ON quota_requests (person_slug, month)`,
	// Approved extra budget for ONE calendar month, keyed by 'YYYY-MM'.
	// Expiry is automatic — the effective-limit query only sums grants whose
	// month key is the current month — no cron, no cleanup.
	`CREATE TABLE IF NOT EXISTS quota_grants (
		id BIGSERIAL PRIMARY KEY,
		person_slug TEXT NOT NULL REFERENCES people(slug) ON DELETE CASCADE,
		month TEXT NOT NULL,
		extra_usd NUMERIC(12,2) NOT NULL,
		request_id BIGINT REFERENCES quota_requests(id) ON DELETE SET NULL,
		granted_by TEXT NOT NULL DEFAULT '',
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`,
	`CREATE INDEX IF NOT EXISTS idx_quota_grants_person_month ON quota_grants (person_slug, month)`,
	// The entitlement endpoint now runs a month-scoped SUM per username on
	// every gateway request inside its 5s subrequest budget; the best
	// existing index was username-only with a timestamp recheck.
	`CREATE INDEX IF NOT EXISTS idx_usage_events_user_ts ON usage_events (username, timestamp)`,
}

func (s *Store) migrateQuota(ctx context.Context) error {
	for _, stmt := range quotaMigrations {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("quota migration failed: %w", err)
		}
	}
	return nil
}

// --- Policy & overrides (admin surface) ---

type QuotaPolicy struct {
	DefaultMonthlyUSD float64   `json:"default_monthly_usd"`
	Enforced          bool      `json:"enforced"`
	UpdatedBy         string    `json:"updated_by"`
	UpdatedAt         time.Time `json:"updated_at"`
}

type QuotaOverride struct {
	Scope      string    `json:"scope"` // "user" (person slug) | "group"
	Principal  string    `json:"principal"`
	MonthlyUSD float64   `json:"monthly_usd"`
	UpdatedBy  string    `json:"updated_by"`
	UpdatedAt  time.Time `json:"updated_at"`
}

func (s *Store) GetQuotaPolicy(ctx context.Context) (QuotaPolicy, error) {
	var p QuotaPolicy
	err := s.db.QueryRowContext(ctx,
		`SELECT default_monthly_usd, enforced, updated_by, updated_at FROM quota_policy WHERE id = true`,
	).Scan(&p.DefaultMonthlyUSD, &p.Enforced, &p.UpdatedBy, &p.UpdatedAt)
	return p, err
}

func (s *Store) UpdateQuotaPolicy(ctx context.Context, actor string, defaultUSD *float64, enforced *bool) (QuotaPolicy, error) {
	if defaultUSD != nil && *defaultUSD <= 0 {
		return QuotaPolicy{}, fmt.Errorf("default_monthly_usd must be > 0")
	}
	if defaultUSD != nil {
		if _, err := s.db.ExecContext(ctx,
			`UPDATE quota_policy SET default_monthly_usd = $1, updated_by = $2, updated_at = NOW() WHERE id = true`,
			*defaultUSD, actor); err != nil {
			return QuotaPolicy{}, err
		}
	}
	if enforced != nil {
		if _, err := s.db.ExecContext(ctx,
			`UPDATE quota_policy SET enforced = $1, updated_by = $2, updated_at = NOW() WHERE id = true`,
			*enforced, actor); err != nil {
			return QuotaPolicy{}, err
		}
	}
	if err := s.Audit(ctx, actor, "quota.policy_update", "policy",
		map[string]any{"default_monthly_usd": defaultUSD, "enforced": enforced}); err != nil {
		return QuotaPolicy{}, err
	}
	s.invalidateQuotaCache()
	return s.GetQuotaPolicy(ctx)
}

func (s *Store) ListQuotaOverrides(ctx context.Context) ([]QuotaOverride, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT scope, principal, monthly_usd, updated_by, updated_at FROM quota_overrides ORDER BY scope, principal`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QuotaOverride
	for rows.Next() {
		var o QuotaOverride
		if err := rows.Scan(&o.Scope, &o.Principal, &o.MonthlyUSD, &o.UpdatedBy, &o.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func (s *Store) UpsertQuotaOverride(ctx context.Context, actor, scope, principal string, monthlyUSD float64) error {
	if scope != "user" && scope != "group" {
		return fmt.Errorf("scope must be 'user' or 'group'")
	}
	principal = strings.TrimSpace(principal)
	if principal == "" {
		return fmt.Errorf("principal required")
	}
	if monthlyUSD <= 0 {
		return fmt.Errorf("monthly_usd must be > 0 (delete the override to fall back to the default)")
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO quota_overrides (scope, principal, monthly_usd, updated_by)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (scope, principal) DO UPDATE SET
			monthly_usd = EXCLUDED.monthly_usd,
			updated_by = EXCLUDED.updated_by,
			updated_at = NOW()`,
		scope, principal, monthlyUSD, actor); err != nil {
		return err
	}
	if err := s.Audit(ctx, actor, "quota.override_set", scope+":"+principal, map[string]any{"monthly_usd": monthlyUSD}); err != nil {
		return err
	}
	s.invalidateQuotaCache()
	return nil
}

func (s *Store) DeleteQuotaOverride(ctx context.Context, actor, scope, principal string) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM quota_overrides WHERE scope = $1 AND principal = $2`, scope, principal); err != nil {
		return err
	}
	if err := s.Audit(ctx, actor, "quota.override_delete", scope+":"+principal, nil); err != nil {
		return err
	}
	s.invalidateQuotaCache()
	return nil
}

// --- Decision core (the enforcement hot path) ---

// QuotaDecision is one person's month evaluated against the quota policy.
// SpentUSD is the same number the dashboard shows: all spend (hosted
// included), priced by the shared costUSDExpr, summed across every login
// linked to the person.
type QuotaDecision struct {
	HasPerson bool      `json:"has_person"`
	Enforced  bool      `json:"enforced"` // the policy flag
	Exempt    bool      `json:"exempt"`   // super-admin — never gated
	BaseUSD   float64   `json:"base_usd"` // user/group override or policy default
	GrantUSD  float64   `json:"grant_usd"`
	LimitUSD  float64   `json:"limit_usd"` // base + this month's grants
	SpentUSD  float64   `json:"spent_usd"`
	Month     string    `json:"month"`
	MonthEnds time.Time `json:"month_ends"`
}

// EffectiveEnforced reports whether gating actually applies to this caller.
func (d QuotaDecision) EffectiveEnforced() bool { return d.Enforced && !d.Exempt }

// Allowed is the hasAccess answer: dollars under limit (or gating off /
// exempt). An error is NOT Allowed's problem — callers fail-open on error
// like they already do for the whole endpoint.
func (d QuotaDecision) Allowed() bool {
	return !d.EffectiveEnforced() || d.SpentUSD < d.LimitUSD
}

// quotaDecision computes the full decision in two queries: one for policy +
// overrides + grants + month bounds, one for month-to-date spend across the
// person's logins. A username absent from the directory is fail-secure:
// bare-username spend at the policy default (mirrors selfScope). The no-person
// case binds a SQL NULL as the slug — comparisons against NULL never match, so
// overrides and grants can never apply to an unknown username. (A sentinel
// string cannot work here: Postgres TEXT rejects NUL bytes, and any printable
// sentinel a slug could one day collide with is worse than NULL.)
func (s *Store) quotaDecision(ctx context.Context, username string, exempt bool) (QuotaDecision, error) {
	d := QuotaDecision{Exempt: exempt}

	var slugArg any
	groupName := ""
	logins := []string{username}
	person, err := s.GetPersonByUsername(ctx, username)
	if err == nil {
		d.HasPerson = true
		slugArg, groupName = person.Slug, person.GroupName
		if own, err := s.PersonUsernames(ctx, person.Slug); err == nil && len(own) > 0 {
			logins = own
		}
	} else if err != sql.ErrNoRows {
		return d, err
	}

	var userOv, groupOv, grantUSD, defaultUSD float64
	err = s.db.QueryRowContext(ctx, `
		SELECT pol.default_monthly_usd, pol.enforced,
			COALESCE(ou.monthly_usd, -1), COALESCE(og.monthly_usd, -1),
			g.grant,
			to_char(date_trunc('month', NOW()), 'YYYY-MM'),
			date_trunc('month', NOW()) + interval '1 month'
		FROM (SELECT default_monthly_usd, enforced FROM quota_policy WHERE id = true) pol
		LEFT JOIN quota_overrides ou ON ou.scope = 'user' AND ou.principal = $1
		LEFT JOIN quota_overrides og ON og.scope = 'group' AND $2 <> '' AND og.principal = $2
		LEFT JOIN LATERAL (
			SELECT COALESCE(SUM(extra_usd), 0) as grant
			FROM quota_grants WHERE person_slug = $1 AND month = to_char(date_trunc('month', NOW()), 'YYYY-MM')
		) g ON true`,
		slugArg, groupName,
	).Scan(&defaultUSD, &d.Enforced, &userOv, &groupOv, &grantUSD, &d.Month, &d.MonthEnds)
	if err != nil {
		return d, fmt.Errorf("quota policy lookup: %w", err)
	}

	// Resolution order: user override → group override → policy default.
	// -1 marks "no override row" (NUMERIC is never negative for a real row).
	switch {
	case userOv >= 0:
		d.BaseUSD = userOv
	case groupOv >= 0:
		d.BaseUSD = groupOv
	default:
		d.BaseUSD = defaultUSD
	}
	d.GrantUSD = grantUSD
	d.LimitUSD = d.BaseUSD + d.GrantUSD

	err = s.db.QueryRowContext(ctx, fmt.Sprintf(`
		SELECT COALESCE(SUM(%s), 0)
		FROM usage_events e
		LEFT JOIN model_pricing p ON e.model = p.model
		WHERE e.username = ANY($1) AND e.timestamp >= date_trunc('month', NOW())`, costUSDExpr),
		logins,
	).Scan(&d.SpentUSD)
	if err != nil {
		return d, fmt.Errorf("quota spend lookup: %w", err)
	}
	return d, nil
}

// --- Decision cache (the gateway's synchronous dependency) ---

const quotaCacheTTL = 15 * time.Second

type quotaCacheEntry struct {
	decision QuotaDecision
	expires  time.Time
}

// QuotaDecisionCached is the entitlement hot path: every gateway request
// asks this before forwarding. 15s of staleness costs at most a few requests
// of overshoot on a $300 budget — the alternative is a per-request directory
// walk + usage scan on the critical latency path of every inference call.
// Any quota mutation invalidates the whole cache (it holds at most a few
// hundred entries; a map clear is cheaper than per-person bookkeeping).
func (s *Store) QuotaDecisionCached(ctx context.Context, username string, exempt bool) (QuotaDecision, error) {
	key := username
	if exempt {
		key += "|x"
	}
	s.quotaMu.Lock()
	e, ok := s.quotaCache[key]
	s.quotaMu.Unlock()
	if ok && time.Now().Before(e.expires) {
		return e.decision, nil
	}
	d, err := s.quotaDecision(ctx, username, exempt)
	if err != nil {
		return d, err
	}
	s.quotaMu.Lock()
	if s.quotaCache == nil {
		s.quotaCache = map[string]quotaCacheEntry{}
	}
	s.quotaCache[key] = quotaCacheEntry{decision: d, expires: time.Now().Add(quotaCacheTTL)}
	s.quotaMu.Unlock()
	return d, nil
}

func (s *Store) invalidateQuotaCache() {
	s.quotaMu.Lock()
	s.quotaCache = nil
	s.quotaMu.Unlock()
}

// --- Request view (what every UI surface renders) ---

// QuotaRequest is one approval request with its people joined in, so a
// manager inbox renders without a second lookup.
type QuotaRequest struct {
	ID           int64      `json:"id"`
	PersonSlug   string     `json:"person_slug"`
	PersonName   string     `json:"person_name"`
	Username     string     `json:"username,omitempty"` // primary login, for "contact them"
	Month        string     `json:"month"`
	AskedUSD     float64    `json:"asked_usd"`
	Reason       string     `json:"reason"`
	Status       string     `json:"status"`
	ApproverSlug string     `json:"approver_slug,omitempty"` // "" = super-admin backstop
	ApproverName string     `json:"approver_name,omitempty"`
	DecidedBy    string     `json:"decided_by,omitempty"`
	Comment      string     `json:"comment,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	DecidedAt    *time.Time `json:"decided_at,omitempty"`
	// PendingFromPriorMonth marks a pending row whose month key is no longer
	// the current month: shown for history, not actionable, no longer
	// blocking new requests (it is still 'pending' but its month has passed).
	PendingFromPriorMonth bool `json:"pending_from_prior_month"`
	// IsCurrentMonth duplicates the same fact positively, for the UI.
	IsCurrentMonth bool `json:"is_current_month"`
}

const quotaRequestSelect = `
	SELECT q.id, q.person_slug, p.full_name, COALESCE(pi.username, ''), q.month,
		q.asked_usd, q.reason, q.status, COALESCE(q.approver_slug, ''), COALESCE(a.full_name, ''),
		q.decided_by, q.comment, q.created_at, q.decided_at,
		q.month = to_char(date_trunc('month', NOW()), 'YYYY-MM')
	FROM quota_requests q
	JOIN people p ON p.slug = q.person_slug
	LEFT JOIN people a ON a.slug = q.approver_slug
	LEFT JOIN LATERAL (SELECT username FROM person_identities WHERE person_slug = q.person_slug AND is_service = false ORDER BY username LIMIT 1) pi ON true
`

func scanQuotaRequest(row interface{ Scan(...any) error }) (QuotaRequest, error) {
	var q QuotaRequest
	var decided sql.NullTime
	err := row.Scan(&q.ID, &q.PersonSlug, &q.PersonName, &q.Username, &q.Month,
		&q.AskedUSD, &q.Reason, &q.Status, &q.ApproverSlug, &q.ApproverName,
		&q.DecidedBy, &q.Comment, &q.CreatedAt, &decided, &q.IsCurrentMonth)
	if err != nil {
		return q, err
	}
	if decided.Valid {
		q.DecidedAt = &decided.Time
	}
	q.PendingFromPriorMonth = q.Status == "pending" && !q.IsCurrentMonth
	return q, nil
}

func (s *Store) GetQuotaRequest(ctx context.Context, id int64) (QuotaRequest, error) {
	return scanQuotaRequest(s.db.QueryRowContext(ctx, quotaRequestSelect+`WHERE q.id = $1`, id))
}

// CreateQuotaRequest files the caller's ask with their directory manager as
// the approver (NULL = super-admin backstop for the manager-less). The
// partial unique index enforces one live pending request per person; a
// stale pending from an earlier month was already non-blocking by
// definition only if cancelled first — it isn't auto-cancelled here, the
// unique index still holds, and the handler explains it.
func (s *Store) CreateQuotaRequest(ctx context.Context, username string, askedUSD float64, reason string) (QuotaRequest, error) {
	person, err := s.GetPersonByUsername(ctx, username)
	if err == sql.ErrNoRows {
		return QuotaRequest{}, ErrQuotaNotInDirectory
	}
	if err != nil {
		return QuotaRequest{}, err
	}
	var approver any
	if person.ManagerSlug != "" {
		approver = person.ManagerSlug
	}
	var id int64
	err = s.db.QueryRowContext(ctx, `
		INSERT INTO quota_requests (person_slug, month, asked_usd, reason, approver_slug)
		VALUES ($1, to_char(date_trunc('month', NOW()), 'YYYY-MM'), $2, $3, $4)
		RETURNING id`,
		person.Slug, askedUSD, strings.TrimSpace(reason), approver,
	).Scan(&id)
	if err != nil {
		// The 23505 constraint name is the precise signal for "a live
		// pending row already exists"; any other insert failure is a 500.
		if strings.Contains(err.Error(), "uq_quota_requests_pending") {
			return QuotaRequest{}, ErrQuotaAlreadyPending
		}
		return QuotaRequest{}, err
	}
	if err := s.Audit(ctx, username, "quota.request", person.Slug, map[string]any{"id": id, "asked_usd": askedUSD}); err != nil {
		return QuotaRequest{}, err
	}
	q, err := s.GetQuotaRequest(ctx, id)
	if err == nil {
		s.invalidateQuotaCache() // pending state shows in the banner
	}
	return q, err
}

// CancelQuotaRequest withdraws a pending request. The requester may cancel
// their own; isAdmin (checked by the handler) cancels anyone's. Atomic in
// status='pending' like every other decision, so cancel-while-decided is a
// clean 409, not a surprise.
func (s *Store) CancelQuotaRequest(ctx context.Context, id int64, actor string, personSlug string, isAdmin bool) error {
	query := `UPDATE quota_requests SET status = 'cancelled', decided_by = $2, decided_at = NOW() WHERE id = $1 AND status = 'pending'`
	args := []any{id, actor}
	if !isAdmin {
		query += ` AND person_slug = $3`
		args = append(args, personSlug)
	}
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrQuotaNoPending // nothing pending at this id for this caller
	}
	if err := s.Audit(ctx, actor, "quota.cancel", fmt.Sprint(id), nil); err != nil {
		return err
	}
	s.invalidateQuotaCache()
	return nil
}

// ApproveQuotaRequest records the decision AND mints the grant for the
// month of approval (which covers the remainder of that calendar month;
// expiry is automatic through the month key). approvedUSD is the manager's
// answer — the asked amount as-is, or an edited figure, up or down.
func (s *Store) ApproveQuotaRequest(ctx context.Context, id int64, actor string, approvedUSD float64) (QuotaRequest, error) {
	if approvedUSD <= 0 {
		return QuotaRequest{}, fmt.Errorf("approved_usd must be > 0")
	}
	if approvedUSD > 100000 {
		return QuotaRequest{}, fmt.Errorf("approved_usd is unreasonably large")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return QuotaRequest{}, err
	}
	defer tx.Rollback() //nolint:errcheck // committed or rolled back
	var personSlug, month string
	err = tx.QueryRowContext(ctx, `
		UPDATE quota_requests SET status = 'approved', decided_by = $2, decided_at = NOW()
		WHERE id = $1 AND status = 'pending'
		RETURNING person_slug, month`,
		id, actor).Scan(&personSlug, &month)
	if err == sql.ErrNoRows {
		return QuotaRequest{}, ErrQuotaNoPending
	}
	if err != nil {
		return QuotaRequest{}, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO quota_grants (person_slug, month, extra_usd, request_id, granted_by)
		VALUES ($1, to_char(date_trunc('month', NOW()), 'YYYY-MM'), $2, $3, $4)`,
		personSlug, approvedUSD, id, actor); err != nil {
		return QuotaRequest{}, err
	}
	if err := tx.Commit(); err != nil {
		return QuotaRequest{}, err
	}
	if err := s.Audit(ctx, actor, "quota.approve", fmt.Sprint(id),
		map[string]any{"person": personSlug, "asked_month": month, "approved_usd": approvedUSD}); err != nil {
		return QuotaRequest{}, err
	}
	s.invalidateQuotaCache()
	return s.GetQuotaRequest(ctx, id)
}

// RejectQuotaRequest turns the request down; the comment is mandatory and
// the requester sees it in their banner.
func (s *Store) RejectQuotaRequest(ctx context.Context, id int64, actor string, comment string) (QuotaRequest, error) {
	comment = strings.TrimSpace(comment)
	if comment == "" {
		return QuotaRequest{}, fmt.Errorf("a comment is required when rejecting")
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE quota_requests SET status = 'rejected', decided_by = $2, comment = $3, decided_at = NOW()
		WHERE id = $1 AND status = 'pending'`,
		id, actor, comment)
	if err != nil {
		return QuotaRequest{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return QuotaRequest{}, ErrQuotaNoPending
	}
	if err := s.Audit(ctx, actor, "quota.reject", fmt.Sprint(id), map[string]any{"comment": comment}); err != nil {
		return QuotaRequest{}, err
	}
	s.invalidateQuotaCache()
	return s.GetQuotaRequest(ctx, id)
}

// ListQuotaRequests is the approver inbox plus the admin list. all=true
// (super-admin) sees every request; otherwise only requests whose stamped
// approver is the given slug. state: "pending", "decided", or "" (all).
func (s *Store) ListQuotaRequests(ctx context.Context, approverSlug string, all bool, state string) ([]QuotaRequest, error) {
	where := "WHERE 1=1"
	args := []any{}
	if !all {
		args = append(args, approverSlug)
		where += fmt.Sprintf(" AND q.approver_slug = $%d", len(args))
	}
	switch state {
	case "pending":
		where += " AND q.status = 'pending'"
	case "decided":
		where += " AND q.status <> 'pending'"
	}
	rows, err := s.db.QueryContext(ctx, quotaRequestSelect+where+
		" ORDER BY (q.status = 'pending') DESC, q.created_at DESC LIMIT 200", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QuotaRequest
	for rows.Next() {
		q, err := scanQuotaRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

// --- Status view (whoami / me / gauges carrier) ---

// QuotaView is the full quota picture for one login: what the dashboard
// popup and banner render, and what the manager page's gauges reuse per
// person.
type QuotaView struct {
	Username string `json:"username"`
	QuotaDecision
	// Request is the person's most recent request in the current month, or a
	// stale pending from an earlier month (PendingFromPriorMonth). nil when
	// there is nothing to show.
	Request *QuotaRequest `json:"request,omitempty"`
}

// GetQuotaView assembles the decision plus the person's latest request for
// the banner. Fresh (uncached) — this backs interactive pages, not the
// gateway hot path.
func (s *Store) GetQuotaView(ctx context.Context, username string, exempt bool) (QuotaView, error) {
	d, err := s.quotaDecision(ctx, username, exempt)
	if err != nil {
		return QuotaView{}, err
	}
	v := QuotaView{Username: username, QuotaDecision: d}
	if d.HasPerson {
		if person, err := s.GetPersonByUsername(ctx, username); err == nil {
			q, err := s.latestRequestForPerson(ctx, person.Slug)
			if err != nil && err != sql.ErrNoRows {
				return v, nil // a request lookup failure degrades the banner, not the whole view
			}
			if err == nil {
				v.Request = &q
			}
		}
	}
	return v, nil
}

// latestRequestForPerson: the newest current-month row, else the newest
// stale pending (shown, not actionable, still blocking new requests).
func (s *Store) latestRequestForPerson(ctx context.Context, slug string) (QuotaRequest, error) {
	return scanQuotaRequest(s.db.QueryRowContext(ctx, quotaRequestSelect+`
		WHERE q.person_slug = $1
		  AND (q.month = to_char(date_trunc('month', NOW()), 'YYYY-MM') OR (q.status = 'pending'))
		ORDER BY (q.status = 'pending' AND q.month <> to_char(date_trunc('month', NOW()), 'YYYY-MM')) ASC,
		         q.created_at DESC
		LIMIT 1`, slug))
}

// CancelPendingForPerson cancels a person's pending row addressed by their
// login — the DELETE /me/quota/request path, so the client never needs the
// row id. A stale pending from an earlier month still matches (status =
// 'pending' regardless of month), which is also how the person frees the
// one-pending slot to file a fresh request. admin=true is the super-admin
// variant: actor is the real session identity in both cases.
func (s *Store) CancelPendingForPerson(ctx context.Context, username string, actor string, admin bool) error {
	person, err := s.GetPersonByUsername(ctx, username)
	if err == sql.ErrNoRows {
		return ErrQuotaNotInDirectory
	}
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE quota_requests SET status = 'cancelled', decided_by = $2, decided_at = NOW()
		WHERE person_slug = $1 AND status = 'pending'`,
		person.Slug, actor)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrQuotaNoPending
	}
	if err := s.Audit(ctx, actor, "quota.cancel", person.Slug, nil); err != nil {
		return err
	}
	s.invalidateQuotaCache()
	return nil
}

// --- Manager-page gauges: per-person quota columns on the org usage rollup ---

// quotaLimitExpr resolves one person's effective monthly $ limit against
// aliases: `st` = subtree slug column, `p` = people row. Splice with
// fmt.Sprintf into GetOrgUsage.
const quotaLimitExpr = `COALESCE(ou.monthly_usd, og.monthly_usd, (SELECT default_monthly_usd FROM quota_policy WHERE id = true), 0)
	+ COALESCE(gr.grant, 0)`

const quotaJoinExpr = `
	LEFT JOIN quota_overrides ou ON ou.scope = 'user' AND ou.principal = st.slug
	LEFT JOIN quota_overrides og ON og.scope = 'group' AND p.group_name <> '' AND og.principal = p.group_name
	LEFT JOIN LATERAL (
		SELECT COALESCE(SUM(extra_usd), 0) as grant
		FROM quota_grants g WHERE g.person_slug = st.slug
		  AND g.month = to_char(date_trunc('month', NOW()), 'YYYY-MM')
	) gr ON true`
