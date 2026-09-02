package handler

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/noyitz/ai-gateway-metering-service/internal/config"
	"github.com/noyitz/ai-gateway-metering-service/internal/k8s"
	"github.com/noyitz/ai-gateway-metering-service/internal/storage"
)

// KeysHandler proxies API key management to an upstream key service on
// behalf of the signed-in user. The upstream is expected to expose:
//
//	POST   {url}/v1/api-keys/search   list the caller's keys
//	POST   {url}/v1/api-keys          issue a key
//	DELETE {url}/v1/api-keys/{id}     revoke a key
//
// The handler is inert unless KEY_SERVICE_URL is set, in which case the
// endpoints report 501 and the account page simply omits key management.
type KeysHandler struct {
	k8sClient *k8s.Client
	cfg       config.Config
	store     *storage.Store
	client    *http.Client
}

func NewKeysHandler(k8sClient *k8s.Client, cfg config.Config, store *storage.Store) *KeysHandler {
	return &KeysHandler{
		k8sClient: k8sClient,
		cfg:       cfg,
		store:     store,
		client: &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					InsecureSkipVerify: cfg.KeyService.InsecureSkipVerify, //nolint:gosec // opt-in for internal self-signed certificates
				},
			},
		},
	}
}

// identityHeaders forwards the caller's identity upstream, resolving group
// membership from the request, then the optional cluster adapter, then the
// configured default.
func (h *KeysHandler) identityHeaders(r *http.Request) map[string]string {
	user := r.Header.Get(h.cfg.UserHeader)
	groups := r.Header.Get(h.cfg.GroupsHeader)

	if groups == "" && h.k8sClient != nil && user != "" {
		userGroups, _ := h.k8sClient.GetUserGroups(user)
		if len(userGroups) > 0 {
			groups = `["` + strings.Join(userGroups, `","`) + `"]`
		}
	}
	if groups == "" {
		groups = `["` + h.cfg.DefaultGroup + `"]`
	}

	headers := map[string]string{
		h.cfg.KeyService.UserHeader:   user,
		h.cfg.KeyService.GroupsHeader: groups,
		"Content-Type":                "application/json",
	}
	if h.cfg.KeyService.Tenant != "" {
		headers[h.cfg.KeyService.TenantHeader] = h.cfg.KeyService.Tenant
	}
	return headers
}

func (h *KeysHandler) proxy(method, path string, headers map[string]string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequest(method, h.cfg.KeyService.URL+path, body)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return h.client.Do(req)
}

func (h *KeysHandler) HandleKeys(w http.ResponseWriter, r *http.Request) {
	if !h.cfg.KeyService.Enabled() {
		http.Error(w, "key management is not configured — set KEY_SERVICE_URL to enable it", http.StatusNotImplemented)
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/me/keys")
	path = strings.TrimSuffix(path, "/")
	hdrs := h.identityHeaders(r)

	switch {
	case r.Method == http.MethodGet && path == "":
		h.listKeys(w, hdrs)
	case r.Method == http.MethodPost && path == "":
		h.createKey(w, r, hdrs)
	case r.Method == http.MethodDelete && path != "":
		keyID := strings.TrimPrefix(path, "/")
		h.revokeKey(w, keyID, hdrs)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *KeysHandler) listKeys(w http.ResponseWriter, hdrs map[string]string) {
	body := `{"filters":{},"pagination":{"limit":50,"offset":0},"sort":{"by":"created_at","order":"desc"}}`
	h.forward(w, http.MethodPost, "/v1/api-keys/search", hdrs, strings.NewReader(body))
}

func (h *KeysHandler) createKey(w http.ResponseWriter, r *http.Request, hdrs map[string]string) {
	h.forward(w, http.MethodPost, "/v1/api-keys", hdrs, r.Body)
}

func (h *KeysHandler) revokeKey(w http.ResponseWriter, keyID string, hdrs map[string]string) {
	h.forward(w, http.MethodDelete, "/v1/api-keys/"+keyID, hdrs, nil)
}

func (h *KeysHandler) forward(w http.ResponseWriter, method, path string, hdrs map[string]string, body io.Reader) {
	resp, err := h.proxy(method, path, hdrs, body)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to reach key service: %v", err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body) //nolint:errcheck // response already committed
}

// HandleWhoAmI echoes the identity the authenticating proxy asserted, which
// the account page uses to decide what to show. While an admin is "viewing
// as" another user, `user` is the impersonated identity and `real_user` the
// admin's own, so the page can render a banner with a way back.
func (h *KeysHandler) HandleWhoAmI(w http.ResponseWriter, r *http.Request) {
	user := r.Header.Get(h.cfg.UserHeader)
	real := r.Header.Get(realUserHeader)
	// real is only set while an admin holds a "view as" claim (RequireAuth
	// writes it), including the "My view as user" self case where real==user.
	impersonating := real != ""

	// Groups arrive as the JSON array string the login flow stored in the
	// session; expose them as a real array (CSV fallback) so pages don't
	// have to guess the encoding.
	var groups []string
	if raw := r.Header.Get(h.cfg.GroupsHeader); raw != "" {
		var gs []string
		if err := json.Unmarshal([]byte(raw), &gs); err != nil {
			for _, g := range strings.Split(raw, ",") {
				if g = strings.TrimSpace(g); g != "" {
					gs = append(gs, g)
				}
			}
		}
		groups = gs
	}
	if groups == nil {
		groups = []string{}
	}

	w.Header().Set("Content-Type", "application/json")
	resp := map[string]any{
		"user":              user,
		"groups":            groups,
		"isAdmin":           IsAdmin(h.cfg, r),
		"keyServiceEnabled": h.cfg.KeyService.Enabled(),
		"impersonating":     impersonating,
		"real_user":         "",
		// "First Last" for the identity the page is showing (the impersonated
		// user while an admin is "viewing as", else the caller themselves).
		"display_name": "",
	}
	if h.store != nil && user != "" {
		if p, err := h.store.GetUserProfile(r.Context(), user); err == nil {
			resp["display_name"] = p.DisplayName()
		}
	}
	// githubId backs the redesigned user dashboard's avatar; only available
	// when the kubernetes adapter can read OpenShift users.
	if h.k8sClient != nil && user != "" {
		if users, err := h.k8sClient.GetOpenShiftUsers(r.Context()); err == nil {
			for _, u := range users {
				if u.Name == user && u.GitHubID != "" {
					resp["githubId"] = u.GitHubID
					break
				}
			}
		}
	}
	if impersonating {
		resp["real_user"] = real
		if h.store != nil && real != "" {
			if p, err := h.store.GetUserProfile(r.Context(), real); err == nil {
				resp["real_user_display"] = p.DisplayName()
			}
		}
	}
	json.NewEncoder(w).Encode(resp) //nolint:errcheck // response already committed
}
