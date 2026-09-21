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

func TestCloudEventsMaxBytes(t *testing.T) {
	t.Setenv("CLOUDEVENTS_MAX_BYTES", "32768")
	if got := Load().CloudEvents.MaxBytes; got != 32768 {
		t.Fatalf("CloudEventsMaxBytes = %d, want 32768", got)
	}

	for _, raw := range []string{"0", "-1", "not-a-number", "1048577"} {
		t.Setenv("CLOUDEVENTS_MAX_BYTES", raw)
		if got := Load().CloudEvents.MaxBytes; got != DefaultCloudEventsMaxBytes {
			t.Errorf("CLOUDEVENTS_MAX_BYTES=%q: got %d, want default %d", raw, got, DefaultCloudEventsMaxBytes)
		}
	}
}

func TestCloudEventsAuthToken(t *testing.T) {
	t.Setenv("CLOUDEVENTS_AUTH_TOKEN", "secret")
	if got := Load().CloudEvents.AuthToken; got != "secret" {
		t.Fatalf("CloudEventsAuthToken = %q, want secret", got)
	}
}

func TestCloudEventsRequiresExplicitUnauthenticatedAgreement(t *testing.T) {
	cfg := CloudEvents{}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected unauthenticated CloudEvents configuration to fail")
	}
	cfg.AllowUnauthenticated = true
	if err := cfg.Validate(); err != nil {
		t.Fatalf("explicit development mode rejected: %v", err)
	}
}
