package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/noyitz/ai-gateway-metering-service/internal/config"
	"github.com/noyitz/ai-gateway-metering-service/internal/handler"
	"github.com/noyitz/ai-gateway-metering-service/internal/k8s"
	"github.com/noyitz/ai-gateway-metering-service/internal/maasapi"
	"github.com/noyitz/ai-gateway-metering-service/internal/pricing"
	"github.com/noyitz/ai-gateway-metering-service/internal/storage"
)

func main() {
	cfg := config.Load()
	if cfg.DatabaseURL == "" {
		slog.Error("DATABASE_URL is required")
		os.Exit(1)
	}

	// MonthlyTokenQuota is the per-user monthly token budget the entitlement
	// endpoint reports against. Enforcement of actual traffic belongs in the
	// gateway (praxis-proxy/ai#121); here it only shapes the reported balance.
	store, err := storage.New(cfg.DatabaseURL, int64(cfg.MonthlyTokenQuota))
	if err != nil {
		slog.Error("failed to connect to database", "error", err)
		os.Exit(1)
	}
	defer store.Close()

	// Seed model pricing from LiteLLM (try fetch latest, fall back to bundled)
	ctx := context.Background()
	litellmPrices, pricingSource := pricing.LoadPrices(ctx)
	if len(litellmPrices) > 0 {
		storePrices := make([]storage.ModelPrice, len(litellmPrices))
		for i, p := range litellmPrices {
			storePrices[i] = storage.ModelPrice{
				Model: p.Model, Provider: p.Provider,
				InputCost: p.InputCost, OutputCost: p.OutputCost,
				CacheWriteCost: p.CacheWriteCost, CacheReadCost: p.CacheReadCost,
			}
		}
		updated, seedErr := store.SeedPricing(ctx, storePrices)
		if seedErr != nil {
			slog.Warn("pricing seed failed — dashboard costs may be stale", "error", seedErr)
		} else {
			slog.Info("model pricing seeded", "models", len(storePrices), "updated", updated, "source", pricingSource)
		}
	}

	// Seed self-hosted ("hosted") model pricing at OpenRouter parity. These
	// are not in LiteLLM's catalog, so they must be seeded independently —
	// even when the LiteLLM load above fails — or the cost query reprices
	// hosted traffic at the paid default. Seeded last so these rows always
	// win the upsert.
	localPrices := pricing.LocalPrices()
	storeLocal := make([]storage.ModelPrice, len(localPrices))
	for i, p := range localPrices {
		storeLocal[i] = storage.ModelPrice{
			Model: p.Model, Provider: p.Provider,
			InputCost: p.InputCost, OutputCost: p.OutputCost,
			CacheWriteCost: p.CacheWriteCost, CacheReadCost: p.CacheReadCost,
		}
	}
	if updated, seedErr := store.SeedPricing(ctx, storeLocal); seedErr != nil {
		slog.Warn("local pricing seed failed — hosted models may reprice at the paid default", "error", seedErr)
	} else {
		slog.Info("local model pricing seeded", "models", len(storeLocal), "updated", updated)
	}

	// Vendor list prices (for the dashboard's cost-saved column) — a second
	// rate set on the same rows, seeded independently. Best-effort: a failed
	// fetch leaves previously seeded list prices in place (SeedListPricing is
	// UPDATE-only and skips zero rates), so the column goes stale, not wrong.
	if listPrices, listErr := pricing.LoadListPrices(ctx); listErr != nil {
		slog.Warn("list pricing fetch failed — cost-saved column may be stale", "error", listErr)
	} else {
		listSeed := make([]storage.ModelPrice, 0, len(listPrices)+len(pricing.LocalListPrices()))
		for _, p := range listPrices {
			listSeed = append(listSeed, storage.ModelPrice{
				Model: p.Model, Provider: p.Provider,
				ListInputCost: p.ListInputCost, ListOutputCost: p.ListOutputCost,
				ListCacheWriteCost: p.ListCacheWriteCost, ListCacheReadCost: p.ListCacheReadCost,
			})
		}
		for _, p := range pricing.LocalListPrices() {
			listSeed = append(listSeed, storage.ModelPrice{
				Model: p.Model, Provider: p.Provider,
				ListInputCost: p.ListInputCost, ListOutputCost: p.ListOutputCost,
				ListCacheWriteCost: p.ListCacheWriteCost, ListCacheReadCost: p.ListCacheReadCost,
			})
		}
		if updated, seedErr := store.SeedListPricing(ctx, listSeed); seedErr != nil {
			slog.Warn("list pricing seed failed — cost-saved column may be stale", "error", seedErr)
		} else {
			slog.Info("list pricing seeded", "models", len(listSeed), "updated", updated)
		}
	}

	eventsHandler := handler.NewEventsHandler(store)
	entitlementsHandler := handler.NewEntitlementsHandler(store)
	dashboardHandler := handler.NewDashboardHandler(store, cfg)

	// Kubernetes adapter — optional. It stays disabled until model/provider
	// CRD coordinates are configured, and even when enabled it degrades
	// gracefully to empty data if the cluster is unreachable, so the service
	// runs anywhere the CloudEvents contract holds.
	var k8sClient *k8s.Client
	if cfg.Kubernetes.Enabled() {
		k8sClient, err = k8s.NewClient(cfg.Kubernetes)
		if err != nil {
			slog.Warn("kubernetes adapter unavailable — admin API will return empty data", "error", err)
			k8sClient = nil
		}
	} else {
		slog.Info("kubernetes adapter disabled — set MODEL_CRD_GROUP/PROVIDER_CRD_GROUP to enable")
	}

	// maas-api client for key management (create/list/revoke). The base URL
	// defaults to the same maas-api the login flow validates against, so a
	// fresh cluster needs no extra configuration.
	maasAPIURL := os.Getenv("MAAS_API_URL")
	if maasAPIURL == "" {
		validateURL := os.Getenv("MAAS_VALIDATE_URL")
		if validateURL == "" {
			validateURL = "http://maas-api:8080/internal/v1/api-keys/validate"
		}
		maasAPIURL = strings.TrimSuffix(validateURL, "/internal/v1/api-keys/validate")
	}
	maasTenant := os.Getenv("MAAS_TENANT")
	if maasTenant == "" {
		maasTenant = "models-as-a-service"
	}
	maasClient := maasapi.NewClient(maasAPIURL, maasTenant)

	adminHandler := handler.NewAdminHandler(k8sClient, maasClient, cfg)
	authHandler := handler.NewAuthHandler(cfg)
	keysHandler := handler.NewKeysHandler(k8sClient, cfg, store)
	profilesHandler := handler.NewProfilesHandler(store)
	orgHandler := handler.NewOrgHandler(store, cfg, maasClient)
	auth := authHandler.RequireAuth

	mux := http.NewServeMux()

	// Machine-to-machine APIs — no session required
	mux.HandleFunc("/api/v1/events", eventsHandler.HandleEvent)
	mux.HandleFunc("/api/v1/customers/", entitlementsHandler.HandleEntitlement)
	// /api/v1/team-usage was REMOVED on purpose: it sat outside auth, took
	// the group from the query string, and defaulted to a hard-coded team.
	// Its replacement is /api/v1/org/usage below, which is authenticated
	// and scope-enforced. Do not re-add a sibling.

	// Auth endpoints — unauthenticated by definition
	mux.HandleFunc("/login", authHandler.HandleLogin)
	mux.HandleFunc("/logout", authHandler.HandleLogout)
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/ready", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	// Public onboarding page — no session by design: it is the link you
	// send someone before they have a key. Its gateway URLs are substituted
	// at serve time from WELCOME_*_URL env vars, so the repo template stays
	// host-free. Root sends newcomers here; signed-in users navigate on.
	mux.HandleFunc("/welcome", dashboardHandler.ServeWelcome)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/welcome", http.StatusFound)
			return
		}
		http.NotFound(w, r)
	})

	// User pages — session required. /whoami backs both the legacy pages and
	// the redesigned user dashboard (name, groups, admin flag, impersonation).
	mux.HandleFunc("/me", auth(adminHandler.ServeMyAccount))
	mux.HandleFunc("/me/keys", auth(keysHandler.HandleKeys))
	mux.HandleFunc("/me/keys/", auth(keysHandler.HandleKeys))
	mux.HandleFunc("/me/whoami", auth(keysHandler.HandleWhoAmI))
	mux.HandleFunc("/api/v1/whoami", auth(keysHandler.HandleWhoAmI))
	mux.HandleFunc("/whoami", auth(keysHandler.HandleWhoAmI))

	// Dashboard — session required, per-user scoping in handlers
	mux.HandleFunc("/dashboard", auth(dashboardHandler.ServeDashboard))
	mux.HandleFunc("/api/v1/dashboard/overview", auth(dashboardHandler.HandleOverview))
	mux.HandleFunc("/api/v1/dashboard/groups", auth(dashboardHandler.HandleGroups))
	mux.HandleFunc("/api/v1/dashboard/users", auth(dashboardHandler.HandleUsers))
	mux.HandleFunc("/api/v1/dashboard/models", auth(dashboardHandler.HandleModels))
	mux.HandleFunc("/api/v1/dashboard/timeline", auth(dashboardHandler.HandleTimeline))
	mux.HandleFunc("/api/v1/dashboard/recent", auth(dashboardHandler.HandleRecent))

	// Pricing modal rate card — readable by any logged-in user: the rates
	// are the same numbers every request is billed at, no user data.
	mux.HandleFunc("/api/v1/pricing", auth(handler.NewPricingRefreshHandler(store).HandleList))

	// Admin pages — session + admin required
	mux.HandleFunc("/admin", auth(handler.RequireAdmin(cfg, adminHandler.ServeAdmin)))
	mux.HandleFunc("/routing", auth(adminHandler.ServeRouting))
	mux.HandleFunc("/admin2", auth(adminHandler.ServeRouting))
	mux.HandleFunc("/compression", auth(adminHandler.ServeCompression))
	// Admin APIs are gated by RequireAdmin (auth() alone is not enough —
	// otherwise any signed-in user could change weights/config).
	mux.HandleFunc("/api/v1/admin/providers", auth(handler.RequireAdmin(cfg, adminHandler.HandleProviders)))
	mux.HandleFunc("/api/v1/admin/models", auth(handler.RequireAdmin(cfg, adminHandler.HandleModels)))
	mux.HandleFunc("/api/v1/admin/models/", auth(handler.RequireAdmin(cfg, adminHandler.HandleUpdateWeights)))
	mux.HandleFunc("/api/v1/admin/config", auth(handler.RequireAdmin(cfg, adminHandler.HandleConfig)))
	mux.HandleFunc("/api/v1/admin/models/provider/", auth(handler.RequireAdmin(cfg, adminHandler.HandleUpdateProvider)))
	mux.HandleFunc("/api/v1/admin/users", auth(handler.RequireAdmin(cfg, profilesHandler.HandleProfiles)))
	mux.HandleFunc("/api/v1/admin/pricing/refresh", auth(handler.NewPricingRefreshHandler(store).HandleRefresh))
	// OpenShift users/groups/entitlements management (redesigned admin page).
	// Display-name profiles stay on /api/v1/admin/users above.
	mux.HandleFunc("/api/v1/admin/openshift-users", auth(handler.RequireAdmin(cfg, adminHandler.HandleUsers)))
	mux.HandleFunc("/api/v1/admin/group-member", auth(handler.RequireAdmin(cfg, adminHandler.HandleGroupMember)))
	mux.HandleFunc("/api/v1/admin/auth-policies", auth(handler.RequireAdmin(cfg, adminHandler.HandleAuthPolicies)))
	mux.HandleFunc("/api/v1/admin/subscriptions", auth(handler.RequireAdmin(cfg, adminHandler.HandleSubscriptions)))
	// Group + key APIs are reachable by any signed-in user: the redesigned
	// user dashboard lists its own group membership and manages the caller's
	// own keys. The handlers scope non-admins to their own identity, so a
	// regular user can only ever see or create their own keys.
	mux.HandleFunc("/api/v1/admin/groups", auth(adminHandler.HandleGroups))
	mux.HandleFunc("/api/v1/admin/keys", auth(adminHandler.HandleKeys))
	mux.HandleFunc("/api/v1/admin/keys/", auth(adminHandler.HandleKeys))
	// Admin "view as user" — the handler checks admin against the real
	// session identity, not the swapped header, so it also clears itself.
	mux.HandleFunc("/admin/impersonate", auth(authHandler.HandleImpersonate))

	// Manager view — session required; the page adapts to the caller's
	// scope (plain user sees self, manager sees subtree, admin sees all).
	mux.HandleFunc("/manager", auth(orgHandler.ServeManager))

	// Org APIs reachable by any signed-in user; every handler enforces the
	// caller's scope internally (a manager asking for a tree or usage
	// outside their subtree gets a 403).
	mux.HandleFunc("/api/v1/org/scope", auth(orgHandler.HandleScope))
	mux.HandleFunc("/api/v1/org/tree", auth(orgHandler.HandleOrgTree))
	mux.HandleFunc("/api/v1/org/usage", auth(orgHandler.HandleOrgUsage))

	// Directory administration — admin only.
	mux.HandleFunc("/api/v1/admin/people", auth(handler.RequireAdmin(cfg, orgHandler.HandlePeople)))
	mux.HandleFunc("/api/v1/admin/people/", auth(handler.RequireAdmin(cfg, orgHandler.HandlePerson)))
	mux.HandleFunc("/api/v1/admin/identities", auth(handler.RequireAdmin(cfg, orgHandler.HandleIdentities)))
	mux.HandleFunc("/api/v1/admin/org/import", auth(handler.RequireAdmin(cfg, orgHandler.HandleImport)))
	mux.HandleFunc("/api/v1/admin/keys/invites", auth(handler.RequireAdmin(cfg, orgHandler.HandleInvites)))

	// Key invite claim — unauthenticated by design: the single-use, expiring
	// token in the URL is the credential (only its SHA-256 is stored, and
	// the key is minted at claim time in the claimant's browser).
	mux.HandleFunc("/invite/", orgHandler.HandleClaim)

	server := &http.Server{Addr: ":" + cfg.Port, Handler: mux}

	go func() {
		slog.Info("metering service starting", "port", cfg.Port)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	<-stop

	slog.Info("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	server.Shutdown(ctx)
}
