package storage

import "testing"

func TestComputeUsageStats(t *testing.T) {
	tests := []struct {
		name       string
		used       int64
		tokenQuota int64
		want       UsageStats
	}{
		{
			name:       "unlimited quota reports usage but never gates",
			used:       100_000_000,
			tokenQuota: 0,
			want:       UsageStats{HasAccess: true, Usage: 100_000_000},
		},
		{
			name:       "negative quota treated as unlimited",
			used:       500,
			tokenQuota: -1,
			want:       UsageStats{HasAccess: true, Usage: 500},
		},
		{
			name:       "under quota has access with remaining balance",
			used:       1_000,
			tokenQuota: 10_000,
			want:       UsageStats{HasAccess: true, Balance: 9_000, Usage: 1_000, Overage: 0},
		},
		{
			name:       "at quota is denied",
			used:       10_000,
			tokenQuota: 10_000,
			want:       UsageStats{HasAccess: false, Balance: 0, Usage: 10_000, Overage: 0},
		},
		{
			name:       "over quota is denied and reports overage",
			used:       12_000,
			tokenQuota: 10_000,
			want:       UsageStats{HasAccess: false, Balance: 0, Usage: 12_000, Overage: 2_000},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := computeUsageStats(tt.used, tt.tokenQuota)
			if got != tt.want {
				t.Errorf("computeUsageStats(%d, %d) = %+v, want %+v",
					tt.used, tt.tokenQuota, got, tt.want)
			}
		})
	}
}
