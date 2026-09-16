package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/lib/pq"

	"github.com/noyitz/ai-gateway-metering-service/internal/config"
	"github.com/noyitz/ai-gateway-metering-service/internal/maasapi"
	"github.com/noyitz/ai-gateway-metering-service/internal/storage"
)

// End-to-end tests for the claim endpoint against a real Postgres and a
// stubbed maas-api. The regressions they guard:
//
//  1. A manual directory add (no roster) must still be claimable: the
//     identity gets linked from the person's email instead of burning the
//     single-use invite on "account not linked".
//  2. A precondition failure (no email at all) must NOT consume the token.
//  3. A failed mint (maas-api down) releases the claim: "click again"
//     beats "beg the admin for a new invite".
//  4. A successful claim seeds user_profiles so reports show the name.
//
// Skipped unless DATABASE_URL is set:
//
//	DATABASE_URL=postgres://... go test ./internal/handler/ -run TestClaim
func openClaimTestStore(t *testing.T) (*storage.Store, context.Context) {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set — claim integration tests need a Postgres")
	}
	dbName := fmt.Sprintf("claimtest_%d", time.Now().UnixNano())
	admin, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open admin: %v", err)
	}
	if _, err := admin.Exec("CREATE DATABASE " + dbName); err != nil {
		admin.Close()
		t.Fatalf("create test db: %v", err)
	}
	admin.Close()
	store, err := storage.New(replaceDBName(dsn, dbName), 0)
	if err != nil {
		t.Fatalf("connect fresh db: %v", err)
	}
	t.Cleanup(func() {
		store.Close()
		if a, err := sql.Open("postgres", dsn); err == nil {
			a.Exec("DROP DATABASE " + dbName) //nolint:errcheck
			a.Close()
		}
	})
	return store, context.Background()
}

func replaceDBName(dsn, name string) string {
	for i := len(dsn) - 1; i >= 0; i-- {
		if dsn[i] == '/' {
			if q := strings.Index(dsn[i:], "?"); q >= 0 {
				return dsn[:i+1] + name + dsn[i+q:]
			}
			return dsn[:i+1] + name
		}
	}
	return dsn
}

type mintRecord struct {
	user, group string
}

// stubMaasAPI mimics POST /v1/api-keys; failFlag flips it to 5xx mid-test,
// and rec records the identity headers of the last successful mint.
func stubMaasAPI(t *testing.T, fail *atomic.Bool, rec *mintRecord) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/api-keys" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		if fail.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		rec.user = r.Header.Get("X-MaaS-Username")
		rec.group = r.Header.Get("X-MaaS-Group")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"id":  "key-123",
			"key": "sk-oai-testkey-abcdef",
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestClaimManualAddFullFlow(t *testing.T) {
	store, ctx := openClaimTestStore(t)
	var fail atomic.Bool
	var rec mintRecord
	srv := stubMaasAPI(t, &fail, &rec)

	p, err := store.CreatePerson(ctx, storage.Person{
		FullName: "Alice Chen", FirstName: "Alice", LastName: "Chen",
		Email: "achen@x.com",
	}, "boss")
	if err != nil {
		t.Fatalf("create person: %v", err)
	}
	token, _, err := store.CreateInvite(ctx, p.Slug, "ai-eng", "alice-key", "boss", time.Hour)
	if err != nil {
		t.Fatalf("invite: %v", err)
	}

	h := NewOrgHandler(store, config.Config{DefaultGroup: "ai-eng"}, maasapi.NewClient(srv.URL, "t"))

	// GET previews without consuming.
	w := httptest.NewRecorder()
	h.HandleClaim(w, httptest.NewRequest(http.MethodGet, "/invite/"+token, nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Alice Chen") {
		t.Fatalf("preview: code=%d body missing name", w.Code)
	}

	// POST mints against the email login and renders the key once.
	w = httptest.NewRecorder()
	h.HandleClaim(w, httptest.NewRequest(http.MethodPost, "/invite/"+token, nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "sk-oai-testkey-abcdef") {
		t.Fatalf("claim: code=%d body=%s", w.Code, w.Body.String())
	}
	if rec.user != "achen@x.com" {
		t.Fatalf("minted against %q, want achen@x.com", rec.user)
	}

	// Invite records the key and is consumed; a second claim is refused.
	iv, err := store.InviteByTokenHash(ctx, token)
	if err != nil || iv.Status != "claimed" || iv.KeyID != "key-123" {
		t.Fatalf("invite after claim: %+v %v", iv, err)
	}
	w = httptest.NewRecorder()
	h.HandleClaim(w, httptest.NewRequest(http.MethodPost, "/invite/"+token, nil))
	if w.Code != http.StatusGone {
		t.Fatalf("reclaim code=%d, want 410", w.Code)
	}

	// The claim seeded the report name.
	prof, err := store.GetUserProfile(ctx, "achen@x.com")
	if err != nil || prof.DisplayName() != "Alice Chen" {
		t.Fatalf("profile after claim: %+v %v", prof, err)
	}
}

func TestClaimNoEmailDoesNotBurnInvite(t *testing.T) {
	store, ctx := openClaimTestStore(t)
	var fail atomic.Bool
	var rec mintRecord
	srv := stubMaasAPI(t, &fail, &rec)

	p, err := store.CreatePerson(ctx, storage.Person{FullName: "No Email"}, "boss")
	if err != nil {
		t.Fatalf("create person: %v", err)
	}
	token, _, err := store.CreateInvite(ctx, p.Slug, "ai-eng", "k", "boss", time.Hour)
	if err != nil {
		t.Fatalf("invite: %v", err)
	}
	h := NewOrgHandler(store, config.Config{DefaultGroup: "ai-eng"}, maasapi.NewClient(srv.URL, "t"))

	w := httptest.NewRecorder()
	h.HandleClaim(w, httptest.NewRequest(http.MethodPost, "/invite/"+token, nil))
	if w.Code != http.StatusPreconditionFailed {
		t.Fatalf("code=%d, want 412", w.Code)
	}
	// The token must still be pending — the admin fixes the record and the
	// SAME link works; re-issuing would hit the same wall.
	iv, err := store.InviteByTokenHash(ctx, token)
	if err != nil || iv.Status != "pending" {
		t.Fatalf("invite burned on precondition failure: %+v %v", iv, err)
	}
}

func TestClaimMintFailureReleasesInvite(t *testing.T) {
	store, ctx := openClaimTestStore(t)
	var fail atomic.Bool
	fail.Store(true)
	var rec mintRecord
	srv := stubMaasAPI(t, &fail, &rec)

	p, err := store.CreatePerson(ctx, storage.Person{
		FullName: "Bob Ray", FirstName: "Bob", LastName: "Ray", Email: "bray@x.com",
	}, "boss")
	if err != nil {
		t.Fatalf("create person: %v", err)
	}
	token, _, err := store.CreateInvite(ctx, p.Slug, "", "k", "boss", time.Hour)
	if err != nil {
		t.Fatalf("invite: %v", err)
	}
	h := NewOrgHandler(store, config.Config{DefaultGroup: "fallback-grp"}, maasapi.NewClient(srv.URL, "t"))

	w := httptest.NewRecorder()
	h.HandleClaim(w, httptest.NewRequest(http.MethodPost, "/invite/"+token, nil))
	if w.Code != http.StatusBadGateway {
		t.Fatalf("code=%d, want 502", w.Code)
	}
	iv, _ := store.InviteByTokenHash(ctx, token)
	if iv.Status != "pending" {
		t.Fatalf("invite burned on mint failure: %+v", iv)
	}

	// maas-api recovers; the SAME link claims successfully.
	fail.Store(false)
	w = httptest.NewRecorder()
	h.HandleClaim(w, httptest.NewRequest(http.MethodPost, "/invite/"+token, nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "sk-oai-testkey-abcdef") {
		t.Fatalf("retry after recovery: code=%d", w.Code)
	}
	// Empty invite group fell through to the configured default, and the
	// mint targeted the email-derived login.
	if rec.user != "bray@x.com" {
		t.Fatalf("minted against %q", rec.user)
	}
	if rec.group != `["fallback-grp"]` {
		t.Fatalf(`mint group %q, want ["fallback-grp"]`, rec.group)
	}
}

// The invite's group is the person's directory group, always. A "group"
// field in the request body is ignored (a mis-clicked dropdown once stamped
// an ai-eng key onto a global-eng person, and the gateway honours the key's
// group — a wrong choice there is a wrong grant); a person with no group is
// refused with instructions to fix the directory record first.
func TestInviteGroupDerivesFromPerson(t *testing.T) {
	store, ctx := openClaimTestStore(t)
	var fail atomic.Bool
	var rec mintRecord
	srv := stubMaasAPI(t, &fail, &rec)
	h := NewOrgHandler(store, config.Config{AdminUsers: []string{"boss"}}, maasapi.NewClient(srv.URL, "t"))

	// Person WITH a group: an override attempt in the body must not win.
	p, err := store.CreatePerson(ctx, storage.Person{
		FullName: "Shane Utt", FirstName: "Shane", LastName: "Utt",
		Email: "sutt@x.com", GroupName: "global-eng",
	}, "boss")
	if err != nil {
		t.Fatalf("create person: %v", err)
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/admin/keys/invites",
		strings.NewReader(`{"person_slug":"`+p.Slug+`","group":"ai-eng"}`))
	r.Header.Set("X-Forwarded-User", "boss")
	h.HandleInvites(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("create invite: code=%d body=%s", w.Code, w.Body.String())
	}
	var out struct {
		InviteURL string `json:"invite_url"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	iv, err := store.InviteByTokenHash(ctx, strings.TrimPrefix(out.InviteURL, "/invite/"))
	if err != nil {
		t.Fatalf("invite lookup: %v", err)
	}
	if iv.GroupName != "global-eng" {
		t.Fatalf("invite stamped %q, want the person's own global-eng", iv.GroupName)
	}

	// Person with NO group: refused, and the message names the fix.
	p2, err := store.CreatePerson(ctx, storage.Person{FullName: "No Group", Email: "ng@x.com"}, "boss")
	if err != nil {
		t.Fatalf("create person: %v", err)
	}
	w = httptest.NewRecorder()
	h.HandleInvites(w, httptest.NewRequest(http.MethodPost, "/api/v1/admin/keys/invites",
		strings.NewReader(`{"person_slug":"`+p2.Slug+`"}`)))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "group") {
		t.Fatalf("no-group person: code=%d body=%s, want 400 naming the group", w.Code, w.Body.String())
	}

	// Unknown slug: 404, not a silently-empty invite.
	w = httptest.NewRecorder()
	h.HandleInvites(w, httptest.NewRequest(http.MethodPost, "/api/v1/admin/keys/invites",
		strings.NewReader(`{"person_slug":"not-in-directory"}`)))
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown slug: code=%d, want 404", w.Code)
	}
}
