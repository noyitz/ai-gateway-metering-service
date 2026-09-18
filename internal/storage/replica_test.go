package storage

import (
	"database/sql"
	"os"
	"strings"
	"testing"

	_ "github.com/lib/pq"
)

func openFakePool(t *testing.T, name string) *sql.DB {
	t.Helper()
	// sql.Open is lazy — no connection is attempted, so this is a valid
	// *sql.DB pointer for identity checks only.
	db, err := sql.Open("postgres", "user=x host=invalid.invalid dbname=x")
	if err != nil {
		t.Fatalf("open %s pool: %v", name, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestReaderSelection(t *testing.T) {
	primary := openFakePool(t, "primary")

	// Unset replica: every read uses the primary.
	s := &Store{db: primary}
	if got := s.reader(); got != primary {
		t.Fatal("reader() with no replica must return the primary")
	}

	replica := openFakePool(t, "replica")
	s.readDB = replica
	if got := s.reader(); got != replica {
		t.Fatal("reader() with replica set must return the replica")
	}
}

func TestUseReadReplicaRejectsUnreachable(t *testing.T) {
	s := &Store{db: openFakePool(t, "primary")}
	// A refused TCP connection fails the ping promptly.
	err := s.UseReadReplica("postgres://u:p@127.0.0.1:1/nope?sslmode=disable")
	if err == nil {
		t.Fatal("expected ping failure for unreachable replica DSN")
	}
	if !strings.Contains(err.Error(), "ping read replica") {
		t.Errorf("error = %v, want it to name the ping step", err)
	}
	if s.readDB != nil {
		t.Fatal("failed UseReadReplica must leave the store on the primary")
	}
}

// TestMoneyReadsStayOnPrimary guards the Phase 2 allowlist at the source
// level: enforcement/cost-decision reads must never run on the replica.
// If someone later flips one of these to s.reader(), CI catches it here
// instead of production discovering spend-lag 429s.
func TestMoneyReadsStayOnPrimary(t *testing.T) {
	src, err := os.ReadFile("postgres.go")
	if err != nil {
		t.Fatalf("read postgres.go: %v", err)
	}
	text := string(src)

	pinned := []string{
		"func (s *Store) GetRecentEvents",
		"func (s *Store) GetMonthlyUsage",
		"func (s *Store) InsertEvent",
	}
	for _, decl := range pinned {
		start := strings.Index(text, decl)
		if start < 0 {
			t.Fatalf("symbol %q not found — did it move? update this guard", decl)
		}
		end := strings.Index(text[start+1:], "\nfunc ")
		if end < 0 {
			end = len(text)
		} else {
			end += start + 1
		}
		body := text[start:end]
		if strings.Contains(body, "s.reader()") {
			t.Errorf("%s uses s.reader() — primary-pinned read moved to the replica", decl)
		}
	}

	// And the six allowlisted dashboard reads must actually use it —
	// otherwise Phase 2 is silently a no-op.
	offloaded := []string{
		"func (s *Store) GetDashboardOverview",
		"func (s *Store) GetDashboardGroups",
		"func (s *Store) GetDashboardUsers",
		"func (s *Store) GetHostedSavings",
		"func (s *Store) GetDashboardModels",
		"func (s *Store) GetDashboardTimeline",
	}
	for _, decl := range offloaded {
		start := strings.Index(text, decl)
		if start < 0 {
			t.Fatalf("symbol %q not found — did it move? update this guard", decl)
		}
		end := strings.Index(text[start+1:], "\nfunc ")
		if end < 0 {
			end = len(text)
		} else {
			end += start + 1
		}
		if !strings.Contains(text[start:end], "s.reader()") {
			t.Errorf("%s does not use s.reader() — it is missing from the replica allowlist", decl)
		}
	}
}
