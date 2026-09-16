package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/noyitz/ai-gateway-metering-service/internal/config"
)

func testCfg() config.Config {
	return config.Config{
		UserHeader:   "X-Forwarded-User",
		GroupsHeader: "X-Forwarded-Groups",
		AdminUsers:   []string{"boss"},
		DefaultGroup: "default",
	}
}

// The pre-existing behaviour of IsAdmin for callers WITH an identity is
// unchanged by this feature (admin allowlist only) — and with the flipped
// default, an anonymous caller is nobody, not an admin.
func TestIsAdmin_SecurityDefaults(t *testing.T) {
	cfg := testCfg()

	mk := func(user string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		if user != "" {
			r.Header.Set(cfg.UserHeader, user)
		}
		return r
	}
	if !IsAdmin(cfg, mk("boss")) {
		t.Error("allowlisted admin must be admin")
	}
	if IsAdmin(cfg, mk("someone")) {
		t.Error("random user must not be admin")
	}
	if IsAdmin(cfg, mk("")) {
		t.Error("anonymous caller must NOT be admin once org visibility exists")
	}
	anonAdmin := testCfg()
	anonAdmin.AllowUnauthenticatedAdmin = true
	if !IsAdmin(anonAdmin, mk("")) {
		t.Error("explicit opt-in must still work for local dev")
	}
}

// ApplyScope is the single gate every dashboard query passes through.
// These cases run without a database on purpose: the branches they cover
// (admin passthrough, anonymous rejection) must not touch the store at all.
func TestApplyScope_DBFreeBranches(t *testing.T) {
	cfg := testCfg()

	// Admin: filter passes through byte-for-byte — existing behaviour.
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set(cfg.UserHeader, "boss")
	w := httptest.NewRecorder()
	got, ok := ApplyScope(w, r, nil, cfg, "alice,bob")
	if !ok || got != "alice,bob" {
		t.Errorf("admin passthrough broken: got %q ok=%v", got, ok)
	}

	// Anonymous non-admin: rejected, and a nil store proves nothing was
	// queried (a store call would panic).
	r = httptest.NewRequest(http.MethodGet, "/", nil)
	w = httptest.NewRecorder()
	if _, ok := ApplyScope(w, r, nil, cfg, ""); ok {
		t.Error("anonymous caller must be rejected")
	}
	if w.Code != http.StatusUnauthorized {
		t.Errorf("anonymous rejection should be 401, got %d", w.Code)
	}
}

// The actor helper must attribute writes to the REAL admin mid view-as, not
// the swapped identity — the audit trail depends on it.
func TestActor_ImpersonationAttribution(t *testing.T) {
	cfg := testCfg()
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.Header.Set(cfg.UserHeader, "viewed-user")
	r.Header.Set(realUserHeader, "boss")
	if a := actor(r, cfg); a != "boss" {
		t.Errorf("actor must be the real session identity, got %q", a)
	}
	if c := caller(r, cfg); c != "viewed-user" {
		t.Errorf("caller must be the swapped identity for scope math, got %q", c)
	}
}

// The three-way split the role feature depends on: super-admin implies
// admin (so the operators can still see the usage page their console links
// to), but admin never implies super-admin (the many admins who only want
// usage must not reach the console, routing, or compression).
func TestSuperAdminImpliesAdmin(t *testing.T) {
	cfg := config.Config{
		UserHeader:      "X-Forwarded-User",
		AdminUsers:      []string{"plainadmin"},
		SuperAdminUsers: []string{"operator"},
	}
	mk := func(user string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		if user != "" {
			r.Header.Set(cfg.UserHeader, user)
		}
		return r
	}
	op := mk("operator")
	if !IsSuperAdmin(cfg, op) {
		t.Error("listed operator must be super-admin")
	}
	if !IsAdmin(cfg, op) {
		t.Error("super-admin must imply admin (usage view stays reachable)")
	}
	pa := mk("plainadmin")
	if IsSuperAdmin(cfg, pa) {
		t.Error("a plain admin must NOT be super-admin")
	}
	if !IsAdmin(cfg, pa) {
		t.Error("plain admin must still be admin")
	}
	if IsAdmin(cfg, mk("")) || IsSuperAdmin(cfg, mk("")) {
		t.Error("anonymous caller is neither admin nor super-admin")
	}
}

// RequireSuperAdmin sends a plain admin (who can see usage) to the usage
// dashboard, and a non-admin to their own account — the fallback matches
// what each caller is actually allowed to look at.
func TestRequireSuperAdmin_Fallbacks(t *testing.T) {
	cfg := config.Config{
		UserHeader:      "X-Forwarded-User",
		AdminUsers:      []string{"plainadmin"},
		SuperAdminUsers: []string{"operator"},
	}
	plainAdminLoc := func(user string) string {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/routing", nil)
		r.Header.Set(cfg.UserHeader, user)
		RequireSuperAdmin(cfg, func(http.ResponseWriter, *http.Request) {
			t.Fatalf("%s must not reach super-admin handler", user)
		})(w, r)
		return w.Header().Get("Location")
	}
	if loc := plainAdminLoc("plainadmin"); loc != "/dashboard" {
		t.Errorf("plain admin redirected to %q, want /dashboard", loc)
	}
	if loc := plainAdminLoc("someone"); loc != "/me" {
		t.Errorf("non-admin redirected to %q, want /me", loc)
	}
	// Super-admin passes through.
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/routing", nil)
	r.Header.Set(cfg.UserHeader, "operator")
	reached := false
	RequireSuperAdmin(cfg, func(http.ResponseWriter, *http.Request) { reached = true })(w, r)
	if !reached {
		t.Error("operator must reach the super-admin handler")
	}
}
