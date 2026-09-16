package config

import (
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
