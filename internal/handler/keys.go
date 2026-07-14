package handler

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/noyitz/ai-gateway-metering-service/internal/k8s"
)

const maasAPIURL = "https://maas-api.opendatahub.svc.cluster.local:8443"

var maasHTTPClient = &http.Client{
	Timeout: 10 * time.Second,
	Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	},
}

type KeysHandler struct {
	k8sClient *k8s.Client
}

func NewKeysHandler(k8sClient *k8s.Client) *KeysHandler {
	return &KeysHandler{k8sClient: k8sClient}
}

func (h *KeysHandler) maasHeaders(r *http.Request) map[string]string {
	user := r.Header.Get("X-Forwarded-User")
	groups := r.Header.Get("X-Forwarded-Groups")
	if groups == "" && h.k8sClient != nil && user != "" {
		userGroups, _ := h.k8sClient.GetUserGroups(user)
		if len(userGroups) > 0 {
			groups = `["` + strings.Join(userGroups, `","`) + `"]`
		}
	}
	if groups == "" {
		groups = `["ai-eng"]`
	}
	return map[string]string{
		"X-MaaS-Username": user,
		"X-MaaS-Group":    groups,
		"X-MaaS-Tenant":   "models-as-a-service",
		"Content-Type":    "application/json",
	}
}

func proxyToMaaS(method, path string, headers map[string]string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequest(method, maasAPIURL+path, body)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return maasHTTPClient.Do(req)
}

func (h *KeysHandler) HandleKeys(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/me/keys")
	path = strings.TrimSuffix(path, "/")
	hdrs := h.maasHeaders(r)

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
	resp, err := proxyToMaaS("POST", "/v1/api-keys/search", hdrs, strings.NewReader(body))
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to reach maas-api: %v", err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

func (h *KeysHandler) createKey(w http.ResponseWriter, r *http.Request, hdrs map[string]string) {
	resp, err := proxyToMaaS("POST", "/v1/api-keys", hdrs, r.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to reach maas-api: %v", err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

func (h *KeysHandler) revokeKey(w http.ResponseWriter, keyID string, hdrs map[string]string) {
	resp, err := proxyToMaaS("DELETE", "/v1/api-keys/"+keyID, hdrs, nil)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to reach maas-api: %v", err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

func (h *KeysHandler) HandleWhoAmI(w http.ResponseWriter, r *http.Request) {
	user := r.Header.Get("X-Forwarded-User")
	groups := r.Header.Get("X-Forwarded-Groups")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"user": user, "groups": groups})
}
