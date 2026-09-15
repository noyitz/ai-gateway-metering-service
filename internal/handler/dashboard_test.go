package handler

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestParseTimeWindow(t *testing.T) {
	tests := []struct {
		name      string
		query     string
		wantMin   time.Duration // approximate window size checked against tolerance
		tolerance time.Duration
	}{
		{"24h", "range=24h", 24 * time.Hour, time.Minute},
		{"30d", "range=30d", 30 * 24 * time.Hour, time.Minute},
		{"default is 7d", "", 7 * 24 * time.Hour, time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/api/v1/dashboard/overview?"+tt.query, nil)
			since, until := parseTimeWindow(r)
			got := until.Sub(since)
			if diff := got - tt.wantMin; diff > tt.tolerance || diff < -tt.tolerance {
				t.Errorf("window = %v, want ~%v", got, tt.wantMin)
			}
		})
	}

	t.Run("mtd starts at first of month midnight", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/api/v1/dashboard/overview?range=mtd", nil)
		since, until := parseTimeWindow(r)
		now := time.Now()
		y, m, _ := now.Date()
		wantSince := time.Date(y, m, 1, 0, 0, 0, 0, now.Location())
		if !since.Equal(wantSince) {
			t.Errorf("since = %v, want %v", since, wantSince)
		}
		if until.Sub(now) < -time.Minute || until.Sub(now) > time.Minute {
			t.Errorf("until = %v, want ~now", until)
		}
	})

	t.Run("mtd is never wider than a month and never empty", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/api/v1/dashboard/overview?range=mtd", nil)
		since, until := parseTimeWindow(r)
		if w := until.Sub(since); w <= 0 || w > 31*24*time.Hour {
			t.Errorf("mtd window = %v, want 0 < w <= 31d", w)
		}
	})
}
