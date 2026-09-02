package handler

import (
	"encoding/json"
	"net/http"

	"github.com/noyitz/ai-gateway-metering-service/internal/storage"
)

// ProfilesHandler maintains the human display names (user_profiles table)
// shown in the dashboards. Mounted behind RequireAdmin; the dashboard pages
// themselves read the names through the regular /api/v1/dashboard/* and
// /me/whoami endpoints, so only this admin surface writes them.
type ProfilesHandler struct {
	store *storage.Store
}

func NewProfilesHandler(store *storage.Store) *ProfilesHandler {
	return &ProfilesHandler{store: store}
}

// HandleProfiles serves both directions of the admin name editor:
//
//	GET  → every profile, username-ordered
//	POST → bulk upsert; body is [{"username","first_name","last_name"}, ...]
func (h *ProfilesHandler) HandleProfiles(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		profiles, err := h.store.ListUserProfiles(r.Context())
		if err != nil {
			http.Error(w, "failed to list user profiles: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if profiles == nil {
			profiles = []storage.UserProfile{}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(profiles) //nolint:errcheck // response already committed

	case http.MethodPost:
		var in []storage.UserProfile
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, "body must be a JSON array of {username, first_name, last_name}: "+err.Error(), http.StatusBadRequest)
			return
		}
		profiles := make([]storage.UserProfile, 0, len(in))
		for _, p := range in {
			if p.Username == "" {
				http.Error(w, "every profile needs a username", http.StatusBadRequest)
				return
			}
			profiles = append(profiles, storage.UserProfile{
				Username:  p.Username,
				FirstName: p.FirstName,
				LastName:  p.LastName,
			})
		}
		updated, err := h.store.UpsertUserProfiles(r.Context(), profiles)
		if err != nil {
			http.Error(w, "failed to save user profiles: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]int{"updated": updated}) //nolint:errcheck // response already committed

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
