package maasapi

import (
	"context"
	"strings"
	"testing"
)

// maas-api's auth middleware 500s (AUTH_FAILURE refId 002) on any v1 call
// missing a non-empty X-MaaS-Group header. The client must refuse those
// calls locally, naming the real problem, instead of sending a request that
// fails with a confusing auth error.
func TestClientRefusesGrouplessCalls(t *testing.T) {
	c := NewClient("http://127.0.0.1:1", "test-tenant") // never dialed: guard must fire first
	tests := []struct {
		name string
		run  func() error
	}{
		{"search nil groups", func() error { _, err := c.SearchAPIKeys(context.Background(), "u@x.com", nil); return err }},
		{"search empty groups", func() error { _, err := c.SearchAPIKeys(context.Background(), "u@x.com", []string{}); return err }},
		{"revoke nil groups", func() error { return c.RevokeAPIKey(context.Background(), "key-1", nil) }},
		{"revoke empty groups", func() error { return c.RevokeAPIKey(context.Background(), "key-1", []string{}) }},
	}
	for _, tc := range tests {
		err := tc.run()
		if err == nil {
			t.Fatalf("%s: want local refusal, got nil error", tc.name)
		}
		if !strings.Contains(err.Error(), "no groups") {
			t.Fatalf("%s: error %q should name the missing groups, not leak a transport/auth error", tc.name, err)
		}
	}
}
