// Package config resolves runtime settings from the environment.
//
// Only DATABASE_URL is required. Every other setting has a working
// default, and every optional integration stays switched off until it is
// explicitly configured, so the service runs unmodified against any
// gateway that speaks the CloudEvents contract.
package config

import (
	"os"
	"strconv"
	"strings"
)

// DefaultMonthlyTokenQuota is the per-user monthly token allowance applied
// when MONTHLY_TOKEN_QUOTA is unset.
const DefaultMonthlyTokenQuota = 100_000_000

// Config is the resolved runtime configuration.
type Config struct {
	// DatabaseURL is the PostgreSQL connection string. Required.
	DatabaseURL string

	// Port is the HTTP listen port.
	Port string

	// MonthlyTokenQuota caps per-user monthly token usage. Balance checks
	// report no access once a user exceeds it.
	MonthlyTokenQuota float64

	// EventSource labels ingested events that arrive without a
	// CloudEvents `source` attribute.
	EventSource string

	// UserHeader and GroupsHeader name the request headers an
	// authenticating proxy uses to forward the caller's identity.
	UserHeader   string
	GroupsHeader string

	// AdminUsers may reach the admin console and the routing pages.
	AdminUsers []string

	// AllowUnauthenticatedAdmin grants admin access when no identity
	// header is present. Convenient for local development, unsafe once
	// the service is exposed.
	AllowUnauthenticatedAdmin bool

	// DefaultGroup is attributed to callers whose group cannot be
	// resolved from a header or from the Kubernetes adapter.
	DefaultGroup string

	KeyService KeyService
	Kubernetes Kubernetes
}

// KeyService describes an optional upstream that issues and revokes API
// keys on behalf of the signed-in user. Disabled unless URL is set.
type KeyService struct {
	// URL is the base URL of the key-issuing API.
	URL string

	// UserHeader, GroupsHeader, and TenantHeader name the headers used to
	// forward caller identity upstream.
	UserHeader   string
	GroupsHeader string
	TenantHeader string

	// Tenant is sent in TenantHeader on every upstream call.
	Tenant string

	// InsecureSkipVerify disables TLS verification against the upstream.
	// Intended for clusters using self-signed internal certificates.
	InsecureSkipVerify bool
}

// Enabled reports whether the key-service proxy should be served.
func (k KeyService) Enabled() bool { return k.URL != "" }

// Kubernetes describes an optional adapter that reads a model catalogue
// from custom resources. The resource coordinates are configurable
// because every gateway models its catalogue differently; the adapter
// stays disabled until at least one group is supplied.
type Kubernetes struct {
	// Namespace holds the model and provider custom resources.
	Namespace string

	// Model and Provider custom resource coordinates.
	ModelGroup       string
	ModelVersion     string
	ModelResource    string
	ProviderGroup    string
	ProviderVersion  string
	ProviderResource string

	// ModelFallbackGroup is consulted when the primary model resource
	// returns nothing, which helps during a catalogue migration between
	// two API groups. Optional.
	ModelFallbackGroup string

	// GroupGroup, GroupVersion, and GroupResource locate a cluster-scoped
	// resource that maps users to groups. Optional.
	GroupGroup    string
	GroupVersion  string
	GroupResource string

	// PipelineConfigMap names a ConfigMap describing the gateway's filter
	// pipeline, surfaced read-only in the admin console. Optional.
	PipelineConfigMap          string
	PipelineConfigMapNamespace string
	PipelineConfigMapKey       string
}

// Enabled reports whether the Kubernetes adapter should be initialised.
func (k Kubernetes) Enabled() bool {
	return k.ModelGroup != "" || k.ProviderGroup != ""
}

// Load resolves configuration from the environment.
func Load() Config {
	return Config{
		DatabaseURL:               os.Getenv("DATABASE_URL"),
		Port:                      envDefault("PORT", "8080"),
		MonthlyTokenQuota:         envFloat("MONTHLY_TOKEN_QUOTA", DefaultMonthlyTokenQuota),
		EventSource:               envDefault("EVENT_SOURCE", "ai-gateway"),
		UserHeader:                envDefault("AUTH_USER_HEADER", "X-Forwarded-User"),
		GroupsHeader:              envDefault("AUTH_GROUPS_HEADER", "X-Forwarded-Groups"),
		AdminUsers:                envList("ADMIN_USERS"),
		AllowUnauthenticatedAdmin: envBool("ALLOW_UNAUTHENTICATED_ADMIN", true),
		DefaultGroup:              envDefault("DEFAULT_GROUP", "default"),
		KeyService: KeyService{
			URL:                strings.TrimSuffix(os.Getenv("KEY_SERVICE_URL"), "/"),
			UserHeader:         envDefault("KEY_SERVICE_USER_HEADER", "X-Auth-Username"),
			GroupsHeader:       envDefault("KEY_SERVICE_GROUPS_HEADER", "X-Auth-Groups"),
			TenantHeader:       envDefault("KEY_SERVICE_TENANT_HEADER", "X-Auth-Tenant"),
			Tenant:             os.Getenv("KEY_SERVICE_TENANT"),
			InsecureSkipVerify: envBool("KEY_SERVICE_INSECURE_SKIP_VERIFY", false),
		},
		Kubernetes: Kubernetes{
			Namespace:                  envDefault("K8S_NAMESPACE", "default"),
			ModelGroup:                 os.Getenv("MODEL_CRD_GROUP"),
			ModelVersion:               envDefault("MODEL_CRD_VERSION", "v1alpha1"),
			ModelResource:              envDefault("MODEL_CRD_RESOURCE", "externalmodels"),
			ProviderGroup:              os.Getenv("PROVIDER_CRD_GROUP"),
			ProviderVersion:            envDefault("PROVIDER_CRD_VERSION", "v1alpha1"),
			ProviderResource:           envDefault("PROVIDER_CRD_RESOURCE", "externalproviders"),
			ModelFallbackGroup:         os.Getenv("MODEL_CRD_FALLBACK_GROUP"),
			GroupGroup:                 os.Getenv("GROUP_CRD_GROUP"),
			GroupVersion:               envDefault("GROUP_CRD_VERSION", "v1"),
			GroupResource:              envDefault("GROUP_CRD_RESOURCE", "groups"),
			PipelineConfigMap:          os.Getenv("PIPELINE_CONFIGMAP"),
			PipelineConfigMapNamespace: os.Getenv("PIPELINE_CONFIGMAP_NAMESPACE"),
			PipelineConfigMapKey:       envDefault("PIPELINE_CONFIGMAP_KEY", "config.yaml"),
		},
	}
}

func envDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envList(key string) []string {
	raw := os.Getenv(key)
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	result := make([]string, 0, len(parts))
	for _, p := range parts {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

func envBool(key string, fallback bool) bool {
	v, err := strconv.ParseBool(os.Getenv(key))
	if err != nil {
		return fallback
	}
	return v
}

func envFloat(key string, fallback float64) float64 {
	v, err := strconv.ParseFloat(os.Getenv(key), 64)
	if err != nil || v <= 0 {
		return fallback
	}
	return v
}
