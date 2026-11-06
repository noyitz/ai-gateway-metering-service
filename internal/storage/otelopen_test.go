package storage

import (
	"testing"
)

func TestOpenDB_SelectsDriver(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	db, err := openDB("postgres://u:p@127.0.0.1:1/db?sslmode=disable")
	if err != nil {
		t.Fatalf("disabled open: %v", err)
	}
	_ = db.Close()

	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:4318")
	db, err = openDB("postgres://u:p@127.0.0.1:1/db?sslmode=disable")
	if err != nil {
		t.Fatalf("enabled open: %v", err)
	}
	_ = db.Close()
}
