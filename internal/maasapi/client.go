package maasapi

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
)

type Client struct {
	baseURL string
	tenant  string
	client  *http.Client
}

type APIKeyResponse struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Key            string `json:"key,omitempty"`
	Username       string `json:"username"`
	Subscription   string `json:"subscription"`
	Tenant         string `json:"tenant"`
	Status         string `json:"status"`
	CreationDate   string `json:"creationDate,omitempty"`
	ExpirationDate string `json:"expirationDate,omitempty"`
	LastUsedAt     string `json:"lastUsedAt,omitempty"`
}

type SearchResult struct {
	Object  string           `json:"object"`
	Data    []APIKeyResponse `json:"data"`
	HasMore bool             `json:"has_more"`
}

func NewClient(baseURL, tenant string) *Client {
	return &Client{
		baseURL: baseURL,
		tenant:  tenant,
		client: &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			},
		},
	}
}

func (c *Client) CreateAPIKey(ctx context.Context, username, group, keyName string) (*APIKeyResponse, error) {
	body, _ := json.Marshal(map[string]string{"name": keyName})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/api-keys", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-MaaS-Username", username)
	req.Header.Set("X-MaaS-Group", fmt.Sprintf(`["%s"]`, group))
	req.Header.Set("X-MaaS-Tenant", c.tenant)

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("maas-api request failed: %w", err)
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("maas-api error %d: %s", resp.StatusCode, string(data))
	}

	var result APIKeyResponse
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}
	return &result, nil
}

func (c *Client) SearchAPIKeys(ctx context.Context, username string, groups []string) (*SearchResult, error) {
	searchBody := map[string]any{}
	if username != "" {
		searchBody["username"] = username
	}
	body, _ := json.Marshal(searchBody)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/api-keys/search", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-MaaS-Username", "admin")
	req.Header.Set("X-MaaS-Tenant", c.tenant)
	if len(groups) > 0 {
		groupJSON, _ := json.Marshal(groups)
		req.Header.Set("X-MaaS-Group", string(groupJSON))
	}
	if token := saToken(); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("maas-api request failed: %w", err)
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(resp.Body)
	slog.Info("maas-api search response", "status", resp.StatusCode, "body", string(data), "url", c.baseURL+"/v1/api-keys/search")
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("maas-api error %d: %s", resp.StatusCode, string(data))
	}

	var result SearchResult
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}
	return &result, nil
}

func (c *Client) RevokeAPIKey(ctx context.Context, keyID string, groups []string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.baseURL+"/v1/api-keys/"+keyID, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-MaaS-Username", "admin")
	req.Header.Set("X-MaaS-Tenant", c.tenant)
	if len(groups) > 0 {
		groupJSON, _ := json.Marshal(groups)
		req.Header.Set("X-MaaS-Group", string(groupJSON))
	}
	if token := saToken(); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("maas-api request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		data, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("maas-api error %d: %s", resp.StatusCode, string(data))
	}
	return nil
}

func saToken() string {
	data, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/token")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
