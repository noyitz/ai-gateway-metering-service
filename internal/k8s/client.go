package k8s

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/noyitz/ai-gateway-metering-service/internal/config"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

// This adapter reads a model catalogue from custom resources. Resource
// coordinates come from configuration rather than being compiled in,
// because each gateway names and versions its catalogue differently.

type ProviderInfo struct {
	Name           string `json:"name"`
	Provider       string `json:"provider"`
	Endpoint       string `json:"endpoint"`
	Phase          string `json:"phase"`
	AuthType       string `json:"authType"`
	SecretName     string `json:"secretName"`
	HasCredentials bool   `json:"hasCredentials"`
}

type ProviderRef struct {
	ProviderName string `json:"providerName"`
	TargetModel  string `json:"targetModel"`
	APIFormat    string `json:"apiFormat"`
	Weight       int64  `json:"weight"`
}

type ModelInfo struct {
	Name         string        `json:"name"`
	Namespace    string        `json:"namespace"`
	Provider     string        `json:"provider"`
	TargetModel  string        `json:"targetModel"`
	Endpoint     string        `json:"endpoint"`
	ProviderRefs []ProviderRef `json:"providerRefs"`
}

type Client struct {
	client    dynamic.Interface
	namespace string
	cfg       config.Kubernetes
}

func NewClient(cfg config.Kubernetes) (*Client, error) {
	restConfig, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to get in-cluster config: %w", err)
	}

	dynClient, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create dynamic client: %w", err)
	}

	slog.Info("kubernetes adapter initialized",
		"namespace", cfg.Namespace,
		"modelResource", cfg.ModelResource,
		"modelGroup", cfg.ModelGroup)
	return &Client{client: dynClient, namespace: cfg.Namespace, cfg: cfg}, nil
}

func (c *Client) modelGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    c.cfg.ModelGroup,
		Version:  c.cfg.ModelVersion,
		Resource: c.cfg.ModelResource,
	}
}

func (c *Client) providerGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    c.cfg.ProviderGroup,
		Version:  c.cfg.ProviderVersion,
		Resource: c.cfg.ProviderResource,
	}
}

func (c *Client) ListProviders(ctx context.Context) ([]ProviderInfo, error) {
	if c.cfg.ProviderGroup == "" {
		return c.listProvidersFromConfig(ctx)
	}
	list, err := c.client.Resource(c.providerGVR()).Namespace(c.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", c.cfg.ProviderResource, err)
	}

	var result []ProviderInfo
	for _, item := range list.Items {
		name := item.GetName()
		provider, _, _ := unstructured.NestedString(item.Object, "spec", "provider")
		endpoint, _, _ := unstructured.NestedString(item.Object, "spec", "endpoint")
		phase, _, _ := unstructured.NestedString(item.Object, "status", "phase")
		authType, _, _ := unstructured.NestedString(item.Object, "spec", "auth", "type")
		secretName, _, _ := unstructured.NestedString(item.Object, "spec", "auth", "secretRef", "name")

		result = append(result, ProviderInfo{
			Name:           name,
			Provider:       provider,
			Endpoint:       endpoint,
			Phase:          phase,
			AuthType:       authType,
			SecretName:     secretName,
			HasCredentials: c.secretExists(ctx, secretName),
		})
	}
	return result, nil
}

func (c *Client) ListModels(ctx context.Context) ([]ModelInfo, error) {
	if c.cfg.ModelGroup == "" {
		return c.listModelsFromConfig(ctx)
	}
	list, err := c.client.Resource(c.modelGVR()).Namespace(c.namespace).List(ctx, metav1.ListOptions{})

	// A configured fallback group covers catalogues mid-migration between
	// two API groups.
	if (err != nil || len(list.Items) == 0) && c.cfg.ModelFallbackGroup != "" {
		fallback := c.modelGVR()
		fallback.Group = c.cfg.ModelFallbackGroup
		list, err = c.client.Resource(fallback).Namespace(c.namespace).List(ctx, metav1.ListOptions{})
	}
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", c.cfg.ModelResource, err)
	}

	var result []ModelInfo
	for _, item := range list.Items {
		info := ModelInfo{
			Name:      item.GetName(),
			Namespace: item.GetNamespace(),
		}

		// Try new schema (externalProviderRefs)
		refs, found, _ := unstructured.NestedSlice(item.Object, "spec", "externalProviderRefs")
		if found && len(refs) > 0 {
			for _, ref := range refs {
				refMap, ok := ref.(map[string]interface{})
				if !ok {
					continue
				}
				pr := ProviderRef{Weight: 1}
				if nameRef, ok := refMap["ref"].(map[string]interface{}); ok {
					pr.ProviderName, _ = nameRef["name"].(string)
				}
				pr.TargetModel, _ = refMap["targetModel"].(string)
				pr.APIFormat, _ = refMap["apiFormat"].(string)
				if w, ok := refMap["weight"].(int64); ok {
					pr.Weight = w
				} else if w, ok := refMap["weight"].(float64); ok {
					pr.Weight = int64(w)
				}
				info.ProviderRefs = append(info.ProviderRefs, pr)
			}
		} else {
			// Legacy schema
			info.Provider, _, _ = unstructured.NestedString(item.Object, "spec", "provider")
			info.TargetModel, _, _ = unstructured.NestedString(item.Object, "spec", "targetModel")
			info.Endpoint, _, _ = unstructured.NestedString(item.Object, "spec", "endpoint")
		}

		result = append(result, info)
	}
	return result, nil
}

func (c *Client) UpdateModelWeights(ctx context.Context, modelName string, weights map[string]int64) error {
	model, err := c.client.Resource(c.modelGVR()).Namespace(c.namespace).Get(ctx, modelName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get %s %s: %w", c.cfg.ModelResource, modelName, err)
	}

	refs, found, _ := unstructured.NestedSlice(model.Object, "spec", "externalProviderRefs")
	if !found || len(refs) == 0 {
		return fmt.Errorf("model %s has no externalProviderRefs", modelName)
	}

	for i, ref := range refs {
		refMap, ok := ref.(map[string]interface{})
		if !ok {
			continue
		}
		nameRef, ok := refMap["ref"].(map[string]interface{})
		if !ok {
			continue
		}
		provName, _ := nameRef["name"].(string)
		if w, exists := weights[provName]; exists {
			refMap["weight"] = w
			refs[i] = refMap
		}
	}

	patch := map[string]interface{}{
		"spec": map[string]interface{}{
			"externalProviderRefs": refs,
		},
	}
	patchBytes, _ := json.Marshal(patch)

	_, err = c.client.Resource(c.modelGVR()).Namespace(c.namespace).Patch(
		ctx, modelName, types.MergePatchType, patchBytes, metav1.PatchOptions{})
	return err
}

type ProfileInfo struct {
	Name            string   `json:"name"`
	RequestPlugins  []string `json:"requestPlugins"`
	ResponsePlugins []string `json:"responsePlugins"`
}

// PipelineConfig is a read-only view of the gateway's filter pipeline,
// surfaced in the admin console so an operator can see which filters are
// active alongside the usage they produce.
type PipelineConfig struct {
	Profiles      []ProfileInfo `json:"profiles"`
	ActiveProfile string        `json:"activeProfile"`
}

// GetPipelineConfig reads the gateway pipeline description from a
// ConfigMap. It returns an empty config when no ConfigMap is configured,
// so the admin console degrades to showing usage only.
func (c *Client) GetPipelineConfig(ctx context.Context) (*PipelineConfig, error) {
	empty := &PipelineConfig{Profiles: []ProfileInfo{}, ActiveProfile: "default"}
	name := c.cfg.PipelineConfigMap
	if name == "" {
		return empty, nil
	}

	namespace := c.cfg.PipelineConfigMapNamespace
	if namespace == "" {
		namespace = c.namespace
	}

	cmGVR := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"}
	cm, err := c.client.Resource(cmGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get ConfigMap %s/%s: %w", namespace, name, err)
	}

	key := c.cfg.PipelineConfigMapKey
	configYAML, found, _ := unstructured.NestedString(cm.Object, "data", key)
	if !found {
		return nil, fmt.Errorf("key %q not found in ConfigMap %s/%s", key, namespace, name)
	}

	// Try IPP format first (profiles[].plugins.request/response), then
	// fall back to praxis format (filter_chains[].filters[]).
	var ipp struct {
		Profiles []struct {
			Name    string `yaml:"name"`
			Plugins struct {
				Request []struct {
					PluginRef string `yaml:"pluginRef"`
				} `yaml:"request"`
				Response []struct {
					PluginRef string `yaml:"pluginRef"`
				} `yaml:"response"`
			} `yaml:"plugins"`
		} `yaml:"profiles"`
	}
	if err := yaml.Unmarshal([]byte(configYAML), &ipp); err != nil {
		return nil, fmt.Errorf("parse pipeline config YAML: %w", err)
	}

	if len(ipp.Profiles) > 0 {
		result := &PipelineConfig{ActiveProfile: "default"}
		for _, p := range ipp.Profiles {
			pi := ProfileInfo{Name: p.Name}
			for _, rp := range p.Plugins.Request {
				pi.RequestPlugins = append(pi.RequestPlugins, rp.PluginRef)
			}
			for _, rp := range p.Plugins.Response {
				pi.ResponsePlugins = append(pi.ResponsePlugins, rp.PluginRef)
			}
			result.Profiles = append(result.Profiles, pi)
		}
		return result, nil
	}

	// Praxis format: filter_chains[].filters[] — each chain is a profile,
	// filters are listed as request plugins (praxis doesn't split req/res).
	var praxis struct {
		FilterChains []struct {
			Name    string `yaml:"name"`
			Filters []struct {
				Filter string `yaml:"filter"`
			} `yaml:"filters"`
		} `yaml:"filter_chains"`
	}
	if err := yaml.Unmarshal([]byte(configYAML), &praxis); err != nil {
		return nil, fmt.Errorf("parse praxis config YAML: %w", err)
	}

	result := &PipelineConfig{ActiveProfile: "default"}
	for _, fc := range praxis.FilterChains {
		pi := ProfileInfo{Name: fc.Name}
		for _, f := range fc.Filters {
			pi.RequestPlugins = append(pi.RequestPlugins, f.Filter)
		}
		result.Profiles = append(result.Profiles, pi)
	}
	return result, nil
}

// readConfigMapYAML reads the pipeline ConfigMap and returns the raw YAML.
func (c *Client) readConfigMapYAML(ctx context.Context) (string, error) {
	name := c.cfg.PipelineConfigMap
	if name == "" {
		return "", fmt.Errorf("no pipeline ConfigMap configured")
	}
	namespace := c.cfg.PipelineConfigMapNamespace
	if namespace == "" {
		namespace = c.namespace
	}
	cmGVR := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"}
	cm, err := c.client.Resource(cmGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("get ConfigMap %s/%s: %w", namespace, name, err)
	}
	raw, found, _ := unstructured.NestedString(cm.Object, "data", c.cfg.PipelineConfigMapKey)
	if !found {
		return "", fmt.Errorf("key %q not found in ConfigMap %s/%s", c.cfg.PipelineConfigMapKey, namespace, name)
	}
	return raw, nil
}

type praxisFilterChain struct {
	Name    string           `yaml:"name"`
	Filters []map[string]any `yaml:"filters"`
}

type praxisTop struct {
	FilterChains []praxisFilterChain `yaml:"filter_chains"`
}

type catalogModel struct {
	ID          string `yaml:"id"`
	DisplayName string `yaml:"display_name"`
	OwnedBy    string `yaml:"owned_by"`
}

type routerRoute struct {
	PathPrefix string            `yaml:"path_prefix"`
	Headers    map[string]string `yaml:"headers"`
	Cluster    string            `yaml:"cluster"`
}

type lbCluster struct {
	Name      string   `yaml:"name"`
	Endpoints []string `yaml:"endpoints"`
	TLS       *struct {
		SNI string `yaml:"sni"`
	} `yaml:"tls"`
}

func remarshal(src any, dst any) error {
	b, err := yaml.Marshal(src)
	if err != nil {
		return err
	}
	return yaml.Unmarshal(b, dst)
}

func (c *Client) listModelsFromConfig(ctx context.Context) ([]ModelInfo, error) {
	raw, err := c.readConfigMapYAML(ctx)
	if err != nil {
		return nil, nil
	}
	var cfg praxisTop
	if err := yaml.Unmarshal([]byte(raw), &cfg); err != nil {
		return nil, nil
	}

	seen := map[string]bool{}
	var result []ModelInfo
	for _, chain := range cfg.FilterChains {
		clusterMap := map[string]string{}
		for _, f := range chain.Filters {
			if f["filter"] != "router" {
				continue
			}
			var routes []routerRoute
			if err := remarshal(f["routes"], &routes); err == nil {
				for _, r := range routes {
					for _, v := range r.Headers {
						clusterMap[v] = r.Cluster
					}
				}
			}
		}
		for _, f := range chain.Filters {
			if f["filter"] != "model_catalog" {
				continue
			}
			var models []catalogModel
			if err := remarshal(f["models"], &models); err != nil {
				continue
			}
			for _, m := range models {
				if seen[m.ID] {
					continue
				}
				seen[m.ID] = true
				info := ModelInfo{
					Name:     m.ID,
					Provider: m.OwnedBy,
				}
				if cluster, ok := clusterMap[m.ID]; ok {
					info.ProviderRefs = []ProviderRef{{ProviderName: cluster, TargetModel: m.ID, Weight: 1}}
				}
				result = append(result, info)
			}
		}
	}
	return result, nil
}

func (c *Client) listProvidersFromConfig(ctx context.Context) ([]ProviderInfo, error) {
	raw, err := c.readConfigMapYAML(ctx)
	if err != nil {
		return nil, nil
	}
	var cfg praxisTop
	if err := yaml.Unmarshal([]byte(raw), &cfg); err != nil {
		return nil, nil
	}

	seen := map[string]bool{}
	var result []ProviderInfo
	for _, chain := range cfg.FilterChains {
		for _, f := range chain.Filters {
			if f["filter"] != "load_balancer" {
				continue
			}
			var clusters []lbCluster
			if err := remarshal(f["clusters"], &clusters); err != nil {
				continue
			}
			for _, cl := range clusters {
				if seen[cl.Name] {
					continue
				}
				seen[cl.Name] = true
				endpoint := ""
				if len(cl.Endpoints) > 0 {
					endpoint = cl.Endpoints[0]
				}
				authType := "none"
				if cl.TLS != nil && cl.TLS.SNI != "" {
					authType = "tls"
				}
				result = append(result, ProviderInfo{
					Name:     cl.Name,
					Provider: cl.Name,
					Endpoint: endpoint,
					Phase:    "Ready",
					AuthType: authType,
				})
			}
		}
	}
	return result, nil
}

func (c *Client) UpdateModelProvider(ctx context.Context, modelName string, providerName string, targetModel string, apiFormat string) error {
	model, err := c.client.Resource(c.modelGVR()).Namespace(c.namespace).Get(ctx, modelName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get %s %s: %w", c.cfg.ModelResource, modelName, err)
	}

	refs, found, _ := unstructured.NestedSlice(model.Object, "spec", "externalProviderRefs")
	if !found || len(refs) == 0 {
		return fmt.Errorf("model %s has no externalProviderRefs", modelName)
	}

	refMap, ok := refs[0].(map[string]interface{})
	if !ok {
		return fmt.Errorf("invalid externalProviderRefs format")
	}
	if nameRef, ok := refMap["ref"].(map[string]interface{}); ok {
		nameRef["name"] = providerName
	} else {
		refMap["ref"] = map[string]interface{}{"name": providerName}
	}
	refMap["targetModel"] = targetModel
	refMap["apiFormat"] = apiFormat
	refs[0] = refMap

	patch := map[string]interface{}{
		"spec": map[string]interface{}{
			"externalProviderRefs": refs,
		},
	}
	patchBytes, _ := json.Marshal(patch)

	_, err = c.client.Resource(c.modelGVR()).Namespace(c.namespace).Patch(
		ctx, modelName, types.MergePatchType, patchBytes, metav1.PatchOptions{})
	if err != nil {
		return err
	}

	slog.Info("ExternalModel provider updated", "model", modelName, "provider", providerName)
	return nil
}

var (
	authPolicyGVR = schema.GroupVersionResource{
		Group:    "maas.opendatahub.io",
		Version:  "v1alpha1",
		Resource: "maasauthpolicies",
	}
	subscriptionGVR = schema.GroupVersionResource{
		Group:    "maas.opendatahub.io",
		Version:  "v1alpha1",
		Resource: "maassubscriptions",
	}
)

type AuthPolicyInfo struct {
	Name      string   `json:"name"`
	Namespace string   `json:"namespace"`
	Groups    []string `json:"groups"`
	Users     []string `json:"users"`
	Models    []string `json:"models"`
	Phase     string   `json:"phase"`
}

type SubscriptionInfo struct {
	Name      string            `json:"name"`
	Namespace string            `json:"namespace"`
	Groups    []string          `json:"groups"`
	Users     []string          `json:"users"`
	Models    []ModelSubRefInfo `json:"models"`
	Priority  int64             `json:"priority"`
	Phase     string            `json:"phase"`
}

type ModelSubRefInfo struct {
	Name       string `json:"name"`
	TokenLimit int64  `json:"tokenLimit"`
}

func (c *Client) GetAuthPolicies(ctx context.Context, namespace string) ([]AuthPolicyInfo, error) {
	ns := namespace
	if ns == "" {
		ns = "models-as-a-service"
	}
	list, err := c.client.Resource(authPolicyGVR).Namespace(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list MaaSAuthPolicies: %w", err)
	}

	var result []AuthPolicyInfo
	for _, item := range list.Items {
		info := AuthPolicyInfo{
			Name:      item.GetName(),
			Namespace: item.GetNamespace(),
		}

		subjects, _, _ := unstructured.NestedMap(item.Object, "spec", "subjects")
		if groups, ok := subjects["groups"].([]interface{}); ok {
			for _, g := range groups {
				if gMap, ok := g.(map[string]interface{}); ok {
					if name, ok := gMap["name"].(string); ok {
						info.Groups = append(info.Groups, name)
					}
				}
			}
		}
		if users, ok := subjects["users"].([]interface{}); ok {
			for _, u := range users {
				if s, ok := u.(string); ok {
					info.Users = append(info.Users, s)
				}
			}
		}

		modelRefs, _, _ := unstructured.NestedSlice(item.Object, "spec", "modelRefs")
		for _, mr := range modelRefs {
			if m, ok := mr.(map[string]interface{}); ok {
				if name, ok := m["name"].(string); ok {
					info.Models = append(info.Models, name)
				}
			}
		}

		phase, _, _ := unstructured.NestedString(item.Object, "status", "phase")
		info.Phase = phase

		result = append(result, info)
	}
	return result, nil
}

func (c *Client) GetSubscriptions(ctx context.Context, namespace string) ([]SubscriptionInfo, error) {
	ns := namespace
	if ns == "" {
		ns = "models-as-a-service"
	}
	list, err := c.client.Resource(subscriptionGVR).Namespace(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list MaaSSubscriptions: %w", err)
	}

	var result []SubscriptionInfo
	for _, item := range list.Items {
		info := SubscriptionInfo{
			Name:      item.GetName(),
			Namespace: item.GetNamespace(),
		}

		owner, _, _ := unstructured.NestedMap(item.Object, "spec", "owner")
		if groups, ok := owner["groups"].([]interface{}); ok {
			for _, g := range groups {
				if gMap, ok := g.(map[string]interface{}); ok {
					if name, ok := gMap["name"].(string); ok {
						info.Groups = append(info.Groups, name)
					}
				}
			}
		}
		if users, ok := owner["users"].([]interface{}); ok {
			for _, u := range users {
				if s, ok := u.(string); ok {
					info.Users = append(info.Users, s)
				}
			}
		}

		modelRefs, _, _ := unstructured.NestedSlice(item.Object, "spec", "modelRefs")
		for _, mr := range modelRefs {
			if m, ok := mr.(map[string]interface{}); ok {
				mInfo := ModelSubRefInfo{}
				if name, ok := m["name"].(string); ok {
					mInfo.Name = name
				}
				if trl, ok := m["tokenRateLimit"].(map[string]interface{}); ok {
					if limit, ok := trl["tokensPerMinute"].(int64); ok {
						mInfo.TokenLimit = limit
					}
				}
				info.Models = append(info.Models, mInfo)
			}
		}

		priority, _, _ := unstructured.NestedInt64(item.Object, "spec", "priority")
		info.Priority = priority

		phase, _, _ := unstructured.NestedString(item.Object, "status", "phase")
		info.Phase = phase

		result = append(result, info)
	}
	return result, nil
}

var (
	openshiftGroupGVR = schema.GroupVersionResource{
		Group:    "user.openshift.io",
		Version:  "v1",
		Resource: "groups",
	}
)

type GroupInfo struct {
	Name    string   `json:"name"`
	Members []string `json:"members"`
}

func (c *Client) GetOpenShiftGroups(ctx context.Context) ([]GroupInfo, error) {
	list, err := c.client.Resource(openshiftGroupGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list OpenShift groups: %w", err)
	}

	var result []GroupInfo
	for _, item := range list.Items {
		info := GroupInfo{
			Name: item.GetName(),
		}
		users, _, _ := unstructured.NestedStringSlice(item.Object, "users")
		info.Members = users
		if info.Members == nil {
			info.Members = []string{}
		}
		result = append(result, info)
	}
	return result, nil
}

func (c *Client) AddUserToGroup(ctx context.Context, groupName, username string) error {
	group, err := c.client.Resource(openshiftGroupGVR).Get(ctx, groupName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get group %s: %w", groupName, err)
	}

	users, _, _ := unstructured.NestedStringSlice(group.Object, "users")
	for _, u := range users {
		if u == username {
			return nil // already in group
		}
	}
	users = append(users, username)
	unstructured.SetNestedStringSlice(group.Object, users, "users")

	_, err = c.client.Resource(openshiftGroupGVR).Update(ctx, group, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("update group %s: %w", groupName, err)
	}
	return nil
}

func (c *Client) RemoveUserFromGroup(ctx context.Context, groupName, username string) error {
	group, err := c.client.Resource(openshiftGroupGVR).Get(ctx, groupName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get group %s: %w", groupName, err)
	}

	users, _, _ := unstructured.NestedStringSlice(group.Object, "users")
	var filtered []string
	for _, u := range users {
		if u != username {
			filtered = append(filtered, u)
		}
	}
	unstructured.SetNestedStringSlice(group.Object, filtered, "users")

	_, err = c.client.Resource(openshiftGroupGVR).Update(ctx, group, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("update group %s: %w", groupName, err)
	}
	return nil
}

type OpenShiftUser struct {
	Name     string `json:"name"`
	GitHubID string `json:"githubId,omitempty"`
}

func (c *Client) GetOpenShiftUsers(ctx context.Context) ([]OpenShiftUser, error) {
	userGVR := schema.GroupVersionResource{Group: "user.openshift.io", Version: "v1", Resource: "users"}
	list, err := c.client.Resource(userGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	var users []OpenShiftUser
	for _, u := range list.Items {
		user := OpenShiftUser{Name: u.GetName()}
		identities, _, _ := unstructured.NestedStringSlice(u.Object, "identities")
		for _, id := range identities {
			if strings.HasPrefix(id, "github:") {
				user.GitHubID = strings.TrimPrefix(id, "github:")
				break
			}
		}
		users = append(users, user)
	}
	return users, nil
}

func (c *Client) secretExists(ctx context.Context, name string) bool {
	if name == "" {
		return false
	}
	coreGVR := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "secrets"}
	_, err := c.client.Resource(coreGVR).Namespace(c.namespace).Get(ctx, name, metav1.GetOptions{})
	return err == nil
}

// GetUserGroups resolves a user's group membership from a cluster-scoped
// resource whose objects carry a `users` list. Returns nothing when no
// group resource is configured.
func (c *Client) GetUserGroups(username string) ([]string, error) {
	if c.cfg.GroupGroup == "" {
		return nil, nil
	}

	groupGVR := schema.GroupVersionResource{
		Group:    c.cfg.GroupGroup,
		Version:  c.cfg.GroupVersion,
		Resource: c.cfg.GroupResource,
	}
	list, err := c.client.Resource(groupGVR).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	var result []string
	for _, item := range list.Items {
		users, _, _ := unstructured.NestedStringSlice(item.Object, "users")
		for _, u := range users {
			if u == username {
				result = append(result, item.GetName())
				break
			}
		}
	}
	return result, nil
}
