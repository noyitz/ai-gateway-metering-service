package config

import (
	"os"
	"reflect"
	"testing"
)

// envList backs ADMIN_USERS / SUPERADMIN_USERS, which gate the admin and
// super-admin surfaces. The deployment sets one comma-separated and one
// space-separated, so both forms (and a mix) must parse into individual
// identities — a whole list collapsed into one entry silently denies access.
func TestEnvList_Separators(t *testing.T) {
	cases := map[string][]string{
		"a@x.com,b@x.com":            {"a@x.com", "b@x.com"},
		"a@x.com b@x.com":            {"a@x.com", "b@x.com"},
		"a@x.com, b@x.com   c@x.com": {"a@x.com", "b@x.com", "c@x.com"},
		"  ":                         nil,
		"":                           nil,
	}
	for raw, want := range cases {
		t.Setenv("TEST_ENV_LIST", raw)
		if got := envList("TEST_ENV_LIST"); !reflect.DeepEqual(got, want) {
			t.Errorf("envList(%q) = %v, want %v", raw, got, want)
		}
	}
}

// The read switch follows the cache-flag rule (PR #19 review, part B
// condition 1): deploying a build and enabling behavior are separate
// decisions, so the defaults must be OFF / 300s regardless of how the
// test environment is configured.
func TestRollupReadSwitchDefaults(t *testing.T) {
	os.Unsetenv("DASHBOARD_USE_ROLLUPS")
	os.Unsetenv("ROLLUP_REFRESH_SECONDS")
	cfg := Load()
	if cfg.DashboardUseRollups {
		t.Error("DashboardUseRollups must default OFF — rollup reads are enabled per deployment, not by shipping the code")
	}
	if cfg.RollupRefreshSeconds != 300 {
		t.Errorf("RollupRefreshSeconds default = %d, want 300 (bounds refresh and parity-check latency)", cfg.RollupRefreshSeconds)
	}
}
