package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRequireInternalAPI(t *testing.T) {
	next := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }
	h := RequireInternalAPI("secret", next)

	tests := []struct {
		name   string
		header string
		want   int
	}{
		{name: "missing", want: http.StatusUnauthorized},
		{name: "wrong", header: "wrong", want: http.StatusUnauthorized},
		{name: "valid", header: "secret", want: http.StatusNoContent},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/internal", nil)
			if tt.header != "" {
				req.Header.Set(internalTokenHeader, tt.header)
			}
			resp := httptest.NewRecorder()
			h(resp, req)
			if resp.Code != tt.want {
				t.Fatalf("status = %d, want %d", resp.Code, tt.want)
			}
		})
	}
}
