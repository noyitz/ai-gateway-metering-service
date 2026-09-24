package handler

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/golang-lru/v2/expirable"

	"github.com/noyitz/ai-gateway-metering-service/internal/config"
	"github.com/noyitz/ai-gateway-metering-service/internal/storage"
)

// stubScope resolves a fixed (or header-derived) scope and never touches
// a database — the middleware's contract with ApplyScope is exercised
// through this seam.
func stubScope(scope string, allow bool) scopeResolver {
	return func(w http.ResponseWriter, _ *http.Request, _ *storage.Store, _ config.Config, requested string) (string, bool) {
		if !allow {
			http.Error(w, "user outside your visibility scope", http.StatusForbidden)
			return "", false
		}
		if scope == "<request>" {
			return requested, true
		}
		return scope, true
	}
}

func newTestCache(t *testing.T, ttl time.Duration, resolve scopeResolver) *DashboardCache {
	t.Helper()
	return &DashboardCache{
		enabled: true,
		ttl:     ttl,
		cfg:     config.Config{},
		resolve: resolve,
		entries: expirable.NewLRU[string, cachedResponse](cacheMaxEntries, nil, ttl),
	}
}

func countingHandler(t *testing.T, calls *atomic.Int64, body func(r *http.Request) string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if body != nil {
			writeJSON(w, map[string]any{"body": body(r), "call": n})
			return
		}
		writeJSON(w, map[string]any{"call": n})
	}
}

func TestCacheHitExpiryRecompute(t *testing.T) {
	var calls atomic.Int64
	c := newTestCache(t, 60*time.Millisecond, stubScope("alice", true))
	h := c.Wrap(countingHandler(t, &calls, nil))

	get := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h(w, httptest.NewRequest("GET", "/api/v1/dashboard/overview?range=24h", nil))
		return w
	}

	first := get()
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d, want 200", first.Code)
	}
	second := get()
	if got := calls.Load(); got != 1 {
		t.Fatalf("handler calls after poll #2 = %d, want 1 (cache hit)", got)
	}
	if second.Body.String() != first.Body.String() {
		t.Fatal("cached body differs from computed body")
	}
	if ct := second.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("cached Content-Type = %q, want application/json", ct)
	}
	if cors := second.Header().Get("Access-Control-Allow-Origin"); cors != "*" {
		t.Errorf("cached CORS = %q, want *", cors)
	}

	time.Sleep(120 * time.Millisecond) // past TTL
	third := get()
	if got := calls.Load(); got != 2 {
		t.Fatalf("handler calls after expiry = %d, want 2 (recompute)", got)
	}
	if !strings.Contains(third.Body.String(), `"call":2`) {
		t.Errorf("post-expiry response is stale: %s", third.Body.String())
	}
}

func TestNoCacheHeaderBypassesCache(t *testing.T) {
	var calls atomic.Int64
	c := newTestCache(t, time.Minute, stubScope("alice", true))
	h := c.Wrap(countingHandler(t, &calls, nil))

	first := httptest.NewRecorder()
	h(first, httptest.NewRequest("GET", "/api/v1/dashboard/recent", nil))
	if got := calls.Load(); got != 1 {
		t.Fatalf("initial handler calls = %d, want 1", got)
	}

	freshReq := httptest.NewRequest("GET", "/api/v1/dashboard/recent", nil)
	freshReq.Header.Set("Cache-Control", "no-cache")
	fresh := httptest.NewRecorder()
	h(fresh, freshReq)
	if got := calls.Load(); got != 2 {
		t.Fatalf("handler calls after no-cache refresh = %d, want 2", got)
	}
	if !strings.Contains(fresh.Body.String(), `"call":2`) {
		t.Fatalf("no-cache response reused the cached body: %s", fresh.Body.String())
	}

	third := httptest.NewRecorder()
	h(third, httptest.NewRequest("GET", "/api/v1/dashboard/recent", nil))
	if got := calls.Load(); got != 2 {
		t.Fatalf("normal request after bypass = %d, want 2 (original cache remains valid)", got)
	}
}

func TestScopeKeyIsolation(t *testing.T) {
	var calls atomic.Int64
	c := newTestCache(t, time.Minute, func(w http.ResponseWriter, r *http.Request, _ *storage.Store, _ config.Config, requested string) (string, bool) {
		return r.Header.Get("X-Test-User"), true
	})
	h := c.Wrap(countingHandler(t, &calls, func(r *http.Request) string {
		return r.Header.Get("X-Test-User") // body is identity-bearing
	}))

	send := func(user string) string {
		req := httptest.NewRequest("GET", "/api/v1/dashboard/overview", nil)
		req.Header.Set("X-Test-User", user)
		w := httptest.NewRecorder()
		h(w, req)
		return w.Body.String()
	}

	alice1 := send("alice")
	bob1 := send("bob")
	alice2 := send("alice")
	bob2 := send("bob")

	if got := calls.Load(); got != 2 {
		t.Fatalf("handler calls = %d, want 2 (one per scope)", got)
	}
	if alice2 != alice1 || bob2 != bob1 {
		t.Error("same scope did not replay its own entry")
	}
	if strings.Contains(alice1, "bob") || strings.Contains(bob1, "alice") {
		t.Fatal("cross-scope leak: an entry served the wrong identity")
	}
}

func TestFilterKeyIsolation(t *testing.T) {
	var calls atomic.Int64
	c := newTestCache(t, time.Minute, stubScope("alice", true))
	h := c.Wrap(countingHandler(t, &calls, nil))

	send := func(query string) {
		h(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/v1/dashboard/users?"+query, nil))
	}
	send("range=24h&sort=cost&order=desc")
	send("range=30d&sort=cost&order=desc")
	send("range=24h&sort=cost&order=desc")
	send("range=24h&sort=name&order=desc")

	if got := calls.Load(); got != 3 {
		t.Fatalf("handler calls = %d, want 3 (each filter combo computed once)", got)
	}
}

func TestCanonicalScopeSharesKeyAcrossOrderings(t *testing.T) {
	var calls atomic.Int64
	// Same multi-login person, two directory orderings — must share one key.
	alt := 0
	c := newTestCache(t, time.Minute, func(w http.ResponseWriter, _ *http.Request, _ *storage.Store, _ config.Config, _ string) (string, bool) {
		alt++
		if alt%2 == 1 {
			return "bob@x,alice@y", true
		}
		return "alice@y,bob@x", true
	})
	h := c.Wrap(countingHandler(t, &calls, nil))
	h(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/v1/dashboard/overview", nil))
	h(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/v1/dashboard/overview", nil))

	if got := calls.Load(); got != 1 {
		t.Fatalf("handler calls = %d, want 1 (shuffled scope list must share the cache key)", got)
	}
}

func TestEmptyScopeStaysDistinct(t *testing.T) {
	// The admin "everyone" scope ("") must never collide with a concrete list.
	var calls atomic.Int64
	scopes := []string{"", "alice"}
	i := 0
	c := newTestCache(t, time.Minute, func(w http.ResponseWriter, _ *http.Request, _ *storage.Store, _ config.Config, _ string) (string, bool) {
		s := scopes[i%len(scopes)]
		i++
		return s, true
	})
	h := c.Wrap(countingHandler(t, &calls, nil))
	for range 6 {
		h(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/v1/dashboard/overview", nil))
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("handler calls = %d, want 2 (empty and named scopes stay distinct)", got)
	}
}

func TestRejectionNeverTouchesHandler(t *testing.T) {
	var calls atomic.Int64
	resolveCalls := 0
	c := newTestCache(t, time.Minute, func(w http.ResponseWriter, _ *http.Request, _ *storage.Store, _ config.Config, _ string) (string, bool) {
		resolveCalls++
		http.Error(w, "user outside your visibility scope", http.StatusForbidden)
		return "", false
	})
	h := c.Wrap(countingHandler(t, &calls, nil))

	for range 3 {
		w := httptest.NewRecorder()
		h(w, httptest.NewRequest("GET", "/api/v1/dashboard/overview", nil))
		if w.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", w.Code)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("rejected request reached the handler")
	}
	if resolveCalls != 3 {
		t.Fatalf("resolver ran %d times, want 3 — rejections must be enforced per request, not cached", resolveCalls)
	}
}

func TestNon200NotCached(t *testing.T) {
	var calls atomic.Int64
	c := newTestCache(t, time.Minute, stubScope("alice", true))
	failing := true
	var mu sync.Mutex
	h := c.Wrap(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		mu.Lock()
		bad := failing
		mu.Unlock()
		if bad {
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]string{"ok": "true"})
	})

	send := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h(w, httptest.NewRequest("GET", "/api/v1/dashboard/overview", nil))
		return w
	}
	if w := send(); w.Code != http.StatusInternalServerError {
		t.Fatalf("first = %d, want 500", w.Code)
	}
	if w := send(); w.Code != http.StatusInternalServerError {
		t.Fatalf("second = %d, want 500 (errors must not be cached)", w.Code)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("handler calls = %d, want 2 (each 500 recomputes)", got)
	}
	mu.Lock()
	failing = false
	mu.Unlock()
	if w := send(); w.Code != http.StatusOK {
		t.Fatalf("after recovery = %d, want 200", w.Code)
	}
}

// TestSingleFlightCoalesces proves the herd collapses to one computation:
// the resolver runs per request (before the flight), so waiting for it N
// times guarantees N requests are inside Wrap before the gate opens.
func TestSingleFlightCoalesces(t *testing.T) {
	const n = 10
	var calls atomic.Int64
	var resolvers atomic.Int64
	gate := make(chan struct{})
	entered := make(chan struct{})

	c := newTestCache(t, time.Minute, func(w http.ResponseWriter, _ *http.Request, _ *storage.Store, _ config.Config, _ string) (string, bool) {
		if resolvers.Add(1) == 1 {
			close(entered)
		}
		return "alice", true
	})
	h := c.Wrap(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		<-gate
		writeJSON(w, map[string]int{"v": 1})
	})

	bodies := make([]string, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := httptest.NewRecorder()
			h(w, httptest.NewRequest("GET", "/api/v1/dashboard/overview?range=24h", nil))
			bodies[i] = w.Body.String()
		}()
	}
	<-entered // first request is inside, blocked in the handler
	deadline := time.Now().Add(2 * time.Second)
	for resolvers.Load() < n && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // let stragglers reach singleflight.Do
	close(gate)
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Fatalf("handler calls = %d, want 1 (single-flight must collapse %d concurrent misses)", got, n)
	}
	for i, b := range bodies {
		if b != bodies[0] {
			t.Fatalf("waiter %d body differs: %q vs %q", i, b, bodies[0])
		}
	}
}

func TestKillSwitchIsExactPassthrough(t *testing.T) {
	cfg := config.Config{DashboardCacheEnabled: false, DashboardCacheTTLSeconds: 60}
	c := NewDashboardCache(cfg, nil)
	var calls atomic.Int64
	inner := countingHandler(t, &calls, nil)
	h := c.Wrap(inner)
	if fmt.Sprintf("%p", h) != fmt.Sprintf("%p", inner) {
		t.Fatal("disabled cache must return the untouched handler")
	}
	for range 3 {
		h(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/v1/dashboard/overview", nil))
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("handler calls = %d, want 3 (disabled = every request computes)", got)
	}
	if stats := c.Stats(); stats["enabled"] != false {
		t.Errorf("stats = %v, want enabled=false", stats)
	}
}

func TestNewCacheWarnsBelowPoll(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	NewDashboardCache(config.Config{DashboardCacheEnabled: false, DashboardCacheTTLSeconds: 10}, nil)
	if !strings.Contains(buf.String(), "below the client poll interval") {
		t.Error("expected startup warning when TTL < poll interval")
	}

	buf.Reset()
	NewDashboardCache(config.Config{DashboardCacheEnabled: false, DashboardCacheTTLSeconds: 60}, nil)
	if strings.Contains(buf.String(), "below the client poll") {
		t.Error("unexpected warning at 60s TTL")
	}
}

func TestResolveScopeHonoursPreResolution(t *testing.T) {
	// Cache ON path: context carries the scope; store is nil — resolveScope
	// must short-circuit without dereferencing it.
	r := httptest.NewRequest("GET", "/api/v1/dashboard/overview", nil).
		WithContext(context.WithValue(context.Background(), scopeContextKey{}, "pre@resolved"))
	got, ok := resolveScope(httptest.NewRecorder(), r, nil, config.Config{}, "ignored")
	if !ok || got != "pre@resolved" {
		t.Fatalf("got (%q,%v), want (pre@resolved,true) without touching the store", got, ok)
	}

	// Cache OFF path: admin branch runs ApplyScope DB-free.
	cfg := config.Config{UserHeader: "X-Forwarded-User", AdminUsers: []string{"boss"}}
	req := httptest.NewRequest("GET", "/api/v1/dashboard/overview", nil)
	req.Header.Set("X-Forwarded-User", "boss")
	got, ok = resolveScope(httptest.NewRecorder(), req, nil, cfg, "someone")
	if !ok || got != "someone" {
		t.Fatalf("admin passthrough = (%q,%v), want (someone,true)", got, ok)
	}
}

func TestStatsNilSafe(t *testing.T) {
	var c *DashboardCache
	if s := c.Stats(); s["enabled"] != false {
		t.Errorf("nil cache stats = %v", s)
	}
	w := httptest.NewRecorder()
	c.ServeStats(w, httptest.NewRequest("GET", "/api/v1/admin/cache-stats", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "enabled") {
		t.Errorf("ServeStats on nil cache = %d %s", w.Code, w.Body.String())
	}
}

func TestCanonicalScope(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"", ""},
		{"alice", "alice"},
		{"bob,alice", "alice,bob"},
		{" alice , bob ", "alice,bob"},
		{"a@x,b@y,c@z", "a@x,b@y,c@z"}, // already sorted stays put
	}
	for _, tt := range tests {
		if got := canonicalScope(tt.in); got != tt.want {
			t.Errorf("canonicalScope(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestCaptureImplicitStatusAndReplay(t *testing.T) {
	c := &capture{header: make(http.Header)}
	_, _ = io.WriteString(c, "hello") // implicit 200, as writeJSON does
	if c.status != http.StatusOK {
		t.Fatalf("implicit status = %d, want 200", c.status)
	}
	c.WriteHeader(http.StatusTeapot) // after first Write: must not change status
	if c.status != http.StatusOK {
		t.Fatalf("status changed after Write: %d", c.status)
	}
	w := httptest.NewRecorder()
	c.replayTo(w)
	if w.Code != http.StatusOK || w.Body.String() != "hello" {
		t.Errorf("replay = %d %q", w.Code, w.Body.String())
	}
}
