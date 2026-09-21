package storage

import (
	"testing"
	"time"
)

func TestInsertEventIsIdempotent(t *testing.T) {
	store, ctx := openTestStore(t)
	defer store.Close()

	event := UsageEvent{
		EventID:          "duplicate-event",
		Timestamp:        time.Now().UTC(),
		Username:         "alice",
		Source:           "urn:praxis:test",
		Model:            "gpt-4o",
		PromptTokens:     10,
		CompletionTokens: 5,
		TotalTokens:      15,
	}

	inserted, err := store.InsertEvent(ctx, event)
	if err != nil || !inserted {
		t.Fatalf("first insert: inserted=%v err=%v", inserted, err)
	}
	inserted, err = store.InsertEvent(ctx, event)
	if err != nil || inserted {
		t.Fatalf("duplicate insert: inserted=%v err=%v", inserted, err)
	}

	var events, rollups int
	if err := store.db.QueryRow(`SELECT count(*) FROM usage_events WHERE source = $1 AND event_id = $2`, event.Source, event.EventID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT count(*) FROM usage_hourly WHERE username = $1 AND model = $2`, event.Username, event.Model).Scan(&rollups); err != nil {
		t.Fatal(err)
	}
	if events != 1 || rollups != 1 {
		t.Fatalf("duplicate changed ledger: events=%d rollups=%d", events, rollups)
	}
}
