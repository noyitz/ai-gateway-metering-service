package k8s

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

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

	var raw struct {
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
	if err := yaml.Unmarshal([]byte(configYAML), &raw); err != nil {
		return nil, fmt.Errorf("parse pipeline config YAML: %w", err)
	}

	result := &PipelineConfig{ActiveProfile: "default"}
	for _, p := range raw.Profiles {
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
