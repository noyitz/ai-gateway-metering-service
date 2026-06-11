package k8s

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

var (
	providerGVR = schema.GroupVersionResource{
		Group:    "inference.opendatahub.io",
		Version:  "v1alpha1",
		Resource: "externalproviders",
	}
	modelGVR = schema.GroupVersionResource{
		Group:    "inference.opendatahub.io",
		Version:  "v1alpha1",
		Resource: "externalmodels",
	}
	legacyModelGVR = schema.GroupVersionResource{
		Group:    "maas.opendatahub.io",
		Version:  "v1alpha1",
		Resource: "externalmodels",
	}
)

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
}

func NewClient(namespace string) (*Client, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to get in-cluster config: %w", err)
	}

	dynClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create dynamic client: %w", err)
	}

	slog.Info("k8s client initialized", "namespace", namespace)
	return &Client{client: dynClient, namespace: namespace}, nil
}

func (c *Client) ListProviders(ctx context.Context) ([]ProviderInfo, error) {
	list, err := c.client.Resource(providerGVR).Namespace(c.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list ExternalProviders: %w", err)
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
	// Try new CRD first, fall back to legacy if empty or error
	list, err := c.client.Resource(modelGVR).Namespace(c.namespace).List(ctx, metav1.ListOptions{})
	if err != nil || len(list.Items) == 0 {
		list, err = c.client.Resource(legacyModelGVR).Namespace(c.namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("list ExternalModels: %w", err)
		}
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
	model, err := c.client.Resource(legacyModelGVR).Namespace(c.namespace).Get(ctx, modelName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get ExternalModel %s: %w", modelName, err)
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

	_, err = c.client.Resource(legacyModelGVR).Namespace(c.namespace).Patch(
		ctx, modelName, types.MergePatchType, patchBytes, metav1.PatchOptions{})
	return err
}

func (c *Client) secretExists(ctx context.Context, name string) bool {
	if name == "" {
		return false
	}
	coreGVR := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "secrets"}
	_, err := c.client.Resource(coreGVR).Namespace(c.namespace).Get(ctx, name, metav1.GetOptions{})
	return err == nil
}
