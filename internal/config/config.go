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
	"unicode"
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

	// AdminUsers may see the org-wide Usage view (they see everyone's
	// usage on the dashboard, not just their own). They do NOT get the
	// Admin console, Routing, or Compression pages — those are
	// SuperAdmin-only. Most admins only ever want the usage page.
	AdminUsers []string

	// SuperAdminUsers may reach the admin console, the routing pages, the
	// compression page, and every admin-gated API. Membership in this list
	// implies AdminUsers. The narrow blast radius is deliberate: only the
	// gateway operators themselves should mutate platform state.
	SuperAdminUsers []string

	// AllowUnauthenticatedAdmin grants admin access when no identity
	// header is present. Convenient for local development, unsafe once
	// the service is exposed.
	AllowUnauthenticatedAdmin bool

	// DefaultGroup is attributed to callers whose group cannot be
	// resolved from a header or from the Kubernetes adapter.
	DefaultGroup string

	// OrgInviteTTLHours bounds a key invite link: a single-use token that
	// nobody opened is worthless once it expires.
	OrgInviteTTLHours int

	// KeyRotationOverlapDays is how long the old key stays valid after a
	// rotation, so nobody's editor dies mid-task. 0 means revoke-on-rotate
	// (use for a suspected leak).
	KeyRotationOverlapDays int

	// DashboardCacheTTLSeconds bounds staleness of cached dashboard
	// responses. It must stay ABOVE the 30s client poll — a shorter TTL
	// makes every poll arrive after its key expired and the cache misses
	// nearly all steady-state traffic. The middleware warns at load.
	DashboardCacheTTLSeconds int

	// DashboardCacheEnabled gates the dashboard response cache. Default
	// OFF: a new build shipping and caching going live are separate,
	// deliberate events — enable per deployment and use cache-stats hit
	// ratio as the go/no-go. Disabling is the one-knob rollback.
	DashboardCacheEnabled bool

	// ReadDatabaseURL is an optional second pool for dashboard/report
	// reads only (Phase 2 of docs/dashboard-scaling-plan.md) — point it
	// at the CNPG read service. Empty (default) keeps every read on the
	// primary. Enforcement and quota reads ignore this pool by design;
	// see the readDB allowlist in internal/storage/postgres.go.
	ReadDatabaseURL string

	// DashboardUseRollups switches the dashboard/report aggregation reads
	// from raw usage_events to the hourly usage_hourly rollup (Phase 3
	// part B). Default OFF — same two-decisions rule as the cache flag:
	// deploying the code and enabling the behavior are separate events,
	// and the go signal is a green parity report. The read path only
	// honors this while rollups are backfilled AND the standing parity
	// check is green; either failing serves raw (see rollups.go).
	// Enforcement, quota and the Recent feed never read rollups.
	DashboardUseRollups bool

	// RollupRefreshSeconds is how often the maintenance loop refreshes
	// recent hours from raw and runs the standing parity check. Bounds
	// the self-heal window for the (locked) refresh-vs-insert race and
	// the standing-parity detection latency.
	RollupRefreshSeconds int

	KeyService KeyService
	Kubernetes Kubernetes
	Welcome    Welcome
}

// Welcome holds the public gateway endpoints surfaced on the
// unauthenticated /welcome onboarding page. The URLs are injected into the
// page at serve time from the deployment environment instead of being
// written into the template, so no cluster-specific host ever lands in the
// repository.
type Welcome struct {
	// UnifiedURL is the Anthropic-dialect gateway base URL (no /v1 suffix).
	UnifiedURL string
	// OpenAIURL is the OpenAI-dialect gateway base URL (ends in /v1).
	OpenAIURL string
	// DashboardURL is this service's public base URL, as users reach it.
	DashboardURL string
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

	// SubscriptionName names the MaaSSubscription CR (in Namespace) whose
	// spec.owner.groups is the canonical, live list of valid org groups.
	SubscriptionName string
}

// Enabled reports whether the Kubernetes adapter should be initialised.
func (k Kubernetes) Enabled() bool {
	return k.ModelGroup != "" || k.ProviderGroup != "" || k.PipelineConfigMap != ""
}

// Load resolves configuration from the environment.
func Load() Config {
	return Config{
		DatabaseURL:       os.Getenv("DATABASE_URL"),
		Port:              envDefault("PORT", "8080"),
		MonthlyTokenQuota: envFloat("MONTHLY_TOKEN_QUOTA", DefaultMonthlyTokenQuota),
		EventSource:       envDefault("EVENT_SOURCE", "ai-gateway"),
		UserHeader:        envDefault("AUTH_USER_HEADER", "X-Forwarded-User"),
		GroupsHeader:      envDefault("AUTH_GROUPS_HEADER", "X-Forwarded-Groups"),
		AdminUsers:        envList("ADMIN_USERS"),
		// The gateway operators. Set via the SUPERADMIN_USERS deployment
		// env var (comma/space separated); empty means no one holds the
		// super-admin surfaces, so the deployment must set it explicitly.
		SuperAdminUsers: envList("SUPERADMIN_USERS"),
		// Default false: since the org/manager feature this service decides
		// who may see whose spend, so an anonymous caller is nobody. Set
		// ALLOW_UNAUTHENTICATED_ADMIN=true deliberately for local
		// development only.
		AllowUnauthenticatedAdmin: envBool("ALLOW_UNAUTHENTICATED_ADMIN", false),
		DefaultGroup:              envDefault("DEFAULT_GROUP", "default"),
		OrgInviteTTLHours:         envInt("ORG_INVITE_TTL_HOURS", 72),
		KeyRotationOverlapDays:    envInt("KEY_ROTATION_OVERLAP_DAYS", 7),
		// Off by default (PR #19 review): shipping the build must not be
		// the same event as turning caching on. Enable per deployment with
		// DASHBOARD_CACHE_ENABLED=true and watch /api/v1/admin/cache-stats
		// — the hit ratio is the go/no-go, and disabling is a one-knob
		// redeploy.
		DashboardCacheTTLSeconds: envInt("DASHBOARD_CACHE_TTL_SECONDS", 60),
		DashboardCacheEnabled:    envBool("DASHBOARD_CACHE_ENABLED", false),
		ReadDatabaseURL:          os.Getenv("READ_DATABASE_URL"),
		DashboardUseRollups:      envBool("DASHBOARD_USE_ROLLUPS", false),
		RollupRefreshSeconds:     envInt("ROLLUP_REFRESH_SECONDS", 300),
		KeyService: KeyService{
			URL:                strings.TrimSuffix(os.Getenv("KEY_SERVICE_URL"), "/"),
			UserHeader:         envDefault("KEY_SERVICE_USER_HEADER", "X-Auth-Username"),
			GroupsHeader:       envDefault("KEY_SERVICE_GROUPS_HEADER", "X-Auth-Groups"),
			TenantHeader:       envDefault("KEY_SERVICE_TENANT_HEADER", "X-Auth-Tenant"),
			Tenant:             os.Getenv("KEY_SERVICE_TENANT"),
			InsecureSkipVerify: envBool("KEY_SERVICE_INSECURE_SKIP_VERIFY", false),
		},
		Welcome: Welcome{
			UnifiedURL:   strings.TrimSuffix(os.Getenv("WELCOME_UNIFIED_URL"), "/"),
			OpenAIURL:    strings.TrimSuffix(os.Getenv("WELCOME_OPENAI_URL"), "/"),
			DashboardURL: strings.TrimSuffix(os.Getenv("WELCOME_DASHBOARD_URL"), "/"),
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
			SubscriptionName:           envDefault("MAAS_SUBSCRIPTION_NAME", "dogfood-team"),
		},
	}
}

func envDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// envList parses a user/identity list from an env var. Entries may be
// separated by commas, whitespace, or any mix of the two — the deployment
// manifests use both (ADMIN_USERS is comma-separated, SUPERADMIN_USERS is
// space-separated), and a list that only splits on commas silently collapses
// the space-separated form into one bogus entry that never matches a caller.
func envList(key string) []string {
	raw := os.Getenv(key)
	if raw == "" {
		return nil
	}
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || unicode.IsSpace(r)
	})
	var result []string
	for _, p := range parts {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

func envInt(key string, fallback int) int {
	if v, err := strconv.Atoi(os.Getenv(key)); err == nil && v >= 0 {
		return v
	}
	return fallback
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
