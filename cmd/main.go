package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/noyitz/ai-gateway-metering-service/internal/handler"
	"github.com/noyitz/ai-gateway-metering-service/internal/k8s"
	"github.com/noyitz/ai-gateway-metering-service/internal/storage"
)

func main() {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		slog.Error("DATABASE_URL is required")
		os.Exit(1)
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	store, err := storage.New(databaseURL)
	if err != nil {
		slog.Error("failed to connect to database", "error", err)
		os.Exit(1)
	}
	defer store.Close()

	eventsHandler := handler.NewEventsHandler(store)
	entitlementsHandler := handler.NewEntitlementsHandler(store)
	teamUsageHandler := handler.NewTeamUsageHandler(store)
	dashboardHandler := handler.NewDashboardHandler(store)

	// K8s client for admin API (optional — fails gracefully if not in cluster)
	k8sNamespace := os.Getenv("K8S_NAMESPACE")
	if k8sNamespace == "" {
		k8sNamespace = "llm"
	}
	var adminHandler *handler.AdminHandler
	k8sClient, err := k8s.NewClient(k8sNamespace)
	if err != nil {
		slog.Warn("k8s client not available — admin API will return empty data", "error", err)
		adminHandler = handler.NewAdminHandler(nil)
	} else {
		adminHandler = handler.NewAdminHandler(k8sClient)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/events", eventsHandler.HandleEvent)
	mux.HandleFunc("/api/v1/customers/", entitlementsHandler.HandleEntitlement)
	mux.HandleFunc("/api/v1/team-usage", teamUsageHandler.HandleTeamUsage)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/dashboard", http.StatusFound)
			return
		}
		http.NotFound(w, r)
	})
	mux.HandleFunc("/dashboard", dashboardHandler.ServeDashboard)
	mux.HandleFunc("/api/v1/dashboard/overview", dashboardHandler.HandleOverview)
	mux.HandleFunc("/api/v1/dashboard/groups", dashboardHandler.HandleGroups)
	mux.HandleFunc("/api/v1/dashboard/users", dashboardHandler.HandleUsers)
	mux.HandleFunc("/api/v1/dashboard/models", dashboardHandler.HandleModels)
	mux.HandleFunc("/api/v1/dashboard/timeline", dashboardHandler.HandleTimeline)
	mux.HandleFunc("/api/v1/dashboard/recent", dashboardHandler.HandleRecent)
	mux.HandleFunc("/admin", adminHandler.ServeAdmin)
	mux.HandleFunc("/routing", adminHandler.ServeRouting)
	mux.HandleFunc("/admin2", adminHandler.ServeRouting)
	mux.HandleFunc("/compression", adminHandler.ServeCompression)
	mux.HandleFunc("/api/v1/admin/providers", adminHandler.HandleProviders)
	mux.HandleFunc("/api/v1/admin/models", adminHandler.HandleModels)
	mux.HandleFunc("/api/v1/admin/models/", adminHandler.HandleUpdateWeights)
	mux.HandleFunc("/api/v1/admin/config", adminHandler.HandleConfig)
	mux.HandleFunc("/api/v1/admin/models/provider/", adminHandler.HandleUpdateProvider)
	mux.HandleFunc("/api/v1/whoami", func(w http.ResponseWriter, r *http.Request) {
		user := r.Header.Get("X-Forwarded-User")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"user": user})
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/ready", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	server := &http.Server{Addr: ":" + port, Handler: mux}

	go func() {
		slog.Info("metering service starting", "port", port)
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
