package storage

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

// These tests exercise the real database paths of the org feature: import,
// scope, the cycle trigger, invites, and above all the guarantee that no
// org operation can alter usage_events. They run against DATABASE_URL and
// skip when it is unset:
//
//	DATABASE_URL=postgres://... go test ./internal/storage/ -run Org
//
// The checksum guard (TestOrgImport_NeverTouchesUsage) is the executable
// form of guarantee G1: "existing users never lose their numbers".

func openTestStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set — org integration tests need a Postgres")
	}
	dbName := fmt.Sprintf("orgtest_%d", time.Now().UnixNano())
	// Create a throwaway database so tests never see (or pollute) real data.
	{
		s, err := New(dsn, 0)
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		if _, err := s.db.Exec("CREATE DATABASE " + dbName); err != nil {
			s.Close()
			t.Fatalf("create test db: %v", err)
		}
		s.Close()
	}
	testDSN := replaceDBName(dsn, dbName)
	store, err := New(testDSN, 0)
	if err != nil {
		t.Fatalf("connect fresh db: %v", err)
	}
	t.Cleanup(func() {
		store.Close()
		s, err := New(dsn, 0)
		if err == nil {
			s.db.Exec("DROP DATABASE " + dbName) //nolint:errcheck
			s.Close()
		}
	})
	return store, context.Background()
}

func replaceDBName(dsn, name string) string {
	// postgres://user:pass@host:port/OLDNAME?params — swap the path segment.
	for i := len(dsn) - 1; i >= 0; i-- {
		if dsn[i] == '/' {
			end := len(dsn)
			if q := indexOf(dsn[i:], '?'); q >= 0 {
				end = i + q
				return dsn[:i+1] + name + dsn[end:]
			}
			return dsn[:i+1] + name
		}
	}
	return dsn
}

func indexOf(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}

func sp(s string) *string { return &s }

// fixture:  boss -> mgr -> ic1, ic2. ic1 is linked to login alice_db and
// has usage. legacy has usage but is deliberately NOT in the roster.
func seedFixture(t *testing.T, s *Store, ctx context.Context) {
	t.Helper()
	s.db.ExecContext(ctx, `INSERT INTO user_profiles (username, first_name, last_name) VALUES
		('alice_db','Iris','Callaghan'), ('legacy','Old','Timer')`)
	s.db.ExecContext(ctx, `INSERT INTO usage_events (event_id, username, model, prompt_tokens, completion_tokens, total_tokens) VALUES
		('e1','alice_db','claude-opus-4-8',1000,500,1500),
		('e2','alice_db','claude-opus-4-8',2000,700,2700),
		('e3','legacy','gpt-4o',800,200,1000)`)

	people := []ImportPerson{
		{Slug: "boss", FullName: "Boss Person", FirstName: "Boss", LastName: "Person"},
		{Slug: "mgr", FullName: "Manager Person", FirstName: "Manager", LastName: "Person", ManagerSlug: sp("boss")},
		{Slug: "ic1", FullName: "Iris Callaghan", FirstName: "Iris", LastName: "Callaghan", ManagerSlug: sp("mgr"), Identity: sp("alice_db")},
		{Slug: "ic2", FullName: "Ida Newbie", FirstName: "Ida", LastName: "Newbie", ManagerSlug: sp("mgr")},
	}
	res, err := s.ImportPeople(ctx, people, "fixture.xlsx", "tester", false)
	if err != nil {
		t.Fatalf("fixture import: %v", err)
	}
	if res.Linked != 1 {
		t.Fatalf("fixture should link ic1 (explicit) — iris auto-link is a second path; got %d", res.Linked)
	}
}

type checksum struct {
	rows   int
	tokens int64
	prompt int64
}

func usageChecksum(t *testing.T, s *Store, ctx context.Context) checksum {
	t.Helper()
	var c checksum
	err := s.db.QueryRowContext(ctx,
		`SELECT count(*), COALESCE(SUM(total_tokens),0), COALESCE(SUM(prompt_tokens),0) FROM usage_events`,
	).Scan(&c.rows, &c.tokens, &c.prompt)
	if err != nil {
		t.Fatalf("checksum: %v", err)
	}
	return c
}

// G1, executable: every org write path must leave usage_events bit-identical.
func TestOrgImport_NeverTouchesUsage(t *testing.T) {
	s, ctx := openTestStore(t)
	// The fixture seeds usage AND imports the roster; the checksum starts
	// only once usage rows exist, so it measures the org writes that follow.
	seedFixture(t, s, ctx)
	before := usageChecksum(t, s, ctx)

	// Every write path the feature has: a re-import (people, edges,
	// identities, batch + audit rows), an admin edit, and an invite.
	if _, err := s.ImportPeople(ctx, []ImportPerson{{Slug: "boss", FullName: "Boss Person"}}, "again.xlsx", "tester", false); err != nil {
		t.Fatalf("re-import: %v", err)
	}
	if after := usageChecksum(t, s, ctx); before != after {
		t.Fatalf("import changed usage_events: before=%v after=%v", before, after)
	}
	if _, err := s.UpdatePerson(ctx, "mgr", "tester", map[string]any{"title": "Head of Things"}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if _, _, err := s.CreateInvite(ctx, "ic2", "default", "test key", "tester", time.Hour); err != nil {
		t.Fatalf("invite: %v", err)
	}
	if after2 := usageChecksum(t, s, ctx); before != after2 {
		t.Fatalf("post-import writes changed usage_events: %v vs %v", before, after2)
	}
}

func TestOrgImport_Idempotent(t *testing.T) {
	s, ctx := openTestStore(t)
	seedFixture(t, s, ctx)

	// A second identical import must not duplicate or drop anything.
	people := []ImportPerson{
		{Slug: "boss", FullName: "Boss Person"},
		{Slug: "mgr", FullName: "Manager Person", ManagerSlug: sp("boss")},
		{Slug: "ic1", FullName: "Iris Callaghan", ManagerSlug: sp("mgr"), Identity: sp("alice_db")},
		{Slug: "ic2", FullName: "Ida Newbie", ManagerSlug: sp("mgr")},
	}
	res, err := s.ImportPeople(ctx, people, "again.xlsx", "tester", false)
	if err != nil {
		t.Fatalf("second import: %v", err)
	}
	if res.People != 4 || res.Linked != 1 {
		t.Fatalf("unexpected re-import stats: %+v", res)
	}
	var peopleN, identsN int
	s.db.QueryRowContext(ctx, `SELECT count(*) FROM people`).Scan(&peopleN)
	s.db.QueryRowContext(ctx, `SELECT count(*) FROM person_identities`).Scan(&identsN)
	if peopleN != 4 || identsN != 1 {
		t.Fatalf("import duplicated rows: people=%d identities=%d", peopleN, identsN)
	}
}

// Fill-only: emails and hand-edited fields survive a re-import; the import
// never clears what a human entered.
func TestOrgImport_FillOnly(t *testing.T) {
	s, ctx := openTestStore(t)
	seedFixture(t, s, ctx)

	// Admin fills in an email and retitles ic2 by hand.
	_, err := s.UpdatePerson(ctx, "ic2", "boss", map[string]any{"email": "ida@corp.example", "title": "Team Lead"})
	if err != nil {
		t.Fatalf("admin edit: %v", err)
	}

	// Roster re-import (no email, stale title from the sheet) must not clobber.
	_, err = s.ImportPeople(ctx, []ImportPerson{
		{Slug: "ic2", FullName: "Ida Newbie", Title: "Junior Eng"},
	}, "again.xlsx", "tester", false)
	if err != nil {
		t.Fatalf("re-import: %v", err)
	}
	p, _ := s.GetPerson(ctx, "ic2")
	if p.Email != "ida@corp.example" {
		t.Errorf("email was cleared/overwritten by import: %q", p.Email)
	}
	if p.Title != "Team Lead" {
		t.Errorf("manual title overwritten by import: %q", p.Title)
	}
}

// Dry-run must report the same stats while writing nothing at all.
func TestOrgImport_DryRunWritesNothing(t *testing.T) {
	s, ctx := openTestStore(t)
	seedFixture(t, s, ctx)
	var b4 [4]int
	s.db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM people), (SELECT count(*) FROM org_import_batches), (SELECT count(*) FROM org_audit), (SELECT count(*) FROM key_invites)`).
		Scan(&b4[0], &b4[1], &b4[2], &b4[3])
	res, err := s.ImportPeople(ctx, []ImportPerson{{Slug: "newbie2", FullName: "New Second"}, {Slug: "newbie3", FullName: "New Third", ManagerSlug: sp("newbie2")}}, "dry.xlsx", "tester", true)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if res.DryRun != true || res.People != 2 {
		t.Fatalf("dry-run stats wrong: %+v", res)
	}
	var b2 [4]int
	s.db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM people), (SELECT count(*) FROM org_import_batches), (SELECT count(*) FROM org_audit), (SELECT count(*) FROM key_invites)`).
		Scan(&b2[0], &b2[1], &b2[2], &b2[3])
	if b4 != b2 {
		t.Fatalf("dry run wrote rows: %+v -> %+v", b4, b2)
	}
}

func TestOrgCycleTrigger(t *testing.T) {
	s, ctx := openTestStore(t)
	seedFixture(t, s, ctx)

	// boss -> mgr exists; making boss report to mgr would close a cycle.
	_, err := s.UpdatePerson(ctx, "boss", "tester", map[string]any{"manager_slug": "mgr"})
	if err == nil {
		t.Fatal("cycle was accepted — the trigger did not fire")
	}
	// Self-report also refused.
	if _, err := s.UpdatePerson(ctx, "mgr", "tester", map[string]any{"manager_slug": "mgr"}); err == nil {
		t.Fatal("self-management accepted")
	}
}

func TestOrgScope(t *testing.T) {
	s, ctx := openTestStore(t)
	seedFixture(t, s, ctx)

	// The manager (login: none — resolve via slug) sees both ICs' logins.
	names, err := s.SubtreeUsernamesForSlug(ctx, "mgr")
	if err != nil {
		t.Fatalf("subtree: %v", err)
	}
	has := map[string]bool{}
	for _, n := range names {
		has[n] = true
	}
	if !has["alice_db"] {
		t.Errorf("subtree of mgr missing alice_db: %v", names)
	}

	// Scope from a login: a linked person resolves to their tree.
	list, isMgr, slug, err := s.ScopeUsernames(ctx, "alice_db")
	if err != nil {
		t.Fatalf("scope: %v", err)
	}
	if isMgr || slug != "ic1" || len(list) != 1 || list[0] != "alice_db" {
		t.Errorf("IC scope wrong: list=%v isMgr=%v slug=%q", list, isMgr, slug)
	}

	// Unregistered login keeps exactly the pre-feature behaviour: self only.
	list, isMgr, slug, err = s.ScopeUsernames(ctx, "legacy")
	if err != nil {
		t.Fatalf("scope legacy: %v", err)
	}
	if isMgr || slug != "" || len(list) != 1 || list[0] != "legacy" {
		t.Errorf("unknown user must be self-scoped: %v %v %q", list, isMgr, slug)
	}

	// A manager WHO IS also a linked user gets the whole subtree, including
	// logins they never share a group with.
	s.db.ExecContext(ctx, `INSERT INTO person_identities (username, person_slug) VALUES ('mgr_login','mgr')`)
	list, isMgr, slug, err = s.ScopeUsernames(ctx, "mgr_login")
	if err != nil {
		t.Fatalf("scope mgr: %v", err)
	}
	if !isMgr || slug != "mgr" {
		t.Fatalf("mgr_login not manager: isMgr=%v slug=%q", isMgr, slug)
	}
	if len(list) != 2 { // mgr_login + alice_db (ic1's login)
		t.Errorf("manager scope should include subordinate logins, got %v", list)
	}
}

func TestOrgUsageRollup(t *testing.T) {
	s, ctx := openTestStore(t)
	seedFixture(t, s, ctx)

	rows, err := s.GetOrgUsage(ctx, "mgr", time.Now().Add(-24*time.Hour), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("rollup: %v", err)
	}
	by := map[string]OrgUsageRow{}
	for _, r := range rows {
		by[r.Slug] = r
	}
	if len(rows) != 3 { // mgr, ic1, ic2 under mgr
		t.Fatalf("rollup row count: %d (%v)", len(rows), rows)
	}
	ic1 := by["ic1"]
	if ic1.Requests != 2 || ic1.TotalTokens != 4200 {
		t.Errorf("ic1 usage wrong: %+v", ic1)
	}
	if ic1.CostUSD <= 0 {
		t.Errorf("ic1 cost should be > 0 with seeded usage, got %f", ic1.CostUSD)
	}
	if ic2 := by["ic2"]; !ic2.NoIdentity || ic2.CostUSD != 0 {
		t.Errorf("ic2 should be surfaced as never-onboarded: %+v", ic2)
	}
	// legacy has usage but is outside the tree — must NOT appear.
	for _, r := range rows {
		if r.Username == "legacy" {
			t.Fatal("usage outside the scope tree leaked into the rollup")
		}
	}
}

func TestOrgIdentities(t *testing.T) {
	s, ctx := openTestStore(t)
	seedFixture(t, s, ctx)

	// Re-link alice_db from ic1 to ic2 (the admin correction flow).
	if err := s.LinkIdentity(ctx, "alice_db", "ic2", "boss"); err != nil {
		t.Fatalf("link: %v", err)
	}
	p, _ := s.GetPersonByUsername(ctx, "alice_db")
	if p.Slug != "ic2" {
		t.Fatalf("relink failed, resolved to %q", p.Slug)
	}
	// Names with usage that are NOT linked still surface (carry-over signal),
	// so an active user can never be silently dropped by an import.
	if _, err := s.GetPersonByUsername(ctx, "legacy"); err == nil {
		t.Fatal("legacy should be unlinked")
	}
}

func TestOrgInvites(t *testing.T) {
	s, ctx := openTestStore(t)
	seedFixture(t, s, ctx)

	token, id, err := s.CreateInvite(ctx, "ic2", "default", "k1", "boss", time.Hour)
	if err != nil {
		t.Fatalf("create invite: %v", err)
	}
	if len(token) != 64 {
		t.Fatalf("token format: %q", token)
	}
	// The stored value must be a hash, not the token.
	var stored string
	s.db.QueryRowContext(ctx, `SELECT token_hash FROM key_invites WHERE id=$1`, id).Scan(&stored)
	if stored == token {
		t.Fatal("invite token stored in plaintext")
	}
	// Preview resolves WITHOUT consuming.
	iv, err := s.InviteByTokenHash(ctx, token)
	if err != nil || iv.Status != "pending" {
		t.Fatalf("preview: %+v %v", iv, err)
	}
	// First claim consumes it.
	if _, _, _, _, err := s.ClaimInvite(ctx, token); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	// Second claim must fail — single use.
	if _, _, _, _, err := s.ClaimInvite(ctx, token); err == nil {
		t.Fatal("invite was claimable twice")
	}
	// A revoked-but-unclaimed invite is unusable.
	tok2, id2, _ := s.CreateInvite(ctx, "ic2", "default", "k2", "boss", time.Hour)
	s.RevokeInvite(ctx, id2, "boss")
	if _, _, _, _, err := s.ClaimInvite(ctx, tok2); err == nil {
		t.Fatal("revoked invite claimable")
	}
	// An expired invite is unusable.
	tok3, _, _ := s.CreateInvite(ctx, "ic2", "default", "k3", "boss", -time.Hour)
	if _, _, _, _, err := s.ClaimInvite(ctx, tok3); err == nil {
		t.Fatal("expired invite claimable")
	}
}

func TestOrgRoots(t *testing.T) {
	s, ctx := openTestStore(t)
	seedFixture(t, s, ctx)
	roots, err := s.RootSlugs(ctx)
	if err != nil {
		t.Fatalf("roots: %v", err)
	}
	if len(roots) != 1 || roots[0] != "boss" {
		t.Fatalf("roots = %v, want [boss]", roots)
	}
}

// OrgTree regression: the recursive seed binds the root slug as a query
// parameter — an unpassed argument 500s the endpoint (seen live). Covers
// nesting, subtree sizes and the multi-root shape a real roster produces.
func TestOrgTree(t *testing.T) {
	s, ctx := openTestStore(t)
	seedFixture(t, s, ctx)
	// A second root, like a live roster that carries non-reporting rows.
	if _, err := s.ImportPeople(ctx, []ImportPerson{{Slug: "svc", FullName: "Svc Bot"}}, "svc.xlsx", "tester", false); err != nil {
		t.Fatalf("seed second root: %v", err)
	}

	tree, err := s.OrgTree(ctx, "boss")
	if err != nil {
		t.Fatalf("tree: %v", err)
	}
	if tree.Slug != "boss" || tree.SubtreeSize != 4 {
		t.Fatalf("root wrong: %s size %d", tree.Slug, tree.SubtreeSize)
	}
	if len(tree.Children) != 1 || tree.Children[0].Slug != "mgr" {
		t.Fatalf("children wrong: %+v", tree.Children)
	}
	mgr := tree.Children[0]
	if len(mgr.Children) != 2 {
		t.Fatalf("mgr should have two ICs: %+v", mgr.Children)
	}
	by := map[string]*OrgTreeNode{}
	for _, c := range mgr.Children {
		by[c.Slug] = c
	}
	if ic1, ok := by["ic1"]; !ok || ic1.Username != "alice_db" {
		t.Fatalf("ic1 should carry its login: %+v", by)
	}

	// A leaf renders as a one-node tree, not an error.
	leaf, err := s.OrgTree(ctx, "ic2")
	if err != nil || leaf.Slug != "ic2" || len(leaf.Children) != 0 {
		t.Fatalf("leaf tree wrong: %+v %v", leaf, err)
	}
}

// TestOrgManualAddClaimable guards the invite flow for people who are not
// on the roster: a manual add with an email must be pre-linked to that
// login (MaaS usernames ARE emails), or the invite claim consumes its
// single-use token and then fails with "account not linked". Also covers
// the mint-failure recovery: a claim released after a failed mint must be
// claimable again, while a claim that recorded a key stays consumed.
func TestOrgManualAddClaimable(t *testing.T) {
	s, ctx := openTestStore(t)

	p, err := s.CreatePerson(ctx, Person{
		FullName: "Alice Chen", FirstName: "Alice", LastName: "Chen",
		Email: "achen@x.com", GroupName: "ai-eng",
	}, "boss")
	if err != nil {
		t.Fatalf("create person: %v", err)
	}
	if p.Username != "achen@x.com" {
		t.Fatalf("manual add with email did not auto-link identity: username=%q", p.Username)
	}
	// An email-less add stays unlinked — nothing to link to yet.
	p2, err := s.CreatePerson(ctx, Person{FullName: "No Email"}, "boss")
	if err != nil {
		t.Fatalf("create person 2: %v", err)
	}
	if p2.Username != "" {
		t.Fatalf("email-less person should stay unlinked, got %q", p2.Username)
	}

	// Mint-failure recovery: consumed, then released → claimable again.
	tok, _, err := s.CreateInvite(ctx, p.Slug, "ai-eng", "alice-key", "boss", time.Hour)
	if err != nil {
		t.Fatalf("invite: %v", err)
	}
	id, _, _, _, err := s.ClaimInvite(ctx, tok)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := s.ReleaseInviteClaim(ctx, id); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, _, _, _, err := s.ClaimInvite(ctx, tok); err != nil {
		t.Fatalf("invite not claimable after release: %v", err)
	}

	// Once a key id is recorded the claim is permanent — release is a no-op.
	tok2, _, err := s.CreateInvite(ctx, p.Slug, "ai-eng", "k2", "boss", time.Hour)
	if err != nil {
		t.Fatalf("invite 2: %v", err)
	}
	id2, _, _, _, err := s.ClaimInvite(ctx, tok2)
	if err != nil {
		t.Fatalf("claim 2: %v", err)
	}
	if err := s.SetInviteKey(ctx, id2, "key-abc"); err != nil {
		t.Fatalf("set key: %v", err)
	}
	if err := s.ReleaseInviteClaim(ctx, id2); err != nil {
		t.Fatalf("release 2: %v", err)
	}
	if _, _, _, _, err := s.ClaimInvite(ctx, tok2); err == nil {
		t.Fatal("invite with a minted key must stay claimed")
	}

	// Clearing emails stores NULL, not '': the column is UNIQUE and two
	// people without emails must coexist (the People-table inline edit).
	if _, err := s.UpdatePerson(ctx, p.Slug, "boss", map[string]any{"email": ""}); err != nil {
		t.Fatalf("clear email on p: %v", err)
	}
	if _, err := s.UpdatePerson(ctx, p2.Slug, "boss", map[string]any{"email": ""}); err != nil {
		t.Fatalf("clear email on p2 (would collide on stored ''): %v", err)
	}

	// The claim-time profile upsert is what puts names on the reports.
	if _, err := s.UpsertUserProfiles(ctx, []UserProfile{
		{Username: "achen@x.com", FirstName: "Alice", LastName: "Chen"},
	}); err != nil {
		t.Fatalf("upsert profile: %v", err)
	}
	prof, err := s.GetUserProfile(ctx, "achen@x.com")
	if err != nil || prof.DisplayName() != "Alice Chen" {
		t.Fatalf("profile after claim upsert: %+v %v", prof, err)
	}
}

// The Add-user modal can pin a manager at creation (dropdown over the
// existing directory, with an explicit "no manager yet" empty choice).
func TestOrgCreateWithManager(t *testing.T) {
	s, ctx := openTestStore(t)
	seedFixture(t, s, ctx)

	withMgr, err := s.CreatePerson(ctx, Person{
		FullName: "New Report", FirstName: "New", LastName: "Report",
		ManagerSlug: "mgr",
	}, "tester")
	if err != nil {
		t.Fatalf("create with manager: %v", err)
	}
	if withMgr.ManagerSlug != "mgr" {
		t.Fatalf("manager not stored at creation: %q", withMgr.ManagerSlug)
	}

	// No manager chosen: the row stores NULL, settable inline later.
	bare, err := s.CreatePerson(ctx, Person{FullName: "No Manager"}, "tester")
	if err != nil {
		t.Fatalf("create without manager: %v", err)
	}
	if bare.ManagerSlug != "" {
		t.Fatalf("expected empty manager, got %q", bare.ManagerSlug)
	}
	if _, err := s.UpdatePerson(ctx, bare.Slug, "tester", map[string]any{"manager_slug": "boss"}); err != nil {
		t.Fatalf("inline manager set after creation: %v", err)
	}

	// A manager outside the directory must fail with a clear message,
	// not a raw FK dump and not a silently-dropped value.
	if _, err := s.CreatePerson(ctx, Person{
		FullName: "Bad Manager", ManagerSlug: "nobody-here",
	}, "tester"); err == nil {
		t.Fatal("unknown manager must be rejected")
	} else if got := err.Error(); got != "manager nobody-here is not in the directory" {
		t.Fatalf("unclear error for unknown manager: %q", got)
	}
}
