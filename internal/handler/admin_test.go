package handler

import (
	"context"
	"strings"
	"testing"

	"github.com/noyitz/ai-gateway-metering-service/internal/config"
)

// platformGroups is the group scope for every maas-api key call. A missing
// k8s client must surface as an error (the handlers degrade to an empty
// list / 503 with a log line) — it must NOT yield an empty slice, because
// the client's guard then rejects the call with a message about groups that
// would point at the wrong layer during an incident.
func TestPlatformGroupsNoKubernetes(t *testing.T) {
	h := NewAdminHandler(nil, nil, config.Config{})
	_, err := h.platformGroups(context.Background())
	if err == nil || !strings.Contains(err.Error(), "kubernetes") {
		t.Fatalf("want kubernetes-not-configured error, got %v", err)
	}
}
