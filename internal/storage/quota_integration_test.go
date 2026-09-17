package storage

import (
	"context"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"
)

// Quota semantics against a real Postgres (skipped without DATABASE_URL,
// same convention as org_integration_test.go — openTestStore hands each
// test a throwaway database that has run the full migration chain):
//
//	DATABASE_URL=postgres://... go test ./internal/storage/ -run Quota
//
// openTestStore builds the Store with tokenQuota=0, which DISABLES the
// legacy token gate — so everything these tests see of HasAccess is the
// dollar decision.

func approxUSD(t *testing.T, label string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 0.005 {
		t.Fatalf("%s: got %v, want %v", label, got, want)
	}
}

var quotaEventSeq int

// seedQuotaRoster: boss -> mgr -> alice (login "alice", group "eng"), plus
// bob (login "bob", no group) and carol (no login, group "eng"). Direct SQL
// where the import API has no field for it (group_name).
func seedQuotaRoster(t *testing.T, s *Store, ctx context.Context) {
	t.Helper()
	aliceID := "alice"
	bobID := "bob"
	people := []ImportPerson{
		{Slug: "boss", FullName: "Boss Person", FirstName: "Boss", LastName: "Person"},
		{Slug: "mgr", FullName: "Manager Person", FirstName: "Manager", LastName: "Person", ManagerSlug: sp("boss")},
		{Slug: "alice", FullName: "Alice Andrew", FirstName: "Alice", LastName: "Andrew", ManagerSlug: sp("mgr"), Identity: &aliceID},
		{Slug: "bob", FullName: "Bob Bobby", FirstName: "Bob", LastName: "Bobby", Identity: &bobID},
		{Slug: "carol", FullName: "Carol Carolson", FirstName: "Carol", LastName: "Carolson", ManagerSlug: sp("mgr")},
	}
	if _, err := s.ImportPeople(ctx, people, "quota-fixture", "tester", false); err != nil {
		t.Fatalf("quota roster import: %v", err)
	}
	quotaExec(t, s, ctx, `UPDATE people SET group_name = 'eng' WHERE slug IN ('alice','carol')`)
}

func quotaExec(t *testing.T, s *Store, ctx context.Context, q string, args ...any) {
	t.Helper()
	if _, err := s.db.ExecContext(ctx, q, args...); err != nil {
		t.Fatalf("seed exec: %v", err)
	}
}

// addSpendMtok seeds one event with exact Mtok amounts so the cost math is
// readable at the unpriced-model fallback rates (1 Mtok prompt -> $15,
// 1 Mtok completion -> $75).
func addSpendMtok(t *testing.T, s *Store, ctx context.Context, username, model string, promptMtok, completionMtok int) {
	t.Helper()
	quotaEventSeq++
	quotaExec(t, s, ctx, `INSERT INTO usage_events (event_id, username, model, prompt_tokens, completion_tokens, total_tokens)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		fmt.Sprintf("q%d-%d", time.Now().UnixNano(), quotaEventSeq),
		username, model, promptMtok*1000000, completionMtok*1000000, (promptMtok+completionMtok)*1000000)
}

// TestQuotaDecision_LimitPrecedenceAndSpend walks the resolution order
// (user override -> group override -> policy default), the month grant
// add-on and its month-key expiry, fail-secure bare-username fallback, the
// unpriced-model fallback-rate spend math, and the GetMonthlyUsage wiring
// that turns the decision into the gateway's hasAccess.
func TestQuotaDecision_LimitPrecedenceAndSpend(t *testing.T) {
	s, ctx := openTestStore(t)
	seedQuotaRoster(t, s, ctx)
	// alice: 1 Mtok prompt on an unpriced model -> $15; her second login
	// adds 2 Mtok -> total $45 across identities.
	addSpendMtok(t, s, ctx, "alice", "unpriced-x", 1, 0)
	quotaExec(t, s, ctx, `INSERT INTO person_identities (username, person_slug) VALUES ('alice_alt','alice')`)
	addSpendMtok(t, s, ctx, "alice_alt", "unpriced-x", 2, 0)
	// carol: 1 Mtok prompt + 1 Mtok completion -> 15 + 75 = $90.
	addSpendMtok(t, s, ctx, "carol", "unpriced-x", 1, 1)

	// bob: no overrides -> policy default 300 (seed value), unenforced.
	d, err := s.quotaDecision(ctx, "bob", false)
	if err != nil {
		t.Fatal(err)
	}
	if !d.HasPerson || d.Enforced {
		t.Fatalf("fresh store: HasPerson=%v Enforced=%v, want true/false", d.HasPerson, d.Enforced)
	}
	approxUSD(t, "bob base", d.BaseUSD, 300)
	if !d.Allowed() {
		t.Fatal("unenforced policy must always allow")
	}

	// carol: group override only (no login of her own — decision still works
	// on her person row through any username; use her slug's absence to
	// confirm the fail-secure path separately below, so give her a login).
	quotaExec(t, s, ctx, `INSERT INTO person_identities (username, person_slug) VALUES ('carol','carol')`)
	if err := s.UpsertQuotaOverride(ctx, "tester", "group", "eng", 150); err != nil {
		t.Fatal(err)
	}
	d, err = s.quotaDecision(ctx, "carol", false)
	if err != nil {
		t.Fatal(err)
	}
	approxUSD(t, "carol group base", d.BaseUSD, 150)
	approxUSD(t, "carol spend (15+75 fallback)", d.SpentUSD, 90)
	if !d.Allowed() {
		t.Fatal("carol $90 < $150 must allow")
	}

	// alice: user override beats the group override that also applies to her.
	if err := s.UpsertQuotaOverride(ctx, "tester", "user", "alice", 75); err != nil {
		t.Fatal(err)
	}
	d, err = s.quotaDecision(ctx, "alice", false)
	if err != nil {
		t.Fatal(err)
	}
	approxUSD(t, "alice user-over-group base", d.BaseUSD, 75)
	approxUSD(t, "alice multi-login spend (1+2 Mtok)", d.SpentUSD, 45)
	if !d.Allowed() {
		t.Fatal("alice $45 < $75 must allow")
	}

	// Enforce + tighten alice's own override below her spend. (The policy
	// default deliberately stays at 300 — bob and ghost must still see it.)
	enforced := true
	if _, err := s.UpdateQuotaPolicy(ctx, "tester", nil, &enforced); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertQuotaOverride(ctx, "tester", "user", "alice", 30); err != nil {
		t.Fatal(err)
	}
	d, err = s.quotaDecision(ctx, "alice", false)
	if err != nil {
		t.Fatal(err)
	}
	if d.Allowed() {
		t.Fatalf("enforced alice $45 >= $30 must deny (decision %+v)", d)
	}
	// Exempt (super-admin) sails through an over-limit decision.
	dEx, err := s.quotaDecision(ctx, "alice", true)
	if err != nil {
		t.Fatal(err)
	}
	if !dEx.Allowed() {
		t.Fatal("exempt must always allow, even over limit")
	}

	// The hot-path wiring, while alice is genuinely over-limit: the dollar
	// gate reaches hasAccess (the token gate is disabled in this store),
	// plus the additive response fields.
	st, err := s.GetMonthlyUsage(ctx, "alice", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if st.HasAccess {
		t.Fatal("GetMonthlyUsage must deny over-limit alice")
	}
	approxUSD(t, "stats spendUsd", st.SpendUSD, 45)
	approxUSD(t, "stats quotaUsd", st.QuotaUSD, 30)
	if st.MonthEnds == "" {
		t.Fatal("monthEnds missing from entitlement stats")
	}
	stEx, err := s.GetMonthlyUsage(ctx, "alice", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if !stEx.HasAccess {
		t.Fatal("GetMonthlyUsage must admit exempt alice")
	}

	// A grant for THIS month lifts the deny; a stale-month grant must not.
	quotaExec(t, s, ctx, `INSERT INTO quota_grants (person_slug, month, extra_usd, granted_by)
		VALUES ('alice', to_char(date_trunc('month', NOW()) - interval '1 month', 'YYYY-MM'), 1000, 'tester')`)
	d, err = s.quotaDecision(ctx, "alice", false)
	if err != nil {
		t.Fatal(err)
	}
	if d.Allowed() {
		t.Fatal("prior-month grant must NOT extend this month's limit")
	}
	quotaExec(t, s, ctx, `INSERT INTO quota_grants (person_slug, month, extra_usd, granted_by)
		VALUES ('alice', to_char(date_trunc('month', NOW()), 'YYYY-MM'), 25, 'tester')`)
	d, err = s.quotaDecision(ctx, "alice", false)
	if err != nil {
		t.Fatal(err)
	}
	approxUSD(t, "alice limit after current-month grant", d.LimitUSD, 55)
	approxUSD(t, "alice grant_usd counts only the current month", d.GrantUSD, 25)
	if !d.Allowed() {
		t.Fatal("alice $45 < $55 after grant must allow")
	}

	// Fail-secure: a username with no directory row gets bare-username
	// spend at the policy default and can never match an override.
	addSpendMtok(t, s, ctx, "ghost", "unpriced-x", 1, 0)
	d, err = s.quotaDecision(ctx, "ghost", false)
	if err != nil {
		t.Fatal(err)
	}
	if d.HasPerson {
		t.Fatal("ghost must have no person row")
	}
	approxUSD(t, "ghost base = policy default", d.BaseUSD, 300)
	approxUSD(t, "ghost spend", d.SpentUSD, 15)
}

// TestQuotaRequestFlow covers the one-hop approval lifecycle: routing to
// the direct manager, the one-pending guard, cancel ownership, approve-down
// minting a current-month grant, double-decide refusing, reject+comment,
// and the stale-pending-then-cancel path.
func TestQuotaRequestFlow(t *testing.T) {
	s, ctx := openTestStore(t)
	seedQuotaRoster(t, s, ctx)

	// Unknown username cannot request anything.
	if _, err := s.CreateQuotaRequest(ctx, "nobody", 100, "why"); !errors.Is(err, ErrQuotaNotInDirectory) {
		t.Fatalf("unknown user: got %v, want ErrQuotaNotInDirectory", err)
	}

	// alice files; routed to her manager, not her boss.
	q, err := s.CreateQuotaRequest(ctx, "alice", 200, "big refactor week")
	if err != nil {
		t.Fatal(err)
	}
	if q.Status != "pending" || q.ApproverSlug != "mgr" {
		t.Fatalf("routing: status=%q approver=%q, want pending/mgr", q.Status, q.ApproverSlug)
	}
	// Only mgr's inbox sees it, and only as pending.
	inbox, err := s.ListQuotaRequests(ctx, "mgr", false, "pending")
	if err != nil || len(inbox) != 1 || inbox[0].ID != q.ID {
		t.Fatalf("mgr inbox: %d rows err=%v", len(inbox), err)
	}
	bossInbox, err := s.ListQuotaRequests(ctx, "boss", false, "pending")
	if err != nil || len(bossInbox) != 0 {
		t.Fatalf("boss must not see it: %d rows err=%v", len(bossInbox), err)
	}
	// One pending per person.
	if _, err := s.CreateQuotaRequest(ctx, "alice", 50, "again"); !errors.Is(err, ErrQuotaAlreadyPending) {
		t.Fatalf("second pending: got %v, want ErrQuotaAlreadyPending", err)
	}

	// Cancel belongs to the requester: bob (non-admin) cannot cancel alice's.
	if err := s.CancelQuotaRequest(ctx, q.ID, "bob", "bob", false); !errors.Is(err, ErrQuotaNoPending) {
		t.Fatalf("cross-person cancel: got %v, want ErrQuotaNoPending", err)
	}
	if err := s.CancelQuotaRequest(ctx, q.ID, "alice", "alice", false); err != nil {
		t.Fatalf("own cancel: %v", err)
	}
	q, err = s.CreateQuotaRequest(ctx, "alice", 200, "refiled")
	if err != nil {
		t.Fatalf("refile after cancel: %v", err)
	}

	// Approve-down: manager answers $120 to a $200 ask; the grant lands in
	// NOW()'s month, and the effective limit moves by the APPROVED amount.
	before, err := s.quotaDecision(ctx, "alice", false)
	if err != nil {
		t.Fatal(err)
	}
	decided, err := s.ApproveQuotaRequest(ctx, q.ID, "mgr", 120)
	if err != nil {
		t.Fatal(err)
	}
	if decided.Status != "approved" {
		t.Fatalf("approve: status %q", decided.Status)
	}
	after, err := s.quotaDecision(ctx, "alice", false)
	if err != nil {
		t.Fatal(err)
	}
	approxUSD(t, "limit grows by the approved amount", after.LimitUSD, before.LimitUSD+120)
	approxUSD(t, "grant_usd", after.GrantUSD, 120)

	// Double-decide: the row left pending, so any second decision is a clean
	// no-pending signal (the handler renders it as 409).
	if _, err := s.ApproveQuotaRequest(ctx, q.ID, "mgr", 500); !errors.Is(err, ErrQuotaNoPending) {
		t.Fatalf("double approve: got %v, want ErrQuotaNoPending", err)
	}
	if _, err := s.RejectQuotaRequest(ctx, q.ID, "mgr", "too late"); !errors.Is(err, ErrQuotaNoPending) {
		t.Fatalf("approve-then-reject: got %v, want ErrQuotaNoPending", err)
	}

	// A decided person may re-file; next round gets rejected with a comment.
	q2, err := s.CreateQuotaRequest(ctx, "alice", 1000, "huge month")
	if err != nil {
		t.Fatalf("re-file after approval: %v", err)
	}
	if _, err := s.RejectQuotaRequest(ctx, q2.ID, "mgr", "   "); err == nil {
		t.Fatal("reject without comment must fail")
	}
	rej, err := s.RejectQuotaRequest(ctx, q2.ID, "mgr", "cut your usage first")
	if err != nil {
		t.Fatal(err)
	}
	if rej.Status != "rejected" || rej.Comment != "cut your usage first" {
		t.Fatalf("reject: %+v", rej)
	}
	// The view surfaces the rejection for the banner…
	v, err := s.GetQuotaView(ctx, "alice", false)
	if err != nil {
		t.Fatal(err)
	}
	if v.Request == nil || v.Request.Status != "rejected" || !v.Request.IsCurrentMonth {
		t.Fatalf("view after reject: %+v", v.Request)
	}
	// …and a fresh pending replaces it in the view.
	if _, err := s.CreateQuotaRequest(ctx, "alice", 300, "third time"); err != nil {
		t.Fatal(err)
	}
	v, err = s.GetQuotaView(ctx, "alice", false)
	if err != nil {
		t.Fatal(err)
	}
	if v.Request == nil || v.Request.Status != "pending" {
		t.Fatalf("view after re-file: %+v", v.Request)
	}

	// Stale pending from a prior month holds the slot, shows in the view as
	// stale, and CancelPendingForPerson frees it (the DELETE /me path).
	quotaExec(t, s, ctx, `DELETE FROM quota_requests WHERE status = 'pending'`)
	quotaExec(t, s, ctx, `INSERT INTO quota_requests (person_slug, month, asked_usd, reason, status, approver_slug)
		VALUES ('bob', to_char(date_trunc('month', NOW()) - interval '1 month', 'YYYY-MM'), 90, 'last month', 'pending', 'mgr')`)
	if _, err := s.CreateQuotaRequest(ctx, "bob", 40, "this month"); !errors.Is(err, ErrQuotaAlreadyPending) {
		t.Fatalf("stale pending must hold the slot: got %v", err)
	}
	vb, err := s.GetQuotaView(ctx, "bob", false)
	if err != nil {
		t.Fatal(err)
	}
	if vb.Request == nil || !vb.Request.PendingFromPriorMonth || vb.Request.IsCurrentMonth {
		t.Fatalf("stale flag in view: %+v", vb.Request)
	}
	if err := s.CancelPendingForPerson(ctx, "bob", "bob", false); err != nil {
		t.Fatalf("cancel-by-person (stale row): %v", err)
	}
	if err := s.CancelPendingForPerson(ctx, "bob", "bob", false); !errors.Is(err, ErrQuotaNoPending) {
		t.Fatalf("second cancel: got %v, want ErrQuotaNoPending", err)
	}
	if _, err := s.CreateQuotaRequest(ctx, "bob", 40, "this month"); err != nil {
		t.Fatalf("file after stale cancel: %v", err)
	}
}

// TestQuotaAuditTrail keeps the audit story honest: every mutation writes
// its quota.* row with the real actor (org_audit, shared with the org
// feature).
func TestQuotaAuditTrail(t *testing.T) {
	s, ctx := openTestStore(t)
	seedQuotaRoster(t, s, ctx)
	if err := s.UpsertQuotaOverride(ctx, "tester", "user", "alice", 500); err != nil {
		t.Fatal(err)
	}
	q, err := s.CreateQuotaRequest(ctx, "alice", 100, "audit me")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApproveQuotaRequest(ctx, q.ID, "mgr", 80); err != nil {
		t.Fatal(err)
	}
	var n int
	for _, tc := range []struct{ action, actor string }{
		{"quota.override_set", "tester"},
		{"quota.request", "alice"},
		{"quota.approve", "mgr"},
	} {
		if err := s.db.QueryRowContext(ctx,
			`SELECT count(*) FROM org_audit WHERE action = $1 AND actor = $2`, tc.action, tc.actor).Scan(&n); err != nil || n < 1 {
			t.Fatalf("audit %s by %s: n=%d err=%v", tc.action, tc.actor, n, err)
		}
	}
}
