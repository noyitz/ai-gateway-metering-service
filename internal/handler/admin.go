package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"sort"
	"strings"

	"github.com/noyitz/ai-gateway-metering-service/internal/config"
	"github.com/noyitz/ai-gateway-metering-service/internal/dashboard"
	"github.com/noyitz/ai-gateway-metering-service/internal/k8s"
	"github.com/noyitz/ai-gateway-metering-service/internal/maasapi"
)

type AdminHandler struct {
	k8sClient  *k8s.Client
	maasClient *maasapi.Client
	cfg        config.Config
}

func NewAdminHandler(k8sClient *k8s.Client, maasClient *maasapi.Client, cfg config.Config) *AdminHandler {
	return &AdminHandler{k8sClient: k8sClient, maasClient: maasClient, cfg: cfg}
}

// IsAdmin reports whether the caller may see the org-wide Usage view — the
// full user table and user filter on the dashboard. Identity comes from the
// header an authenticating proxy sets in front of this service; the service
// performs no authentication of its own. Admins do NOT get the admin console
// or the Routing/Compression pages — see IsSuperAdmin for those.
func IsAdmin(cfg config.Config, r *http.Request) bool {
	user := r.Header.Get(cfg.UserHeader)
	if user == "" {
		return cfg.AllowUnauthenticatedAdmin
	}
	for _, admin := range cfg.AdminUsers {
		if user == admin {
			return true
		}
	}
	// Super-adminship implies admin: whoever can administer the platform
	// can obviously see the usage page the console links to.
	for _, admin := range cfg.SuperAdminUsers {
		if user == admin {
			return true
		}
	}
	return false
}

// IsSuperAdmin reports whether the caller may reach the admin console,
// Routing, Compression, and every admin-gated API. Most "admins" only ever
// want the usage page; mutating platform state stays with the operators.
func IsSuperAdmin(cfg config.Config, r *http.Request) bool {
	user := r.Header.Get(cfg.UserHeader)
	if user == "" {
		return cfg.AllowUnauthenticatedAdmin
	}
	for _, admin := range cfg.SuperAdminUsers {
		if user == admin {
			return true
		}
	}
	return false
}

// RequireAdmin gates a handler behind IsAdmin, sending everyone else to
// their own account page.
func RequireAdmin(cfg config.Config, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !IsAdmin(cfg, r) {
			slog.Debug("admin access denied", "path", r.URL.Path)
			http.Redirect(w, r, "/me", http.StatusFound)
			return
		}
		next(w, r)
	}
}

// RequireSuperAdmin gates the operator-only surface (admin console, routing,
// compression, admin APIs). An admin who is not a super-admin lands on the
// usage dashboard — the one page they should be looking at anyway.
func RequireSuperAdmin(cfg config.Config, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !IsSuperAdmin(cfg, r) {
			slog.Debug("super-admin access denied", "path", r.URL.Path)
			if IsAdmin(cfg, r) {
				http.Redirect(w, r, "/dashboard", http.StatusFound)
			} else {
				http.Redirect(w, r, "/me", http.StatusFound)
			}
			return
		}
		next(w, r)
	}
}

func (h *AdminHandler) ServeAdmin(w http.ResponseWriter, r *http.Request) {
	data, err := fs.ReadFile(dashboard.FS, "admin.html")
	if err != nil {
		http.Error(w, "admin page not found", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(data)
}

func (h *AdminHandler) ServeRouting(w http.ResponseWriter, r *http.Request) {
	data, err := fs.ReadFile(dashboard.FS, "routing.html")
	if err != nil {
		http.Error(w, "routing page not found", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(data)
}

func (h *AdminHandler) ServeCompression(w http.ResponseWriter, r *http.Request) {
	data, err := fs.ReadFile(dashboard.FS, "compression.html")
	if err != nil {
		http.Error(w, "compression page not found", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(data)
}

func (h *AdminHandler) HandleProviders(w http.ResponseWriter, r *http.Request) {
	if h.k8sClient == nil {
		writeJSON(w, []k8s.ProviderInfo{})
		return
	}
	providers, err := h.k8sClient.ListProviders(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if providers == nil {
		providers = []k8s.ProviderInfo{}
	}
	writeJSON(w, providers)
}

func (h *AdminHandler) HandleModels(w http.ResponseWriter, r *http.Request) {
	if h.k8sClient == nil {
		writeJSON(w, []k8s.ModelInfo{})
		return
	}
	models, err := h.k8sClient.ListModels(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if models == nil {
		models = []k8s.ModelInfo{}
	}
	writeJSON(w, models)
}

func (h *AdminHandler) HandleConfig(w http.ResponseWriter, r *http.Request) {
	if h.k8sClient == nil {
		writeJSON(w, &k8s.PipelineConfig{Profiles: []k8s.ProfileInfo{}, ActiveProfile: "default"})
		return
	}
	pipeline, err := h.k8sClient.GetPipelineConfig(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, pipeline)
}

func (h *AdminHandler) HandleUpdateProvider(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.k8sClient == nil {
		http.Error(w, "k8s client not available", http.StatusServiceUnavailable)
		return
	}

	// Path: /api/v1/admin/models/provider/{modelName}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 6 {
		http.Error(w, "invalid path — expected /api/v1/admin/models/provider/{name}", http.StatusBadRequest)
		return
	}
	modelName := parts[5]

	var body struct {
		ProviderName string `json:"providerName"`
		TargetModel  string `json:"targetModel"`
		APIFormat    string `json:"apiFormat"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	if err := h.k8sClient.UpdateModelProvider(r.Context(), modelName, body.ProviderName, body.TargetModel, body.APIFormat); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "updated"})
}

func (h *AdminHandler) HandleUpdateWeights(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.k8sClient == nil {
		http.Error(w, "k8s client not available", http.StatusServiceUnavailable)
		return
	}

	// Extract model name from path: /api/v1/admin/models/{name}/weights
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 5 {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	modelName := parts[4]

	var weights map[string]int64
	if err := json.NewDecoder(r.Body).Decode(&weights); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	if err := h.k8sClient.UpdateModelWeights(r.Context(), modelName, weights); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "updated"})
}

// sessionGroups returns the caller's groups from the header the login flow
// stored (JSON array string, with a CSV fallback).
func sessionGroups(r *http.Request, cfg config.Config) []string {
	var gs []string
	if raw := r.Header.Get(cfg.GroupsHeader); raw != "" {
		if err := json.Unmarshal([]byte(raw), &gs); err != nil {
			for _, g := range strings.Split(raw, ",") {
				if g = strings.TrimSpace(g); g != "" {
					gs = append(gs, g)
				}
			}
		}
	}
	return gs
}

func (h *AdminHandler) HandleUsers(w http.ResponseWriter, r *http.Request) {
	if h.k8sClient == nil {
		writeJSON(w, []k8s.OpenShiftUser{})
		return
	}
	users, err := h.k8sClient.GetOpenShiftUsers(r.Context())
	if err != nil {
		writeJSON(w, []k8s.OpenShiftUser{})
		return
	}
	writeJSON(w, users)
}

func (h *AdminHandler) HandleGroupMember(w http.ResponseWriter, r *http.Request) {
	if h.k8sClient == nil {
		http.Error(w, "k8s not available", http.StatusServiceUnavailable)
		return
	}

	var body struct {
		Group    string `json:"group"`
		Username string `json:"username"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	var err error
	switch r.Method {
	case http.MethodPost:
		err = h.k8sClient.AddUserToGroup(r.Context(), body.Group, body.Username)
	case http.MethodDelete:
		err = h.k8sClient.RemoveUserFromGroup(r.Context(), body.Group, body.Username)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (h *AdminHandler) HandleGroups(w http.ResponseWriter, r *http.Request) {
	if h.k8sClient == nil {
		writeJSON(w, []k8s.GroupInfo{})
		return
	}
	groups, err := h.k8sClient.GetOpenShiftGroups(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if groups == nil {
		groups = []k8s.GroupInfo{}
	}
	writeJSON(w, groups)
}

func (h *AdminHandler) HandleAuthPolicies(w http.ResponseWriter, r *http.Request) {
	if h.k8sClient == nil {
		writeJSON(w, []k8s.AuthPolicyInfo{})
		return
	}
	policies, err := h.k8sClient.GetAuthPolicies(r.Context(), "")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if policies == nil {
		policies = []k8s.AuthPolicyInfo{}
	}
	writeJSON(w, policies)
}

func (h *AdminHandler) HandleSubscriptions(w http.ResponseWriter, r *http.Request) {
	if h.k8sClient == nil {
		writeJSON(w, []k8s.SubscriptionInfo{})
		return
	}
	subs, err := h.k8sClient.GetSubscriptions(r.Context(), "")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if subs == nil {
		subs = []k8s.SubscriptionInfo{}
	}
	writeJSON(w, subs)
}

// HandleValidGroups returns the canonical, live list of org groups the
// gateway actually grants a model subscription to — spec.owner.groups on
// the configured MaaSSubscription CR — for the People & Org group picker.
func (h *AdminHandler) HandleValidGroups(w http.ResponseWriter, r *http.Request) {
	if h.k8sClient == nil {
		writeJSON(w, map[string][]string{"groups": {}})
		return
	}
	groups, err := h.k8sClient.GetMaaSSubscriptionGroups(r.Context(), h.cfg.Kubernetes.Namespace, h.cfg.Kubernetes.SubscriptionName)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	sort.Strings(groups)
	writeJSON(w, map[string][]string{"groups": groups})
}

// HandleRoles returns the configured admin and super-admin identities so the
// People & Org table can badge each person's platform role. Read-only: both
// lists are env-derived (ADMIN_USERS / SUPERADMIN_USERS) and cannot be edited
// from the console — the operators own that surface deliberately.
func (h *AdminHandler) HandleRoles(w http.ResponseWriter, r *http.Request) {
	admins := append([]string{}, h.cfg.AdminUsers...)
	supers := append([]string{}, h.cfg.SuperAdminUsers...)
	sort.Strings(admins)
	sort.Strings(supers)
	writeJSON(w, map[string][]string{"admins": admins, "superAdmins": supers})
}

// platformGroups returns the group set to scope maas-api v1 calls with: the
// configured MaaSSubscription's live groups — the same source the gateway
// enforces and the valid-groups endpoint serves. maas-api requires a
// non-empty X-MaaS-Group (it scopes the internal token it mints), so an
// empty or unreadable list is an error; OpenShift Group objects are not a
// valid substitute — they are empty on every real deployment.
func (h *AdminHandler) platformGroups(ctx context.Context) ([]string, error) {
	if h.k8sClient == nil {
		return nil, fmt.Errorf("kubernetes client not configured")
	}
	groups, err := h.k8sClient.GetMaaSSubscriptionGroups(ctx, h.cfg.Kubernetes.Namespace, h.cfg.Kubernetes.SubscriptionName)
	if err != nil {
		return nil, err
	}
	if len(groups) == 0 {
		return nil, fmt.Errorf("MaaSSubscription %s/%s lists no groups", h.cfg.Kubernetes.Namespace, h.cfg.Kubernetes.SubscriptionName)
	}
	return groups, nil
}

func (h *AdminHandler) HandleKeys(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.listKeys(w, r)
	case http.MethodPost:
		h.createKey(w, r)
	case http.MethodDelete:
		h.revokeKey(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *AdminHandler) listKeys(w http.ResponseWriter, r *http.Request) {
	if h.maasClient == nil {
		writeJSON(w, &maasapi.SearchResult{Data: []maasapi.APIKeyResponse{}})
		return
	}
	username := r.URL.Query().Get("username")
	// Non-admins are pinned to their own keys regardless of the query.
	if !IsAdmin(h.cfg, r) {
		username = r.Header.Get(h.cfg.UserHeader)
	}
	groups, err := h.platformGroups(r.Context())
	if err != nil {
		slog.Error("key search skipped: no platform groups", "error", err)
		writeJSON(w, &maasapi.SearchResult{Data: []maasapi.APIKeyResponse{}})
		return
	}
	result, err := h.maasClient.SearchAPIKeys(r.Context(), username, groups)
	if err != nil {
		slog.Error("key search failed", "error", err)
		writeJSON(w, &maasapi.SearchResult{Data: []maasapi.APIKeyResponse{}})
		return
	}
	writeJSON(w, result)
}

func (h *AdminHandler) createKey(w http.ResponseWriter, r *http.Request) {
	if h.maasClient == nil {
		http.Error(w, "maas-api client not available", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		Username string `json:"username"`
		Group    string `json:"group"`
		KeyName  string `json:"keyName"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	// Self-service: a regular user may only create a key for themselves, in
	// one of their own groups (the redesigned user dashboard's create form).
	if !IsAdmin(h.cfg, r) {
		body.Username = r.Header.Get(h.cfg.UserHeader)
		groups := sessionGroups(r, h.cfg)
		ok := body.Group == ""
		for _, g := range groups {
			if g == body.Group {
				ok = true
				break
			}
		}
		if !ok {
			if len(groups) > 0 {
				body.Group = groups[0]
			} else {
				body.Group = ""
			}
		}
	}
	if body.Username == "" || body.KeyName == "" {
		http.Error(w, "username and keyName are required", http.StatusBadRequest)
		return
	}

	result, err := h.maasClient.CreateAPIKey(r.Context(), body.Username, body.Group, body.KeyName)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, result)
}

func (h *AdminHandler) revokeKey(w http.ResponseWriter, r *http.Request) {
	if h.maasClient == nil {
		http.Error(w, "maas-api client not available", http.StatusServiceUnavailable)
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 5 {
		http.Error(w, "key ID required in path", http.StatusBadRequest)
		return
	}
	keyID := parts[4]

	groups, err := h.platformGroups(r.Context())
	if err != nil {
		slog.Error("key revoke blocked: no platform groups", "key", keyID, "error", err)
		http.Error(w, "group scope unavailable for key operations", http.StatusServiceUnavailable)
		return
	}

	// Non-admins may only revoke a key that belongs to them.
	if !IsAdmin(h.cfg, r) {
		caller := r.Header.Get(h.cfg.UserHeader)
		own, err := h.maasClient.SearchAPIKeys(r.Context(), caller, groups)
		if err != nil {
			http.Error(w, "unable to verify key ownership", http.StatusInternalServerError)
			return
		}
		owns := false
		for _, k := range own.Data {
			if k.ID == keyID {
				owns = true
				break
			}
		}
		if !owns {
			http.Error(w, "can only revoke your own keys", http.StatusForbidden)
			return
		}
	}

	if err := h.maasClient.RevokeAPIKey(r.Context(), keyID, groups); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "revoked"})
}
