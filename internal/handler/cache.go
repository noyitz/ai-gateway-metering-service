package handler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/hashicorp/golang-lru/v2/expirable"
	"golang.org/x/sync/singleflight"

	"github.com/noyitz/ai-gateway-metering-service/internal/config"
	"github.com/noyitz/ai-gateway-metering-service/internal/storage"
)

// Dashboard response cache (Phase 1 of docs/dashboard-scaling-plan.md).
//
// The 6 /api/v1/dashboard/* endpoints re-aggregate raw usage_events on
// every call, and every open dashboard tab re-polls them every 30s. This
// middleware collapses that fan-out: one computation per distinct
// (endpoint, params, resolved-scope) key per TTL, everything else served
// from a bounded in-memory LRU.
//
// Invariants:
//   - Auth and authz run per request. The middleware sits INSIDE the
//     auth wrapper, so a cache hit never skips session validation, and
//     the cache key contains the scope ApplyScope would resolve — an
//     entry computed for one identity can never serve another.
//   - TTL must stay ABOVE the clients' 30s poll: a TTL below the poll
//     means every poller arrives after its key expired and the cache
//     misses nearly all steady-state traffic (single-flight only
//     collapses *simultaneous* misses). Load logs a warning when set
//     below the poll interval.
//   - Only 200 responses are cached. Scope rejections (401/403) and
//     store errors (500) pass through and are never stored, so failures
//     are not sticky for the TTL window.
//   - single-flight serialises concurrent misses for the same key; the
//     stored body is immutable and written to clients without copying,
//     so shared replays are race-free.

// cacheMaxEntries bounds memory: dashboard payloads are small JSON
// aggregates; 512 entries is a few MB worst case against the 128Mi
// container limit. Sized to exceed the live working set (users × page
// variants × filter combos) at current fleet scale.
const cacheMaxEntries = 512

// dashboardPollSeconds mirrors the client refresh interval in
// dashboard.html/admin.html; the TTL warning compares against it.
const dashboardPollSeconds = 30

type cachedResponse struct {
	status      int
	contentType string
	body        []byte
}

// scopeResolver resolves (and enforces) the dashboard user filter for a
// request. ApplyScope is the production implementation; tests substitute
// a stub so the middleware can be driven without a directory database.
type scopeResolver func(w http.ResponseWriter, r *http.Request, store *storage.Store, cfg config.Config, requestedUser string) (string, bool)

type DashboardCache struct {
	enabled    bool
	ttl        time.Duration
	entries    *expirable.LRU[string, cachedResponse]
	flights    singleflight.Group
	store      *storage.Store
	cfg        config.Config
	resolve    scopeResolver
	hits       atomic.Int64
	misses     atomic.Int64
	rejections atomic.Int64
}

func NewDashboardCache(cfg config.Config, store *storage.Store) *DashboardCache {
	ttl := time.Duration(cfg.DashboardCacheTTLSeconds) * time.Second
	if ttl <= 0 {
		ttl = 60 * time.Second
	}
	if ttl < dashboardPollSeconds*time.Second {
		slog.Warn("dashboard cache TTL below the client poll interval — the cache will miss most steady-state polls",
			"ttl", ttl, "poll", dashboardPollSeconds*time.Second)
	}
	enabled := cfg.DashboardCacheEnabled
	c := &DashboardCache{
		enabled: enabled,
		ttl:     ttl,
		store:   store,
		cfg:     cfg,
		resolve: ApplyScope,
	}
	if enabled {
		c.entries = expirable.NewLRU[string, cachedResponse](cacheMaxEntries, nil, ttl)
		slog.Info("dashboard response cache enabled", "ttl", ttl, "max_entries", cacheMaxEntries)
	}
	return c
}

// scopeContextKey backs resolveScope: the middleware resolves the scope
// once, hands the handler the result through the context, and the handler
// skips its own directory lookups. Cache-off leaves the context empty and
// handlers resolve exactly as before.
type scopeContextKey struct{}

func scopeFromRequest(r *http.Request) (string, bool) {
	sc, ok := r.Context().Value(scopeContextKey{}).(string)
	return sc, ok
}

// resolveScope is the handlers' entry point: reuse the middleware's
// pre-resolution when present, otherwise enforce scope inline as before.
func resolveScope(w http.ResponseWriter, r *http.Request, store *storage.Store, cfg config.Config, requestedUser string) (string, bool) {
	if sc, ok := scopeFromRequest(r); ok {
		return sc, true
	}
	return ApplyScope(w, r, store, cfg, requestedUser)
}

// Wrap guards one dashboard handler. Usage: auth(cache.Wrap(h.HandleOverview)).
func (c *DashboardCache) Wrap(next http.HandlerFunc) http.HandlerFunc {
	if c == nil || !c.enabled {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		requestedUser := r.URL.Query().Get("user")

		// Resolve (and enforce) the scope BEFORE any cache lookup: the
		// resolved list is the identity half of the key, and a rejected
		// caller must never reach the cache at all.
		rec := &capture{header: make(http.Header)}
		scope, ok := c.resolve(rec, r, c.store, c.cfg, requestedUser)
		if !ok {
			c.rejections.Add(1)
			rec.replayTo(w)
			return
		}

		key := cacheKey(r, canonicalScope(scope))
		if entry, found := c.entries.Get(key); found {
			c.hits.Add(1)
			writeCached(w, entry)
			return
		}

		v, _, _ := c.flights.Do(key, func() (any, error) {
			// Double-check: the flight we waited behind may have stored it.
			if entry, found := c.entries.Get(key); found {
				return entry, nil
			}
			c.misses.Add(1)
			inner := &capture{header: make(http.Header)}
			// The handler re-resolves scope; hand it the middleware's
			// result so the directory lookups run once per request.
			// WithoutCancel: if the request that STARTED the flight
			// disconnects, its cancellation must not fail the query for
			// every other waiter riding the same flight.
			ctx := context.WithValue(context.WithoutCancel(r.Context()), scopeContextKey{}, scope)
			next.ServeHTTP(inner, r.WithContext(ctx))
			if inner.status == http.StatusOK {
				entry := cachedResponse{
					status:      inner.status,
					contentType: inner.header.Get("Content-Type"),
					body:        append([]byte(nil), inner.body.Bytes()...),
				}
				c.entries.Add(key, entry)
				return entry, nil
			}
			return responseOnly{rec: inner}, nil
		})

		switch res := v.(type) {
		case cachedResponse:
			writeCached(w, res)
		case responseOnly:
			res.rec.replayTo(w)
		}
	}
}

func writeCached(w http.ResponseWriter, entry cachedResponse) {
	if entry.contentType != "" {
		w.Header().Set("Content-Type", entry.contentType)
	}
	// writeJSON sets CORS on the live path; replay it so cached and fresh
	// responses are byte-equivalent for any reader.
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(entry.status)
	_, _ = w.Write(entry.body)
}

// cacheKey hashes method + path + raw query + canonical scope. Raw query
// keeps every filter param (range/from/to/group/model/sort/order/limit/
// group_by/ref) without the middleware needing to know their meaning;
// clients send them in a stable order, and the hash keeps memory flat
// regardless of key length.
func cacheKey(r *http.Request, scope string) string {
	h := sha256.New()
	h.Write([]byte(r.Method))
	h.Write([]byte("\n"))
	h.Write([]byte(r.URL.Path))
	h.Write([]byte("\n"))
	h.Write([]byte(r.URL.RawQuery))
	h.Write([]byte("\n"))
	h.Write([]byte(scope))
	return hex.EncodeToString(h.Sum(nil))
}

// canonicalScope sorts the usernames so a shuffled PersonUsernames result
// (the DB order is incidental) cannot split one logical scope across
// several cache keys. The empty scope is the admin's "everyone" view and
// must stay distinct from any concrete list — sorting never merges it.
func canonicalScope(scope string) string {
	if scope == "" {
		return ""
	}
	parts := strings.Split(scope, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

// capture buffers one response so the middleware can store or replay it.
// It only needs to cover what these handlers emit: one WriteHeader (or
// an implicit 200 from the first Write) plus headers set before writing.
type capture struct {
	status int
	body   bytes.Buffer
	header http.Header
}

func (c *capture) Header() http.Header { return c.header }

func (c *capture) Write(b []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	return c.body.Write(b)
}

func (c *capture) WriteHeader(status int) {
	if c.status == 0 {
		c.status = status
	}
}

func (c *capture) replayTo(w http.ResponseWriter) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	for k, vs := range c.header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(c.status)
	_, _ = w.Write(c.body.Bytes())
}

// responseOnly carries a non-200 through the single-flight without
// letting it poison the cache.
type responseOnly struct{ rec *capture }

// ServeStats exposes the cache counters as JSON (super-admin gated at
// registration).
func (c *DashboardCache) ServeStats(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, c.Stats())
}

// Stats returns cache counters for the admin verify step
// (hits/misses/rejections since process start).
func (c *DashboardCache) Stats() map[string]any {
	if c == nil || !c.enabled {
		return map[string]any{"enabled": false}
	}
	return map[string]any{
		"enabled":     c.enabled,
		"ttl_seconds": int(c.ttl.Seconds()),
		"entries":     c.entries.Len(),
		"hits":        c.hits.Load(),
		"misses":      c.misses.Load(),
		"rejections":  c.rejections.Load(),
	}
}
