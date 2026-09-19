package storage

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"

	_ "github.com/lib/pq"
)

// TestWritePathSQLParsesAgainstLiveSchema is a deploy pre-flight, not a
// unit test: it PREPAREs every write-path statement this package emits
// (parse + analyze against the real live schema, NOTHING executes) so a
// typo, a missing column or an alias drift is caught before the image
// goes anywhere near an ingest path. Run against prod via port-forward:
//
//	oc port-forward svc/aigateway-pg-rw 15432:5432 &
//	PREFLIGHT_DSN="postgres://<user>:<pass>@localhost:15432/<db>?sslmode=disable" \
//	  go test ./internal/storage -run TestWritePathSQLParsesAgainstLiveSchema
//
// Skipped (not failed) without PREFLIGHT_DSN so CI stays hermetic.
func TestWritePathSQLParsesAgainstLiveSchema(t *testing.T) {
	dsn := os.Getenv("PREFLIGHT_DSN")
	if dsn == "" {
		t.Skip("PREFLIGHT_DSN unset — live-schema pre-flight only runs on demand")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	defer conn.Close()

	// The parity rollup-side queries live inline in ParityReport; the
	// write-path statements below are the ones that can break ingest, so
	// those are the pre-flight contract. Parity is read-only and gets
	// exercised by its own endpoint after backfill.
	stmts := map[string]string{
		"insert_event":         insertEventSQL,
		"upsert_rollup":        upsertRollupSQL,
		"hour_lock_up":         upsertHourLockSQL,
		"hour_lock_rng":        rebuildHourLockSQL,
		"cost_backfill":        costBackfillSQL,
		"rebuild_hour":         rebuildHourSQL,
		"meta_get":             `SELECT value FROM rollup_meta WHERE key = $1`,
		"meta_set":             `INSERT INTO rollup_meta (key, value) VALUES ($1, $2) ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`,
		"rollup_overview":      rollupOverviewSQL,
		"rollup_groups":        rollupGroupsSQL,
		"rollup_team":          rollupTeamUsageSQL,
		"rollup_models":        rollupModelsSQL,
		"rollup_timeline":      rollupTimelineSQL("day", "e.model"),
		"rollup_timeline_hour": rollupTimelineSQL("hour", "e.username"),
		"rollup_users": hostedSavingsWithSQL(7, true) +
			fmt.Sprintf(rollupUsersSelect, displayNameExpr, displayNameExpr, "total_tokens", "DESC"),
		"rollup_savings": hostedSavingsWithSQL(6, true) + `,
		sa AS (SELECT COALESCE(SUM(saved), 0) as saved FROM sv)
		SELECT COALESCE(ROUND((SELECT saved FROM sa)::numeric, 2), 0)::float8, (SELECT r FROM rat)`,
		"denial_insert": `
		INSERT INTO usage_events (event_id, username, model, provider, group_name, status_code, source, cost_usd)
		VALUES ('deny-' || gen_random_uuid()::text, $1, $2, 'gateway',
			(SELECT p.group_name FROM person_identities pi
			 JOIN people p ON p.slug = pi.person_slug
			 WHERE pi.username = $1 LIMIT 1),
			429, 'metering-quota', 0)
		RETURNING timestamp, group_name`,
	}
	for name, q := range stmts {
		// $N placeholders must be typed for PREPARE inference where the
		// Go call sites cast them anyway; if inference complains we want
		// to know — it means the app statement itself would fail.
		if _, err := conn.ExecContext(ctx, "PREPARE preflight_"+name+" AS "+q); err != nil {
			t.Errorf("PREPARE %s: %v", name, err)
			continue
		}
		if _, err := conn.ExecContext(ctx, "DEALLOCATE preflight_"+name); err != nil {
			t.Errorf("DEALLOCATE %s: %v", name, err)
		}
	}
}
