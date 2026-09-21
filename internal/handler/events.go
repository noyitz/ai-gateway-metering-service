package handler

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/noyitz/ai-gateway-metering-service/internal/config"
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
	User                string `json:"user"`
	Group               string `json:"group"`
	Subscription        string `json:"subscription"`
	Provider            string `json:"provider"`
	Model               string `json:"model"`
	PromptTokens        int    `json:"prompt_tokens"`
	CompletionTokens    int    `json:"completion_tokens"`
	TotalTokens         int    `json:"total_tokens"`
	CachedInputTokens   int    `json:"cached_input_tokens"`
	CacheCreationTokens int    `json:"cache_creation_tokens"`
	ReasoningTokens     int    `json:"reasoning_tokens"`
	UserAgent           string `json:"user_agent"`
	// StatusCode is the upstream HTTP status. The gateway sets it only on
	// error events (inference.request.error); success/usage events omit it,
	// so a nil pointer means the request succeeded (HTTP 200).
	StatusCode *int `json:"status_code"`
}

type EventsHandler struct {
	store     *storage.Store
	maxBytes  int
	authToken string
}

func NewEventsHandler(store *storage.Store, cfg config.CloudEvents) *EventsHandler {
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = config.DefaultCloudEventsMaxBytes
	}
	return &EventsHandler{store: store, maxBytes: cfg.MaxBytes, authToken: cfg.AuthToken}
}

func (h *EventsHandler) HandleEvent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.authToken != "" && !validCloudEventAuth(r, h.authToken) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="cloud-events"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || !strings.EqualFold(mediaType, "application/cloudevents+json") {
		http.Error(w, "content type must be application/cloudevents+json", http.StatusUnsupportedMediaType)
		return
	}

	var event cloudEvent
	maxBytes := h.maxBytes
	if maxBytes <= 0 {
		maxBytes = config.DefaultCloudEventsMaxBytes
	}
	r.Body = http.MaxBytesReader(w, r.Body, int64(maxBytes))
	if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
		slog.Error("failed to decode event", "error", err)
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if err := validateCloudEvent(event); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
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

	// HTTP status: the gateway carries status_code only on error events
	// (which is why those rows show 0/0/0/0 tokens). A success/usage event
	// has no status_code, so it is a 200. Store a concrete value on every
	// new row; historical rows predating this column stay NULL (unknown).
	status := http.StatusOK
	if event.Data.StatusCode != nil {
		status = *event.Data.StatusCode
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
		UserAgent:           event.Data.UserAgent,
		StatusCode:          &status,
	}

	if h.store == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	inserted, err := h.store.InsertEvent(r.Context(), usageEvent)
	if err != nil {
		slog.Error("failed to insert event", "error", err, "event_id", event.ID)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if !inserted {
		slog.Info("event duplicate ignored", "event_id", event.ID, "source", event.Source, "reason", "duplicate")
	} else {
		slog.Info("event processed", "user", event.Data.User, "model", event.Data.Model, "tokens", total)
	}
	w.WriteHeader(http.StatusNoContent)
}

func validCloudEventAuth(r *http.Request, expected string) bool {
	const prefix = "Bearer "
	value := r.Header.Get("Authorization")
	if len(value) <= len(prefix) || !strings.EqualFold(value[:len(prefix)], prefix) {
		return false
	}
	token := strings.TrimSpace(value[len(prefix):])
	return len(token) == len(expected) && subtle.ConstantTimeCompare([]byte(token), []byte(expected)) == 1
}

func validateCloudEvent(event cloudEvent) error {
	if event.SpecVersion != "1.0" || event.ID == "" || event.Source == "" || event.Type == "" {
		return errors.New("missing or invalid CloudEvents fields: specversion, id, source, type")
	}
	if _, err := url.Parse(event.Source); err != nil {
		return errors.New("source must be a valid URI-reference")
	}
	if event.Time != "" {
		if _, err := time.Parse(time.RFC3339, event.Time); err != nil {
			return errors.New("time must be RFC3339")
		}
	}
	if event.Data.User == "" || event.Data.Model == "" {
		return errors.New("missing required metering fields: data.user, data.model")
	}
	return nil
}
