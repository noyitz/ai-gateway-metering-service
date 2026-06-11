package handler

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/noyitz/ai-gateway-metering-service/internal/storage"
)

type cloudEvent struct {
	SpecVersion     string         `json:"specversion"`
	ID              string         `json:"id"`
	Source          string         `json:"source"`
	Type            string         `json:"type"`
	Subject         string         `json:"subject"`
	Time            string         `json:"time"`
	DataContentType string         `json:"datacontenttype"`
	Data            cloudEventData `json:"data"`
}

type cloudEventData struct {
	User             string `json:"user"`
	Group            string `json:"group"`
	Subscription     string `json:"subscription"`
	Provider         string `json:"provider"`
	Model            string `json:"model"`
	PromptTokens       int `json:"prompt_tokens"`
	CompletionTokens   int `json:"completion_tokens"`
	TotalTokens        int `json:"total_tokens"`
	CachedInputTokens  int `json:"cached_input_tokens"`
	CacheCreationTokens int `json:"cache_creation_tokens"`
	ReasoningTokens    int `json:"reasoning_tokens"`
}

type EventsHandler struct {
	store *storage.Store
}

func NewEventsHandler(store *storage.Store) *EventsHandler {
	return &EventsHandler{store: store}
}

func (h *EventsHandler) HandleEvent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var event cloudEvent
	if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
		slog.Error("failed to decode event", "error", err)
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	ts, err := time.Parse(time.RFC3339, event.Time)
	if err != nil {
		ts = time.Now()
	}

	total := event.Data.TotalTokens
	if total == 0 {
		total = event.Data.PromptTokens + event.Data.CompletionTokens
	}

	usageEvent := storage.UsageEvent{
		EventID:             event.ID,
		Timestamp:           ts,
		Username:            event.Data.User,
		GroupName:           event.Data.Group,
		Subscription:        event.Data.Subscription,
		Provider:            event.Data.Provider,
		Model:               event.Data.Model,
		PromptTokens:        event.Data.PromptTokens,
		CompletionTokens:    event.Data.CompletionTokens,
		TotalTokens:         total,
		CachedInputTokens:   event.Data.CachedInputTokens,
		CacheCreationTokens: event.Data.CacheCreationTokens,
		ReasoningTokens:     event.Data.ReasoningTokens,
		Source:              event.Source,
	}

	if err := h.store.InsertEvent(r.Context(), usageEvent); err != nil {
		slog.Error("failed to insert event", "error", err, "event_id", event.ID)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	slog.Info("event recorded", "user", event.Data.User, "model", event.Data.Model, "tokens", total)
	w.WriteHeader(http.StatusNoContent)
}
