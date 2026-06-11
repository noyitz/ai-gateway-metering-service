package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandleEvent_ParsesCloudEvent(t *testing.T) {
	event := cloudEvent{
		SpecVersion:     "1.0",
		ID:              "evt-test-123",
		Source:          "maas-gateway",
		Type:            "inference.tokens.used",
		Subject:         "testuser",
		Time:            "2026-05-28T10:30:00Z",
		DataContentType: "application/json",
		Data: cloudEventData{
			User:             "testuser",
			Group:            "engineering",
			Subscription:     "premium",
			Provider:         "anthropic",
			Model:            "claude-opus-4-6",
			PromptTokens:     100,
			CompletionTokens: 50,
			TotalTokens:      150,
		},
	}

	body, _ := json.Marshal(event)

	var parsed cloudEvent
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("failed to parse: %v", err)
	}

	if parsed.Data.User != "testuser" {
		t.Errorf("user: got %q, want %q", parsed.Data.User, "testuser")
	}
	if parsed.Data.PromptTokens != 100 {
		t.Errorf("prompt_tokens: got %d, want %d", parsed.Data.PromptTokens, 100)
	}
	if parsed.Data.Provider != "anthropic" {
		t.Errorf("provider: got %q, want %q", parsed.Data.Provider, "anthropic")
	}
	if parsed.ID != "evt-test-123" {
		t.Errorf("id: got %q, want %q", parsed.ID, "evt-test-123")
	}
}

func TestHandleEvent_RejectsGet(t *testing.T) {
	h := &EventsHandler{}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil)
	w := httptest.NewRecorder()
	h.HandleEvent(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status: got %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}

func TestHandleEvent_RejectsInvalidJSON(t *testing.T) {
	h := &EventsHandler{}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/events", bytes.NewReader([]byte("not json")))
	w := httptest.NewRecorder()
	h.HandleEvent(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestHandleEvent_AcceptsValidEvent_NoStore(t *testing.T) {
	h := &EventsHandler{store: nil}
	event := cloudEvent{
		SpecVersion: "1.0",
		ID:          "evt-001",
		Source:      "test",
		Type:        "inference.tokens.used",
		Subject:     "user1",
		Data: cloudEventData{
			User:         "user1",
			Model:        "gpt-4o",
			PromptTokens: 10,
		},
	}
	body, _ := json.Marshal(event)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/events", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.HandleEvent(w, req)

	// Without a store, handler should parse successfully but fail on insert (or return 204 if store is nil)
	// This tests the parsing path at minimum
	if w.Code != http.StatusNoContent && w.Code != http.StatusInternalServerError {
		t.Errorf("status: got %d, want 204 or 500", w.Code)
	}
}
