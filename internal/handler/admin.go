package handler

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"strings"

	"github.com/noyitz/ai-gateway-metering-service/internal/dashboard"
	"github.com/noyitz/ai-gateway-metering-service/internal/k8s"
	"github.com/noyitz/ai-gateway-metering-service/internal/maasapi"
)

type AdminHandler struct {
	k8sClient  *k8s.Client
	maasClient *maasapi.Client
}

func NewAdminHandler(k8sClient *k8s.Client, maasClient *maasapi.Client) *AdminHandler {
	return &AdminHandler{k8sClient: k8sClient, maasClient: maasClient}
}

func (h *AdminHandler) ServeAdmin(w http.ResponseWriter, r *http.Request) {
	data, err := fs.ReadFile(dashboard.FS, "admin.html")
	if err != nil {
		http.Error(w, "admin page not found", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(data)
}

func (h *AdminHandler) ServeRouting(w http.ResponseWriter, r *http.Request) {
	data, err := fs.ReadFile(dashboard.FS, "routing.html")
	if err != nil {
		http.Error(w, "routing page not found", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(data)
}

func (h *AdminHandler) ServeCompression(w http.ResponseWriter, r *http.Request) {
	data, err := fs.ReadFile(dashboard.FS, "compression.html")
	if err != nil {
		http.Error(w, "compression page not found", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
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
		writeJSON(w, &k8s.IPPConfig{Profiles: []k8s.ProfileInfo{}, ActiveProfile: "default"})
		return
	}
	config, err := h.k8sClient.GetIPPConfig(r.Context(), "openshift-ingress")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, config)
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
	var groups []string
	if h.k8sClient != nil {
		if gs, err := h.k8sClient.GetOpenShiftGroups(r.Context()); err == nil {
			for _, g := range gs {
				groups = append(groups, g.Name)
			}
		}
	}
	result, err := h.maasClient.SearchAPIKeys(r.Context(), username, groups)
	if err != nil {
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

	var groups []string
	if h.k8sClient != nil {
		if gs, err := h.k8sClient.GetOpenShiftGroups(r.Context()); err == nil {
			for _, g := range gs {
				groups = append(groups, g.Name)
			}
		}
	}
	if err := h.maasClient.RevokeAPIKey(r.Context(), keyID, groups); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "revoked"})
}
