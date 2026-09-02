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
	client    *http.Client
}

func NewKeysHandler(k8sClient *k8s.Client, cfg config.Config) *KeysHandler {
	return &KeysHandler{
		k8sClient: k8sClient,
		cfg:       cfg,
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
// the account page uses to decide what to show.
func (h *KeysHandler) HandleWhoAmI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck // response already committed
		"user":              r.Header.Get(h.cfg.UserHeader),
		"groups":            r.Header.Get(h.cfg.GroupsHeader),
		"isAdmin":           IsAdmin(h.cfg, r),
		"keyServiceEnabled": h.cfg.KeyService.Enabled(),
	})
}
