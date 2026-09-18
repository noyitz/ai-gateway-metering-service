package handler

import "testing"

func TestParseTimeWindowNamed(t *testing.T) {
	tests := []struct {
		in    string
		hours float64
		ok    bool
	}{
		{"24h", 24, true}, {"7d", 168, true}, {"30d", 720, true}, {"90d", 2160, true},
		{"365d", 0, false}, {"", 0, false}, {"7D", 0, false},
	}
	for _, tt := range tests {
		got, ok := parseTimeWindowNamed(tt.in)
		if ok != tt.ok || got.Hours() != tt.hours {
			t.Errorf("parseTimeWindowNamed(%q) = (%v,%v), want (%vh,%v)", tt.in, got, ok, tt.hours, tt.ok)
		}
	}
}
