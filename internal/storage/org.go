package storage

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/lib/pq"
)

// Org directory: the people table, the identities that link a person to a
// login (usage_events.username), and everything the manager view needs on
// top: subtree resolution, key invites, import batches, and audit.
//
// The design guarantees that hold across every method here:
//   - Nothing in this layer ever writes usage_events. Spend is derived at
//     query time from usage_events joined through person_identities; a
//     person row carries no spend column at all, so no import or edit can
//     zero out a number that already exists.
//   - Import is fill-only: org fields update, emails COALESCE (filled,
//     never cleared), and any column an admin edited by hand (tracked in
//     manual_fields) is left alone so a future sync can never clobber it.

// Person is one row of the org directory.
type Person struct {
	Slug           string    `json:"slug"`
	FullName       string    `json:"full_name"`
	FirstName      string    `json:"first_name"`
	LastName       string    `json:"last_name"`
	Title          string    `json:"title"`
	Location       string    `json:"location"`
	Email          string    `json:"email"`
	EmploymentType string    `json:"employment_type"`
	Aliases        []string  `json:"aliases"`
	IsService      bool      `json:"is_service"`
	Active         bool      `json:"active"`
	NeedsReview    bool      `json:"needs_review"`
	ManualFields   []string  `json:"manual_fields"`
	Source         string    `json:"source"`
	ManagerSlug    string    `json:"manager_slug,omitempty"`
	ManagerName    string    `json:"manager_name,omitempty"`
	Username       string    `json:"username,omitempty"` // primary linked login, "" when unlinked
	Reports        int       `json:"reports"`
	GroupName      string    `json:"group_name"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// ImportPerson mirrors one entry of the roster import payload (see
// Downloads/pricetag-import-roster.py for the generator). Fields the
// payload does not carry (spend, onboarded) deliberately have no column —
// spend lives in usage_events and is derived, never asserted.
type ImportPerson struct {
	Slug               string   `json:"slug"`
	FullName           string   `json:"full_name"`
	FirstName          string   `json:"first_name"`
	LastName           string   `json:"last_name"`
	Aliases            []string `json:"aliases"`
	Email              *string  `json:"email"`
	Title              string   `json:"title"`
	Location           string   `json:"location"`
	EmploymentType     string   `json:"employment_type"`
	ManagerSlug        *string  `json:"manager_slug"`
	Identity           *string  `json:"identity"`
	UsernameCandidates []struct {
		Username   string  `json:"username"`
		Confidence float64 `json:"confidence"`
		Rule       string  `json:"rule"`
	} `json:"username_candidates"`
}

// ImportResult reports what an import did (or would have done when dry).
type ImportResult struct {
	DryRun          bool     `json:"dry_run"`
	People          int      `json:"people"`
	Managers        int      `json:"managers"`
	Contractors     int      `json:"contractors"`
	Linked          int      `json:"linked"`
	AutoLinked      int      `json:"auto_linked"`
	NeedsReview     int      `json:"needs_review"`
	Orphans         int      `json:"orphans"`
	MissingEmail    int      `json:"missing_email"`
	UnlinkedUsernam []string `json:"unlinked_usage_usernames"`
	OrgFieldsOnly   bool     `json:"org_fields_only"`
	BatchID         int64    `json:"batch_id,omitempty"`
}

// orgMigrations run after the core migrations, appended so the core list
// stays untouched. Every statement is idempotent (IF NOT EXISTS / OR
// REPLACE), same contract as the rest of the migrate loop.
var orgMigrations = []string{
	`CREATE TABLE IF NOT EXISTS people (
		slug TEXT PRIMARY KEY,
		full_name TEXT NOT NULL,
		first_name TEXT NOT NULL DEFAULT '',
		last_name TEXT NOT NULL DEFAULT '',
		title TEXT NOT NULL DEFAULT '',
		location TEXT NOT NULL DEFAULT '',
		email TEXT UNIQUE,
		employment_type TEXT NOT NULL DEFAULT 'employee',
		aliases TEXT[] NOT NULL DEFAULT '{}',
		is_service BOOLEAN NOT NULL DEFAULT false,
		active BOOLEAN NOT NULL DEFAULT true,
		needs_review BOOLEAN NOT NULL DEFAULT false,
		manual_fields TEXT[] NOT NULL DEFAULT '{}',
		source TEXT NOT NULL DEFAULT 'manual',
		manager_slug TEXT REFERENCES people(slug) ON DELETE SET NULL,
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`,
	`CREATE INDEX IF NOT EXISTS idx_people_manager ON people (manager_slug)`,
	`ALTER TABLE people ADD COLUMN IF NOT EXISTS group_name TEXT NOT NULL DEFAULT ''`,
	`CREATE INDEX IF NOT EXISTS idx_people_group ON people (group_name)`,
	// A manager chain must stay a tree: the manager view aggregates whole
	// subtrees, and a cycle would make the recursive walk never terminate.
	// Enforced in the database so no code path (import, PATCH, future sync)
	// can introduce one.
	`CREATE OR REPLACE FUNCTION people_no_cycle() RETURNS trigger AS $$
DECLARE
	cur TEXT := NEW.manager_slug;
	depth INT := 0;
BEGIN
	IF NEW.manager_slug IS NULL THEN
		RETURN NEW;
	END IF;
	IF NEW.manager_slug = NEW.slug THEN
		RAISE EXCEPTION 'people: % cannot report to itself', NEW.slug;
	END IF;
	WHILE cur IS NOT NULL LOOP
		IF cur = NEW.slug THEN
			RAISE EXCEPTION 'people: management cycle detected involving %', NEW.slug;
		END IF;
		SELECT manager_slug INTO cur FROM people WHERE slug = cur;
		depth := depth + 1;
		IF depth > 1000 THEN
			RAISE EXCEPTION 'people: management chain deeper than 1000 (corrupt data)';
		END IF;
	END LOOP;
	RETURN NEW;
END
$$ LANGUAGE plpgsql`,
	`DROP TRIGGER IF EXISTS trg_people_no_cycle ON people`,
	`CREATE TRIGGER trg_people_no_cycle BEFORE INSERT OR UPDATE OF manager_slug ON people FOR EACH ROW EXECUTE FUNCTION people_no_cycle()`,
	// A login (usage_events.username) belongs to exactly one person per
	// provider; a person may carry several (provider migration, aliases).
	`CREATE TABLE IF NOT EXISTS person_identities (
		provider TEXT NOT NULL DEFAULT 'maas',
		username TEXT NOT NULL,
		person_slug TEXT NOT NULL REFERENCES people(slug) ON DELETE CASCADE,
		is_service BOOLEAN NOT NULL DEFAULT false,
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		PRIMARY KEY (provider, username)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_pi_person ON person_identities (person_slug)`,
	// Explicit visibility grants beyond the manager chain (e.g. a skip-level
	// or cross-team viewer). Reserved for the proper-auth integration; the
	// manager view does not depend on rows existing here.
	`CREATE TABLE IF NOT EXISTS visibility_grants (
		id BIGSERIAL PRIMARY KEY,
		grantee TEXT NOT NULL,
		scope TEXT NOT NULL DEFAULT 'subtree',
		target_slug TEXT,
		granted_by TEXT NOT NULL,
		expires_at TIMESTAMPTZ,
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`,
	// Key invites: only the SHA-256 of the single-use token is ever stored,
	// so a database leak exposes no usable invite, and the API key itself is
	// minted at claim time by the claimant's own request — the plaintext key
	// is never persisted anywhere in this service.
	`CREATE TABLE IF NOT EXISTS key_invites (
		id BIGSERIAL PRIMARY KEY,
		token_hash TEXT NOT NULL UNIQUE,
		person_slug TEXT NOT NULL REFERENCES people(slug) ON DELETE CASCADE,
		group_name TEXT NOT NULL DEFAULT '',
		key_name TEXT NOT NULL DEFAULT '',
		created_by TEXT NOT NULL,
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		expires_at TIMESTAMPTZ NOT NULL,
		claimed_at TIMESTAMPTZ,
		key_id TEXT,
		revoked_at TIMESTAMPTZ
	)`,
	`CREATE TABLE IF NOT EXISTS org_import_batches (
		id BIGSERIAL PRIMARY KEY,
		filename TEXT NOT NULL DEFAULT '',
		actor TEXT NOT NULL,
		dry_run BOOLEAN NOT NULL DEFAULT true,
		stats JSONB NOT NULL DEFAULT '{}',
		diff JSONB NOT NULL DEFAULT '{}',
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`,
	`CREATE TABLE IF NOT EXISTS org_audit (
		id BIGSERIAL PRIMARY KEY,
		actor TEXT NOT NULL,
		action TEXT NOT NULL,
		target TEXT NOT NULL DEFAULT '',
		detail JSONB NOT NULL DEFAULT '{}',
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`,
}

func (s *Store) migrateOrg(ctx context.Context) error {
	for _, stmt := range orgMigrations {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("org migration failed: %w", err)
		}
	}
	return nil
}

// slugNorm normalizes a slug coming from user input or a payload: the roster
// file's "<position>_<first>_<last>" identifiers must never be trusted as
// keys (the position is a row counter — one departure renumbers everyone).
func SlugNorm(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			return r
		default:
			return '_'
		}
	}, s)
	for strings.Contains(s, "__") { // "José" folds to "jos__" — collapse runs
		s = strings.ReplaceAll(s, "__", "_")
	}
	return strings.Trim(s, "_")
}

// Audit records one actor's action. Every write path here calls it with the
// REAL session identity (the handler resolves the impersonation header), so
// "who was really looking/editing" survives admin view-as.
func (s *Store) Audit(ctx context.Context, actor, action, target string, detail any) error {
	b := []byte("{}")
	if detail != nil {
		if enc, err := json.Marshal(detail); err == nil {
			b = enc
		}
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO org_audit (actor, action, target, detail) VALUES ($1, $2, $3, $4)`,
		actor, action, target, string(b))
	return err
}

// ImportPeople upserts the roster directory in one transaction. Dry runs
// compute the same diff and record the batch without writing any data.
// Fill-only rules (see file header) apply to both.
func (s *Store) ImportPeople(ctx context.Context, people []ImportPerson, filename, actor string, dryRun bool) (ImportResult, error) {
	res := ImportResult{DryRun: dryRun, OrgFieldsOnly: true}
	bySlug := make(map[string]ImportPerson, len(people))

	for _, p := range people {
		slug := SlugNorm(p.Slug)
		if slug == "" {
			return res, fmt.Errorf("import row with empty slug: %q", p.FullName)
		}
		if _, dup := bySlug[slug]; dup {
			return res, fmt.Errorf("duplicate slug in payload: %s", slug)
		}
		p.Slug = slug
		bySlug[slug] = p
		res.People++
		if p.EmploymentType == "contractor" {
			res.Contractors++
		}
		if p.ManagerSlug != nil && *p.ManagerSlug != "" {
			res.Managers++ // count rows that manage someone = rows with a manager set below; corrected after walk
		}
	}
	// "managers" in the result means people who HAVE reports (the org-shape
	// count), not people who have a manager. Recompute from the edges.
	res.Managers = 0
	hasReports := map[string]bool{}
	for _, p := range bySlug {
		if p.ManagerSlug != nil && *p.ManagerSlug != "" {
			hasReports[SlugNorm(*p.ManagerSlug)] = true
		}
	}
	for slug := range hasReports {
		if _, ok := bySlug[slug]; ok {
			res.Managers++
		}
	}

	// Orphans: manager_slug points outside the payload (and not at an
	// existing DB person when applying).
	var existing map[string]bool
	if dryRun {
		existing = map[string]bool{}
	} else {
		var err error
		existing, err = s.personSlugSet(ctx)
		if err != nil {
			return res, err
		}
	}
	for _, p := range bySlug {
		if p.ManagerSlug != nil && *p.ManagerSlug != "" {
			m := SlugNorm(*p.ManagerSlug)
			if _, ok := bySlug[m]; !ok && !existing[m] {
				res.Orphans++
			}
		}
		if p.Email == nil || *p.Email == "" {
			res.MissingEmail++
		}
	}

	// Auto-link rule: an explicit identity always wins; otherwise link only
	// on an EXACT display-name match against user_profiles (zero guessing in
	// the application layer — name heuristics stay in the offline generator
	// where a human reviews them).
	autoCandidates := map[string][]string{} // full name lower -> usernames
	profRows, err := s.db.QueryContext(ctx, `SELECT username, first_name, last_name FROM user_profiles`)
	if err != nil {
		return res, err
	}
	for profRows.Next() {
		var u, f, l string
		if err := profRows.Scan(&u, &f, &l); err != nil {
			profRows.Close()
			return res, err
		}
		name := strings.ToLower(strings.TrimSpace(f + " " + l))
		if name != "" {
			autoCandidates[name] = append(autoCandidates[name], u)
		}
	}
	profRows.Close()
	if err := profRows.Err(); err != nil {
		return res, err
	}

	for _, p := range bySlug {
		link := ""
		if p.Identity != nil && *p.Identity != "" {
			link = *p.Identity
		} else if us, ok := autoCandidates[strings.ToLower(strings.TrimSpace(p.FullName))]; ok && len(us) == 1 {
			link = us[0]
			res.AutoLinked++
		}
		if link != "" {
			res.Linked++
		}
	}

	// Usernames with usage that no payload person links to — surfaced so a
	// human notices them (these people keep their usage and their dashboard;
	// the import never touches them beyond reporting).
	res.UnlinkedUsernam = s.usernamesWithUsageNotIn(ctx, bySlug, autoCandidates)

	if dryRun {
		return res, nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return res, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // committed or rolled back

	// Pass 1 — people rows, org fields only. Manager stays NULL so the FK
	// never sees an edge whose target is not yet in the table.
	for _, p := range people {
		p.Slug = SlugNorm(p.Slug)
		email := sqlAnyString(p.Email)
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO people (slug, full_name, first_name, last_name, title, location, email, employment_type, aliases, source)
			VALUES ($1, $2, $3, $4, $5, $6, $7, CASE WHEN $8 = '' THEN 'employee' ELSE $8 END, $9, 'import')
			ON CONFLICT (slug) DO UPDATE SET
				full_name = CASE WHEN 'full_name' = ANY(people.manual_fields) THEN people.full_name ELSE EXCLUDED.full_name END,
				first_name = CASE WHEN 'first_name' = ANY(people.manual_fields) THEN people.first_name ELSE EXCLUDED.first_name END,
				last_name = CASE WHEN 'last_name' = ANY(people.manual_fields) THEN people.last_name ELSE EXCLUDED.last_name END,
				title = CASE WHEN 'title' = ANY(people.manual_fields) THEN people.title ELSE EXCLUDED.title END,
				location = CASE WHEN 'location' = ANY(people.manual_fields) THEN people.location ELSE EXCLUDED.location END,
				-- an absent/empty employment_type in the payload never erases
				-- a known value; $8 is the raw parameter, EXCLUDED would carry
				-- the 'employee' default filled in VALUES above.
				employment_type = CASE
					WHEN 'employment_type' = ANY(people.manual_fields) THEN people.employment_type
					WHEN $8 = '' THEN people.employment_type
					ELSE $8
				END,
				email = COALESCE(people.email, EXCLUDED.email),
				aliases = ARRAY(SELECT DISTINCT unnest(people.aliases || EXCLUDED.aliases)),
				updated_at = NOW()`,
			p.Slug, p.FullName, p.FirstName, p.LastName, p.Title, p.Location, email,
			p.EmploymentType, pgArray(p.Aliases)); err != nil {
			return res, fmt.Errorf("upsert person %s: %w", p.Slug, err)
		}
	}

	// Pass 2 — manager edges. The cycle trigger guards every edge; an edge
	// whose target is missing leaves the person at the top level (surfaced
	// as an orphan in stats), it never fails the whole import.
	for _, p := range people {
		p.Slug = SlugNorm(p.Slug)
		if p.ManagerSlug == nil || *p.ManagerSlug == "" {
			continue
		}
		m := SlugNorm(*p.ManagerSlug)
		_, err := tx.ExecContext(ctx, `
			UPDATE people SET manager_slug = $2, updated_at = NOW()
			WHERE slug = $1
			  AND NOT ('manager_slug' = ANY(manual_fields))
			  AND EXISTS (SELECT 1 FROM people WHERE slug = $2)`,
			p.Slug, m)
		if err != nil {
			return res, fmt.Errorf("set manager %s -> %s: %w", p.Slug, m, err)
		}
	}

	// Pass 3 — identities (explicit or unique exact-name auto match).
	for _, p := range people {
		p.Slug = SlugNorm(p.Slug)
		link := ""
		if p.Identity != nil && *p.Identity != "" {
			link = *p.Identity
		} else if us, ok := autoCandidates[strings.ToLower(strings.TrimSpace(p.FullName))]; ok && len(us) == 1 {
			link = us[0]
		}
		if link == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO person_identities (username, person_slug)
			VALUES ($1, $2) ON CONFLICT (provider, username) DO NOTHING`,
			link, p.Slug); err != nil {
			return res, fmt.Errorf("link identity %s: %w", link, err)
		}
	}

	var batchID int64
	stats, _ := json.Marshal(res)
	if err := tx.QueryRowContext(ctx,
		`INSERT INTO org_import_batches (filename, actor, dry_run, stats) VALUES ($1, $2, false, $3) RETURNING id`,
		filename, actor, string(stats)).Scan(&batchID); err != nil {
		return res, fmt.Errorf("record batch: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return res, fmt.Errorf("commit: %w", err)
	}
	res.BatchID = batchID
	return res, nil
}

func (s *Store) personSlugSet(ctx context.Context) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT slug FROM people`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	set := map[string]bool{}
	for rows.Next() {
		var slug string
		if err := rows.Scan(&slug); err != nil {
			return nil, err
		}
		set[slug] = true
	}
	return set, rows.Err()
}

// usernamesWithUsageNotIn returns login names that have usage_events rows
// but are not linked to any imported person — the carry-over signal so an
// active user is never silently dropped from the directory's view.
func (s *Store) usernamesWithUsageNotIn(ctx context.Context, bySlug map[string]ImportPerson, auto map[string][]string) []string {
	linked := map[string]bool{}
	for _, p := range bySlug {
		if p.Identity != nil && *p.Identity != "" {
			linked[*p.Identity] = true
		}
		if us, ok := auto[strings.ToLower(strings.TrimSpace(p.FullName))]; ok && len(us) == 1 {
			linked[us[0]] = true
		}
	}
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT username FROM usage_events ORDER BY username`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return nil
		}
		if !linked[u] {
			out = append(out, u)
		}
	}
	return out
}

// --- People reads / edits ---

const personSelect = `
	SELECT p.slug, p.full_name, p.first_name, p.last_name, p.title, p.location,
		COALESCE(p.email, ''), p.employment_type, p.aliases, p.is_service, p.active, p.needs_review,
		p.manual_fields, p.source, COALESCE(p.manager_slug, ''), COALESCE(m.full_name, ''),
		COALESCE(pi.username, ''),
		(SELECT COUNT(*) FROM people r WHERE r.manager_slug = p.slug),
		p.group_name,
		p.updated_at
	FROM people p
	LEFT JOIN people m ON m.slug = p.manager_slug
	LEFT JOIN person_identities pi ON pi.person_slug = p.slug
`

func scanPerson(row interface{ Scan(...any) error }) (Person, error) {
	var p Person
	err := row.Scan(&p.Slug, &p.FullName, &p.FirstName, &p.LastName, &p.Title, &p.Location,
		&p.Email, &p.EmploymentType, pq.Array(&p.Aliases), &p.IsService, &p.Active, &p.NeedsReview,
		pq.Array(&p.ManualFields), &p.Source, &p.ManagerSlug, &p.ManagerName, &p.Username, &p.Reports,
		&p.GroupName, &p.UpdatedAt)
	if p.Aliases == nil {
		p.Aliases = []string{}
	}
	if p.ManualFields == nil {
		p.ManualFields = []string{}
	}
	return p, err
}

// ListPeople returns the directory. filter: "" (all), "needs_attention"
// (missing email or identity, or flagged), "managers" (has reports),
// "no_key" is applied in the handler (needs the key service).
func (s *Store) ListPeople(ctx context.Context, filter string) ([]Person, error) {
	where := ""
	switch filter {
	case "needs_attention":
		where = `WHERE (p.email IS NULL OR pi.username IS NULL OR p.needs_review OR p.active IS NOT TRUE)`
	case "managers":
		where = `WHERE EXISTS (SELECT 1 FROM people r WHERE r.manager_slug = p.slug)`
	}
	rows, err := s.db.QueryContext(ctx, personSelect+where+" ORDER BY p.full_name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Person
	for rows.Next() {
		p, err := scanPerson(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// RootSlugs lists top-of-org people (no manager), used as the default tree
// root for admins and the org chart's landing view.
func (s *Store) RootSlugs(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT slug FROM people WHERE manager_slug IS NULL ORDER BY full_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var slug string
		if err := rows.Scan(&slug); err != nil {
			return nil, err
		}
		out = append(out, slug)
	}
	return out, rows.Err()
}

// IdentityRow joins a login to its person for the admin Identities screen.
type IdentityRow struct {
	Provider   string `json:"provider"`
	Username   string `json:"username"`
	Slug       string `json:"person_slug"`
	PersonName string `json:"person_name"`
	HasUsage   bool   `json:"has_usage"`
}

func (s *Store) ListIdentities(ctx context.Context) ([]IdentityRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT pi.provider, pi.username, pi.person_slug, p.full_name,
			EXISTS (SELECT 1 FROM usage_events e WHERE e.username = pi.username)
		FROM person_identities pi JOIN people p ON p.slug = pi.person_slug
		ORDER BY pi.username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []IdentityRow
	for rows.Next() {
		var ir IdentityRow
		if err := rows.Scan(&ir.Provider, &ir.Username, &ir.Slug, &ir.PersonName, &ir.HasUsage); err != nil {
			return nil, err
		}
		out = append(out, ir)
	}
	return out, rows.Err()
}

func (s *Store) GetPerson(ctx context.Context, slug string) (Person, error) {
	row := s.db.QueryRowContext(ctx, personSelect+`WHERE p.slug = $1`, slug)
	return scanPerson(row)
}

// GetPersonByUsername resolves a login name to its directory person.
// sql.ErrNoRows means "not in the directory" — callers treat that as
// self-scope, identical to today's behaviour for unknown users.
func (s *Store) GetPersonByUsername(ctx context.Context, username string) (Person, error) {
	rows, err := s.db.QueryContext(ctx, personSelect+`WHERE pi.username = $1 LIMIT 1`, username)
	if err != nil {
		return Person{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		return Person{}, sql.ErrNoRows
	}
	return scanPerson(rows)
}

// UpdatePersonFields applies admin edits. Every column an admin touches is
// added to manual_fields so future imports leave it alone.
var editablePersonColumns = map[string]bool{
	"full_name": true, "first_name": true, "last_name": true, "title": true,
	"location": true, "email": true, "employment_type": true, "manager_slug": true,
	"active": true, "needs_review": true, "is_admin": true, "group_name": true,
}

// UpdatePerson applies the given column set (keys restricted to the map
// above) and records them as manual. manager_slug may be "" to clear.
func (s *Store) UpdatePerson(ctx context.Context, slug string, actor string, fields map[string]any) (Person, error) {
	sets := []string{}
	args := []any{}
	manual := []string{}
	i := 1
	for col, val := range fields {
		if !editablePersonColumns[col] {
			return Person{}, fmt.Errorf("field not editable: %s", col)
		}
		if col == "manager_slug" {
			if v, ok := val.(string); ok && v == "" {
				sets = append(sets, "manager_slug = NULL")
			} else {
				sets = append(sets, fmt.Sprintf("manager_slug = $%d", i))
				args = append(args, SlugNorm(fmt.Sprint(val)))
				i++
			}
		} else {
			sets = append(sets, fmt.Sprintf("%s = $%d", col, i))
			args = append(args, val)
			i++
		}
		manual = append(manual, col)
	}
	if len(sets) == 0 {
		return s.GetPerson(ctx, slug)
	}
	sets = append(sets, "updated_at = NOW()")
	sets = append(sets, fmt.Sprintf("manual_fields = ARRAY(SELECT DISTINCT unnest(manual_fields || $%d::text[]))", i))
	args = append(args, pgArray(manual), slug)
	if _, err := s.db.ExecContext(ctx,
		fmt.Sprintf(`UPDATE people SET %s WHERE slug = $%d`, strings.Join(sets, ", "), i+1), args...); err != nil {
		return Person{}, err
	}
	if err := s.Audit(ctx, actor, "person.update", slug, fields); err != nil {
		return Person{}, err
	}
	return s.GetPerson(ctx, slug)
}

// CreatePerson adds a directory entry by hand (admins may add someone the
// roster missed). Slug is derived from the name when empty.
func (s *Store) CreatePerson(ctx context.Context, p Person, actor string) (Person, error) {
	if p.Slug == "" {
		p.Slug = SlugNorm(p.FullName)
	}
	if p.Slug == "" {
		return Person{}, fmt.Errorf("name required")
	}
	email := p.Email
	if email == "" {
		email = "\x00" // sentinel replaced by NULL below
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO people (slug, full_name, first_name, last_name, title, location, email, employment_type, group_name, source)
		VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, chr(0)), CASE WHEN $8 = '' THEN 'employee' ELSE $8 END, $9, 'manual')`,
		p.Slug, p.FullName, p.FirstName, p.LastName, p.Title, p.Location, email, p.EmploymentType, p.GroupName)
	if err != nil {
		if strings.Contains(err.Error(), "duplicate key") {
			return Person{}, fmt.Errorf("a person with slug %s already exists", p.Slug)
		}
		return Person{}, err
	}
	_ = s.Audit(ctx, actor, "person.create", p.Slug, nil)
	return s.GetPerson(ctx, p.Slug)
}

// LinkIdentity binds a login name to a person (admin correction of a
// mis-guessed or renamed account).
func (s *Store) LinkIdentity(ctx context.Context, username, slug, actor string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE person_identities SET person_slug = $2 WHERE username = $1 AND provider = 'maas'`, username, slug)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		_, err = s.db.ExecContext(ctx, `
			INSERT INTO person_identities (username, person_slug) VALUES ($1, $2)`, username, slug)
		if err != nil {
			return err
		}
	}
	return s.Audit(ctx, actor, "identity.link", username, map[string]string{"person": slug})
}

func (s *Store) UnlinkIdentity(ctx context.Context, username, actor string) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM person_identities WHERE username = $1 AND provider = 'maas'`, username); err != nil {
		return err
	}
	return s.Audit(ctx, actor, "identity.unlink", username, nil)
}

// --- Scope resolution (the auth core) ---

// ScopeUsernames returns the full set of login names a caller may see:
// themselves, plus every descendant's linked logins when they are a
// manager. Self is always first and always included, linked or not.
// Returns isManager = true when the person has at least one direct report.
func (s *Store) ScopeUsernames(ctx context.Context, username string) (usernames []string, isManager bool, personSlug string, err error) {
	person, perr := s.GetPersonByUsername(ctx, username)
	if perr == sql.ErrNoRows {
		return []string{username}, false, "", nil
	}
	if perr != nil {
		return nil, false, "", perr
	}
	isManager = person.Reports > 0

	rows, err := s.db.QueryContext(ctx, `
		WITH RECURSIVE subtree(slug) AS (
			SELECT $1
			UNION ALL
			SELECT p.slug FROM people p JOIN subtree st ON p.manager_slug = st.slug
		)
		SELECT pi.username
		FROM subtree st
		JOIN person_identities pi ON pi.person_slug = st.slug AND pi.is_service = false
		ORDER BY pi.username`, person.Slug)
	if err != nil {
		return nil, false, "", err
	}
	defer rows.Close()
	seen := map[string]bool{username: true}
	out := []string{username}
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return nil, false, "", err
		}
		if !seen[u] {
			out = append(out, u)
			seen[u] = true
		}
	}
	return out, isManager, person.Slug, rows.Err()
}

// SubtreeUsernamesForSlug resolves from a person slug (used by org usage
// and the org chart for admin drill-down).
func (s *Store) SubtreeUsernamesForSlug(ctx context.Context, slug string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		WITH RECURSIVE subtree(slug) AS (
			SELECT $1
			UNION ALL
			SELECT p.slug FROM people p JOIN subtree st ON p.manager_slug = st.slug
		)
		SELECT pi.username
		FROM subtree st
		JOIN person_identities pi ON pi.person_slug = st.slug AND pi.is_service = false
		ORDER BY pi.username`, slug)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// OrgTreeNode is one node of the org chart with its directory fields.
type OrgTreeNode struct {
	Slug        string         `json:"slug"`
	FullName    string         `json:"full_name"`
	Title       string         `json:"title"`
	Username    string         `json:"username,omitempty"`
	IsManager   bool           `json:"is_manager"`
	Reports     int            `json:"reports"`
	SubtreeSize int            `json:"subtree_size"`
	Children    []*OrgTreeNode `json:"children,omitempty"`
}

// OrgTree returns the subtree rooted at slug, fully materialized. Depth is
// capped defensively even though the cycle trigger should make that
// unreachable.
func (s *Store) OrgTree(ctx context.Context, rootSlug string) (*OrgTreeNode, error) {
	rows, err := s.db.QueryContext(ctx, `
		WITH RECURSIVE subtree(slug, depth) AS (
			SELECT $1, 0
			UNION ALL
			SELECT p.slug, st.depth + 1 FROM people p JOIN subtree st ON p.manager_slug = st.slug
			WHERE st.depth < 50
		)
		SELECT s.slug, s.depth FROM subtree s ORDER BY s.depth, s.slug`, rootSlug)
	if err != nil {
		return nil, err
	}
	type row struct {
		slug  string
		depth int
	}
	var rowsOut []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.slug, &r.depth); err != nil {
			rows.Close()
			return nil, err
		}
		rowsOut = append(rowsOut, r)
	}
	rows.Close()
	if len(rowsOut) == 0 {
		return nil, sql.ErrNoRows
	}

	nodes := map[string]*OrgTreeNode{}
	for _, r := range rowsOut {
		p, err := s.GetPerson(ctx, r.slug)
		if err != nil {
			return nil, err
		}
		nodes[p.Slug] = &OrgTreeNode{
			Slug: p.Slug, FullName: p.FullName, Title: p.Title,
			Username: p.Username, IsManager: p.Reports > 0, Reports: p.Reports,
			SubtreeSize: 1,
		}
	}
	var root *OrgTreeNode
	for _, r := range rowsOut {
		n := nodes[r.slug]
		if r.slug == rootSlug {
			root = n
			continue
		}
		p, err := s.GetPerson(ctx, r.slug)
		if err != nil {
			return nil, err
		}
		if parent, ok := nodes[p.ManagerSlug]; ok {
			parent.Children = append(parent.Children, n)
		}
	}
	if root == nil {
		return nil, sql.ErrNoRows
	}
	// bottom-up subtree sizes
	var size func(n *OrgTreeNode) int
	size = func(n *OrgTreeNode) int {
		total := 1
		for _, c := range n.Children {
			total += size(c)
		}
		n.SubtreeSize = total
		return total
	}
	size(root)
	return root, nil
}

// OrgUsageRow is one person's usage rollup, attributed through their
// identity link (person_identities) rather than grouped by raw username,
// so a person with several logins appears once.
type OrgUsageRow struct {
	Slug        string  `json:"slug"`
	FullName    string  `json:"full_name"`
	Username    string  `json:"username,omitempty"`
	ManagerSlug string  `json:"manager_slug,omitempty"`
	IsManager   bool    `json:"is_manager"`
	Requests    int     `json:"requests"`
	TotalTokens int64   `json:"total_tokens"`
	CostUSD     float64 `json:"cost_usd"`
	LastUsed    *string `json:"last_used,omitempty"`
	NoIdentity  bool    `json:"no_identity,omitempty"`
}

// GetOrgUsage rolls usage up per person for the whole subtree under rootSlug
// over a window. It reuses costUSDExpr — one cost model, shared with every
// other dashboard number — and reads usage_events without writing anything.
func (s *Store) GetOrgUsage(ctx context.Context, rootSlug string, since, until time.Time) ([]OrgUsageRow, error) {
	query := fmt.Sprintf(`
		WITH RECURSIVE subtree(slug) AS (
			SELECT $1
			UNION ALL
			SELECT p.slug FROM people p JOIN subtree st ON p.manager_slug = st.slug
		),
		u AS (
			SELECT pi.person_slug,
				COUNT(*) AS requests,
				COALESCE(SUM(e.total_tokens),0) AS total_tokens,
				COALESCE(ROUND(SUM(%s)::numeric,2),0) AS cost_usd,
				MAX(e.timestamp) AS last_used
			FROM subtree st
			JOIN person_identities pi ON pi.person_slug = st.slug AND pi.is_service = false
			JOIN usage_events e ON e.username = pi.username
			LEFT JOIN model_pricing p ON e.model = p.model
			WHERE e.timestamp >= $2 AND e.timestamp < $3
			GROUP BY pi.person_slug
		)
		SELECT st.slug, p.full_name, COALESCE(pi.username, ''), COALESCE(p.manager_slug,''),
			EXISTS (SELECT 1 FROM people r WHERE r.manager_slug = p.slug),
			COALESCE(u.requests,0), COALESCE(u.total_tokens,0), COALESCE(u.cost_usd,0),
			to_char(u.last_used, 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
		FROM subtree st
		JOIN people p ON p.slug = st.slug
		LEFT JOIN LATERAL (SELECT username FROM person_identities WHERE person_slug = st.slug AND is_service = false ORDER BY username LIMIT 1) pi ON true
		LEFT JOIN u ON u.person_slug = st.slug
		ORDER BY COALESCE(u.cost_usd,0) DESC, p.full_name`, costUSDExpr)
	rows, err := s.db.QueryContext(ctx, query, rootSlug, since, until)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OrgUsageRow
	for rows.Next() {
		var r OrgUsageRow
		var lastUsed sql.NullString
		if err := rows.Scan(&r.Slug, &r.FullName, &r.Username, &r.ManagerSlug,
			&r.IsManager, &r.Requests, &r.TotalTokens, &r.CostUSD, &lastUsed); err != nil {
			return nil, err
		}
		if lastUsed.Valid {
			r.LastUsed = &lastUsed.String
		}
		r.NoIdentity = r.Username == ""
		out = append(out, r)
	}
	return out, rows.Err()
}

// --- Key invites ---

// CreateInvite mints a single-use invite. The token is returned exactly
// once (in the response the admin copies into the invite); the database
// keeps only its SHA-256, so this table leaks no usable secrets.
func (s *Store) CreateInvite(ctx context.Context, personSlug, group, keyName, actor string, ttl time.Duration) (token string, inviteID int64, err error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", 0, err
	}
	token = hex.EncodeToString(buf)
	sum := sha256.Sum256([]byte(token))
	var id int64
	err = s.db.QueryRowContext(ctx, `
		INSERT INTO key_invites (token_hash, person_slug, group_name, key_name, created_by, expires_at)
		VALUES ($1, $2, $3, $4, $5, NOW() + $6::interval) RETURNING id`,
		hex.EncodeToString(sum[:]), personSlug, group, keyName, actor,
		fmt.Sprintf("%d seconds", int(ttl.Seconds()))).Scan(&id)
	if err != nil {
		return "", 0, err
	}
	_ = s.Audit(ctx, actor, "key.invite_create", personSlug, map[string]any{"invite_id": id})
	return token, id, nil
}

// Invite is the view-safe projection (never carries the token).
type Invite struct {
	ID         int64     `json:"id"`
	PersonSlug string    `json:"person_slug"`
	PersonName string    `json:"person_name"`
	GroupName  string    `json:"group_name"`
	KeyName    string    `json:"key_name"`
	CreatedBy  string    `json:"created_by"`
	CreatedAt  time.Time `json:"created_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	ClaimedAt  *string   `json:"claimed_at,omitempty"`
	KeyID      string    `json:"key_id,omitempty"`
	RevokedAt  *string   `json:"revoked_at,omitempty"`
	Status     string    `json:"status"`
}

func (s *Store) ListInvites(ctx context.Context) ([]Invite, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT i.id, i.person_slug, p.full_name, i.group_name, i.key_name, i.created_by,
			i.created_at, i.expires_at, to_char(i.claimed_at, 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
			COALESCE(i.key_id,''), to_char(i.revoked_at, 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
		FROM key_invites i JOIN people p ON p.slug = i.person_slug
		ORDER BY i.id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Invite
	for rows.Next() {
		var iv Invite
		var claimed, revoked sql.NullString
		if err := rows.Scan(&iv.ID, &iv.PersonSlug, &iv.PersonName, &iv.GroupName, &iv.KeyName,
			&iv.CreatedBy, &iv.CreatedAt, &iv.ExpiresAt, &claimed, &iv.KeyID, &revoked); err != nil {
			return nil, err
		}
		if claimed.Valid {
			iv.ClaimedAt = &claimed.String
		}
		if revoked.Valid {
			iv.RevokedAt = &revoked.String
		}
		switch {
		case revoked.Valid:
			iv.Status = "revoked"
		case claimed.Valid:
			iv.Status = "claimed"
		case iv.ExpiresAt.Before(time.Now()):
			iv.Status = "expired"
		default:
			iv.Status = "pending"
		}
		out = append(out, iv)
	}
	return out, rows.Err()
}

// InviteByTokenHash resolves an invite for the claim preview (GET /invite/
// {token}) WITHOUT consuming it. The raw token is hashed here and matched
// against the stored hash, so the comparison never exposes the plaintext
// token in a query parameter beyond the request itself.
func (s *Store) InviteByTokenHash(ctx context.Context, token string) (Invite, error) {
	sum := sha256.Sum256([]byte(token))
	hash := hex.EncodeToString(sum[:])
	var iv Invite
	var claimed, revoked sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT i.id, i.person_slug, p.full_name, i.group_name, i.key_name, i.created_by,
			i.created_at, i.expires_at, to_char(i.claimed_at, 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
			COALESCE(i.key_id,''), to_char(i.revoked_at, 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
		FROM key_invites i JOIN people p ON p.slug = i.person_slug
		WHERE i.token_hash = $1`,
		hash).Scan(&iv.ID, &iv.PersonSlug, &iv.PersonName, &iv.GroupName, &iv.KeyName,
		&iv.CreatedBy, &iv.CreatedAt, &iv.ExpiresAt, &claimed, &iv.KeyID, &revoked)
	if err != nil {
		return Invite{}, err
	}
	switch {
	case revoked.Valid:
		iv.Status = "revoked"
	case claimed.Valid:
		iv.Status = "claimed"
	case iv.ExpiresAt.Before(time.Now()):
		iv.Status = "expired"
	default:
		iv.Status = "pending"
	}
	return iv, nil
}

// ClaimInvite validates and consumes an invite token atomically. On success
// the caller (handler) mints the key via maas-api and records its id; the
// token can never be redeemed twice, claimed or not.
func (s *Store) ClaimInvite(ctx context.Context, token string) (inviteID int64, personSlug, group, keyName string, err error) {
	sum := sha256.Sum256([]byte(token))
	hash := hex.EncodeToString(sum[:])
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, "", "", "", err
	}
	defer tx.Rollback() //nolint:errcheck
	err = tx.QueryRowContext(ctx, `
		UPDATE key_invites SET claimed_at = NOW()
		WHERE token_hash = $1 AND claimed_at IS NULL AND revoked_at IS NULL AND expires_at > NOW()
		RETURNING id, person_slug, group_name, key_name`,
		hash).Scan(&inviteID, &personSlug, &group, &keyName)
	if err != nil {
		if err == sql.ErrNoRows {
			return 0, "", "", "", fmt.Errorf("invite is invalid, already claimed, revoked, or expired")
		}
		return 0, "", "", "", err
	}
	if err := tx.Commit(); err != nil {
		return 0, "", "", "", err
	}
	return inviteID, personSlug, group, keyName, nil
}

func (s *Store) SetInviteKey(ctx context.Context, inviteID int64, keyID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE key_invites SET key_id = $2 WHERE id = $1`, inviteID, keyID)
	return err
}

func (s *Store) RevokeInvite(ctx context.Context, inviteID int64, actor string) error {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE key_invites SET revoked_at = NOW() WHERE id = $1 AND claimed_at IS NULL`, inviteID); err != nil {
		return err
	}
	return s.Audit(ctx, actor, "key.invite_revoke", fmt.Sprint(inviteID), nil)
}

// --- misc helpers ---

// sqlAnyString maps nil/"" to NULL (Postgres UNIQUE treats NULLs as
// distinct, so 112 people without emails never collide).
func sqlAnyString(s *string) any {
	if s == nil || *s == "" {
		return nil
	}
	return *s
}

// pgArray renders a []string as a Postgres array literal for a text[]
// parameter (lib/pq has no []string driver by default).
func pgArray(vals []string) string {
	if len(vals) == 0 {
		return "{}"
	}
	escaped := make([]string, 0, len(vals))
	for _, v := range vals {
		v = strings.ReplaceAll(v, `\`, `\\`)
		v = strings.ReplaceAll(v, `"`, `\"`)
		escaped = append(escaped, `"`+v+`"`)
	}
	return "{" + strings.Join(escaped, ",") + "}"
}
