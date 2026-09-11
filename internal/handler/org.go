package handler

import (
	"database/sql"
	"encoding/json"
	"html/template"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/noyitz/ai-gateway-metering-service/internal/config"
	"github.com/noyitz/ai-gateway-metering-service/internal/dashboard"
	"github.com/noyitz/ai-gateway-metering-service/internal/maasapi"
	"github.com/noyitz/ai-gateway-metering-service/internal/storage"
	"io/fs"
)

// OrgHandler serves the people directory, the manager scope, the org chart,
// and key invites.
//
// Scope model (the whole feature's security hinges on this):
//   - admin: sees everything (ADMIN_USERS allowlist, as today);
//   - manager: a person WITH REPORTS in the directory — never assigned,
//     always derived from the org shape. Sees their whole subtree: their
//     people plus subordinate managers' people;
//   - everyone else: themselves only, exactly as before this feature.
//
// Admin "view as" (the signed session claim) swaps the identity header
// BEFORE any scope is computed, so an admin impersonating a manager gets
// exactly that manager's view, and every write audits the real actor from
// X-Forwarded-Real-User.
type OrgHandler struct {
	store       *storage.Store
	cfg         config.Config
	maasClient  *maasapi.Client
	inviteTTL   time.Duration
	overlapDays int
}

func NewOrgHandler(store *storage.Store, cfg config.Config, maasClient *maasapi.Client) *OrgHandler {
	ttl := time.Duration(cfg.OrgInviteTTLHours) * time.Hour
	if ttl <= 0 {
		ttl = 72 * time.Hour
	}
	return &OrgHandler{store: store, cfg: cfg, maasClient: maasClient,
		inviteTTL: ttl, overlapDays: cfg.KeyRotationOverlapDays}
}

// actor returns the REAL session identity: while an admin is viewing as
// someone, RequireAuth keeps the true identity in realUserHeader. Every
// audit entry and admin write attributes to this, never the swapped header.
func actor(r *http.Request, cfg config.Config) string {
	if real := r.Header.Get(realUserHeader); real != "" {
		return real
	}
	return r.Header.Get(cfg.UserHeader)
}

// caller returns the identity in effect for this request (the impersonated
// user while viewing-as). All scope math runs against this.
func caller(r *http.Request, cfg config.Config) string {
	return r.Header.Get(cfg.UserHeader)
}

// ApplyScope rewrites the dashboard's user filter for a non-admin caller.
// Admins pass through untouched (today's behaviour, byte for byte). For
// everyone else the requested user list (if any) must sit inside their
// scope or the request is rejected with 403; with no explicit list, their
// whole scope becomes the filter. The result feeds the existing
// `username = ANY(string_to_array($n, ','))` storage filter unchanged —
// manager visibility is one new argument, not new reporting SQL.
// Returns ok=false when an error response was already written.
//
// Package-level (not an OrgHandler method) so every dashboard handler can
// reach it without constructing the org handler first.
func ApplyScope(w http.ResponseWriter, r *http.Request, store *storage.Store, cfg config.Config, requestedUser string) (string, bool) {
	if IsAdmin(cfg, r) {
		return requestedUser, true
	}
	me := caller(r, cfg)
	if me == "" {
		http.Error(w, "who are you?", http.StatusUnauthorized)
		return "", false
	}
	list, _, _, err := store.ScopeUsernames(r.Context(), me)
	if err != nil {
		// Fail closed to self: a directory outage must never widen anyone's
		// visibility beyond what existed before this feature.
		slog.Error("scope resolution failed, falling back to self", "user", me, "error", err)
		list = []string{me}
	}
	if requestedUser == "" {
		return strings.Join(list, ","), true
	}
	inScope := make(map[string]bool, len(list))
	for _, u := range list {
		inScope[u] = true
	}
	for _, u := range strings.Split(requestedUser, ",") {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		if !inScope[u] {
			http.Error(w, "user outside your visibility scope", http.StatusForbidden)
			return "", false
		}
	}
	return requestedUser, true
}

// ServeManager renders the manager page. Anyone signed in may open it —
// the page itself adapts (a plain user sees their own numbers, a manager
// their subtree); a manager who never appears in the directory is simply a
// plain user, which is the pre-feature behaviour.
func (h *OrgHandler) ServeManager(w http.ResponseWriter, r *http.Request) {
	data, err := fs.ReadFile(dashboard.FS, "manager.html")
	if err != nil {
		http.Error(w, "manager page not found", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(data)
}

// HandleScope: GET /api/v1/org/scope — the page's bootstrap call.
func (h *OrgHandler) HandleScope(w http.ResponseWriter, r *http.Request) {
	me := caller(r, h.cfg)
	resp := map[string]any{
		"username":   me,
		"scope":      "self",
		"isAdmin":    IsAdmin(h.cfg, r),
		"isManager":  false,
		"scopeSize":  1,
		"roots":      []string{},
		"personSlug": "",
	}
	if me == "" {
		writeJSON(w, resp)
		return
	}
	if IsAdmin(h.cfg, r) {
		resp["scope"] = "admin"
		roots, err := h.store.RootSlugs(r.Context())
		if err == nil {
			resp["roots"] = roots
		}
		writeJSON(w, resp)
		return
	}
	list, isManager, slug, err := h.store.ScopeUsernames(r.Context(), me)
	if err == nil {
		resp["scopeSize"] = len(list)
		resp["isManager"] = isManager
		resp["personSlug"] = slug
		if isManager {
			resp["scope"] = "manager"
			resp["roots"] = []string{slug}
		}
	}
	writeJSON(w, resp)
}

// scopeRoot resolves which subtree a ?root= request may read. Admins may
// name any slug (defaulting to the org's first root); managers only their
// own; everyone else gets their own name — which has no tree, so callers
// get an empty chart rather than a 403 on a page they're allowed to open.
func (h *OrgHandler) scopeRoot(w http.ResponseWriter, r *http.Request) (string, bool) {
	me := caller(r, h.cfg)
	reqRoot := storage.SlugNorm(r.URL.Query().Get("root"))
	if IsAdmin(h.cfg, r) {
		if reqRoot != "" {
			return reqRoot, true
		}
		roots, err := h.store.RootSlugs(r.Context())
		if err != nil || len(roots) == 0 {
			http.Error(w, "directory empty — import the roster first", http.StatusNotFound)
			return "", false
		}
		return roots[0], true
	}
	_, _, slug, err := h.store.ScopeUsernames(r.Context(), me)
	if err != nil || slug == "" {
		http.Error(w, "not in directory", http.StatusNotFound)
		return "", false
	}
	if reqRoot != "" && reqRoot != slug {
		// Explicitly outside the caller's tree.
		http.Error(w, "outside your visibility scope", http.StatusForbidden)
		return "", false
	}
	return slug, true
}

// HandleOrgTree: GET /api/v1/org/tree
func (h *OrgHandler) HandleOrgTree(w http.ResponseWriter, r *http.Request) {
	root, ok := h.scopeRoot(w, r)
	if !ok {
		return
	}
	tree, err := h.store.OrgTree(r.Context(), root)
	if err == sql.ErrNoRows {
		http.Error(w, "no such person", http.StatusNotFound)
		return
	}
	if err != nil {
		slog.Error("org tree query failed", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, tree)
}

// HandleOrgUsage: GET /api/v1/org/usage — per-person rollup under the
// caller's scope root, reusing the shared cost model.
func (h *OrgHandler) HandleOrgUsage(w http.ResponseWriter, r *http.Request) {
	root, ok := h.scopeRoot(w, r)
	if !ok {
		return
	}
	since, until := parseTimeWindow(r)
	rows, err := h.store.GetOrgUsage(r.Context(), root, since, until)
	if err != nil {
		slog.Error("org usage query failed", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if rows == nil {
		rows = []storage.OrgUsageRow{}
	}
	writeJSON(w, rows)
}

// --- Admin: people CRUD ---

// HandlePeople: GET /api/v1/admin/people?filter=…, POST (create one).
func (h *OrgHandler) HandlePeople(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		people, err := h.store.ListPeople(r.Context(), r.URL.Query().Get("filter"))
		if err != nil {
			slog.Error("list people failed", "error", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		if people == nil {
			people = []storage.Person{}
		}
		writeJSON(w, people)
	case http.MethodPost:
		var p storage.Person
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil || p.FullName == "" {
			http.Error(w, "full_name required", http.StatusBadRequest)
			return
		}
		created, err := h.store.CreatePerson(r.Context(), p, actor(r, h.cfg))
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, created)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// HandlePerson: /api/v1/admin/people/{slug} — GET or PATCH ({"fields":{...}}).
func (h *OrgHandler) HandlePerson(w http.ResponseWriter, r *http.Request) {
	slug := storage.SlugNorm(strings.TrimPrefix(r.URL.Path, "/api/v1/admin/people/"))
	if slug == "" {
		http.Error(w, "slug required", http.StatusBadRequest)
		return
	}
	switch r.Method {
	case http.MethodGet:
		p, err := h.store.GetPerson(r.Context(), slug)
		if err == sql.ErrNoRows {
			http.Error(w, "no such person", http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, p)
	case http.MethodPatch, http.MethodPost:
		var body struct {
			Fields map[string]any `json:"fields"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Fields) == 0 {
			http.Error(w, `body: {"fields": {...}}`, http.StatusBadRequest)
			return
		}
		if ms, ok := body.Fields["manager_slug"]; ok {
			if s, isStr := ms.(string); isStr && s != "" {
				body.Fields["manager_slug"] = storage.SlugNorm(s)
			}
		}
		updated, err := h.store.UpdatePerson(r.Context(), slug, actor(r, h.cfg), body.Fields)
		if err != nil {
			// Surface the cycle-trigger message verbatim — it is the clearest
			// explanation of why the change was refused.
			if strings.Contains(err.Error(), "cycle") {
				http.Error(w, "refused: that manager change would create a reporting cycle", http.StatusBadRequest)
				return
			}
			slog.Error("update person failed", "slug", slug, "error", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, updated)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// HandleIdentities: GET list; POST {"username","person_slug"} link;
// DELETE ?username= unlink.
func (h *OrgHandler) HandleIdentities(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		rows, err := h.store.ListIdentities(r.Context())
		if err != nil {
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, rows)
	case http.MethodPost:
		var body struct {
			Username string `json:"username"`
			Slug     string `json:"person_slug"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Username == "" || body.Slug == "" {
			http.Error(w, "username and person_slug required", http.StatusBadRequest)
			return
		}
		if err := h.store.LinkIdentity(r.Context(), body.Username, storage.SlugNorm(body.Slug), actor(r, h.cfg)); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]string{"status": "linked"})
	case http.MethodDelete:
		username := r.URL.Query().Get("username")
		if username == "" {
			http.Error(w, "username required", http.StatusBadRequest)
			return
		}
		if err := h.store.UnlinkIdentity(r.Context(), username, actor(r, h.cfg)); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]string{"status": "unlinked"})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// HandleImport: POST /api/v1/admin/org/import. Body is the generator's
// payload {"batch": {...}, "people": [...]}. dry_run (default true) returns
// the full diff WITHOUT writing anything; apply with {"dry_run": false}.
func (h *OrgHandler) HandleImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var payload struct {
		Batch  json.RawMessage        `json:"batch"`
		DryRun *bool                  `json:"dry_run"`
		People []storage.ImportPerson `json:"people"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || len(payload.People) == 0 {
		http.Error(w, "payload needs a non-empty \"people\" array (same shape as pricetag-people-import.json)", http.StatusBadRequest)
		return
	}
	dryRun := payload.DryRun == nil || *payload.DryRun
	filename := ""
	var batchMeta struct {
		Filename string `json:"filename"`
	}
	if len(payload.Batch) > 0 && json.Unmarshal(payload.Batch, &batchMeta) == nil {
		filename = batchMeta.Filename
	}
	result, err := h.store.ImportPeople(r.Context(), payload.People, filename, actor(r, h.cfg), dryRun)
	if err != nil {
		slog.Error("org import failed", "dry_run", dryRun, "error", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, result)
}

// --- Admin: invites ---

// HandleInvites: GET list; POST {"person_slug","group","key_name"} creates
// an invite and returns its URL EXACTLY ONCE.
func (h *OrgHandler) HandleInvites(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		list, err := h.store.ListInvites(r.Context())
		if err != nil {
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		if list == nil {
			list = []storage.Invite{}
		}
		writeJSON(w, list)
	case http.MethodPost:
		if h.maasClient == nil {
			http.Error(w, "key service not configured (MAAS_API_URL)", http.StatusServiceUnavailable)
			return
		}
		var body struct {
			PersonSlug string `json:"person_slug"`
			Group      string `json:"group"`
			KeyName    string `json:"key_name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.PersonSlug == "" {
			http.Error(w, "person_slug required", http.StatusBadRequest)
			return
		}
		slug := storage.SlugNorm(body.PersonSlug)
		if body.KeyName == "" {
			body.KeyName = "invite " + time.Now().UTC().Format("2006-01-02")
		}
		token, id, err := h.store.CreateInvite(r.Context(), slug, body.Group, body.KeyName, actor(r, h.cfg), h.inviteTTL)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// The plaintext token exists only in this response.
		writeJSON(w, map[string]any{"id": id, "invite_url": "/invite/" + token, "expires_in_hours": int(h.inviteTTL.Hours())})
	case http.MethodDelete:
		n, err := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
		if err != nil || n <= 0 {
			http.Error(w, "id required", http.StatusBadRequest)
			return
		}
		if err := h.store.RevokeInvite(r.Context(), n, actor(r, h.cfg)); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]string{"status": "revoked"})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// claimTmpl is deliberately minimal and self-contained: the claimant's key
// is minted when THIS page posts, rendered once in their own browser, and
// never stored anywhere in this service.
var claimTmpl = template.Must(template.New("claim").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Your API key</title>
<style>
:root{color-scheme:light dark;font-family:Inter,system-ui,sans-serif}
body{margin:0;min-height:100vh;display:grid;place-items:center;background:#0b0e11;color:#e2e6ec}
.card{max-width:560px;padding:32px;background:#141820;border:1px solid #2e3848;border-radius:12px}
h1{font-size:17px;margin:0 0 8px}
p{font-size:13px;color:#8090a8;line-height:1.6}
code{display:block;background:#0b0e11;border:1px solid #2e3848;border-radius:8px;padding:12px;
font-size:12px;word-break:break-all;color:#3dcc6e;margin:16px 0}
button{background:#4da0f8;color:#0b0e11;border:0;border-radius:8px;padding:10px 18px;
font-size:13px;font-weight:600;cursor:pointer}
.err{color:#e8554e}
</style></head><body>
<div class="card">
{{if .Done}}
<h1>Done — this is the only time this key will be shown</h1>
<p>Copy it now into your client's config, then close this page. It cannot be
recovered from this service: we keep only a hash. Lost it? Rotate it from the admin's Keys screen.</p>
<code id="k">{{.Key}}</code>
<button onclick="navigator.clipboard.writeText(document.getElementById('k').textContent).then(()=>this.textContent='Copied')">Copy key</button>
<p>Setup for Claude Code: set <code style="display:inline;padding:2px 6px">ANTHROPIC_API_KEY</code> (or the gateway base URL + key) to this value.</p>
{{else if .Error}}
<h1>Invite not usable</h1>
<p class="err">{{.Error}}</p>
<p>Ask your admin to issue a fresh invite — this link cannot be reused.</p>
{{else}}
<h1>Claim your API key</h1>
<p>Hello {{.Name}} — this link was issued for you by your admin. The key is
generated right now, by your own click, and is shown once. It never passes
through email or chat, and this service never stores it.</p>
<button id="go" onclick="claim()">Generate my key</button>
{{end}}
</div>
{{if not .Done}}{{if not .Error}}
<script>
async function claim(){
  const b=document.getElementById('go');b.disabled=true;b.textContent='Generating…';
  const r=await fetch(location.pathname,{method:'POST',headers:{'Content-Type':'application/json'}});
  const j=await r.json();
  if(!r.ok){document.querySelector('.card').innerHTML='<h1>Invite not usable</h1><p class="err">'+(j.error||'claim failed')+'</p>';return;}
  location.reload();
  window.__k=j.key;
}
</script>
{{end}}{{end}}
</body></html>`))

// HandleClaim serves /invite/{token}. Unauthenticated by design: the token
// IS the credential (single-use, hashed at rest, expiring). A GET previews
// the invite; the POST mints the key via maas-api and renders it once.
func (h *OrgHandler) HandleClaim(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.URL.Path, "/invite/")
	if token == "" || strings.Contains(token, "/") {
		http.NotFound(w, r)
		return
	}
	// Validate format before touching the DB (constant-time-ish cheap guard
	// against log noise; the real check is the sha256 match in ClaimInvite).
	if len(token) != 64 {
		renderClaim(w, claimData{Error: "This link is malformed."})
		return
	}

	switch r.Method {
	case http.MethodGet:
		// Preview without consuming: show the person's name so they can
		// confirm the link was meant for them before claiming.
		iv, err := h.store.InviteByTokenHash(r.Context(), token)
		if err != nil {
			renderClaim(w, claimData{Error: "This invite is invalid, already claimed, revoked, or expired."})
			return
		}
		if iv.Status == "pending" {
			renderClaim(w, claimData{Name: iv.PersonName})
		} else {
			renderClaim(w, claimData{Error: "This invite is " + iv.Status + "."})
		}
	case http.MethodPost:
		inviteID, personSlug, group, keyName, err := h.store.ClaimInvite(r.Context(), token)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusGone)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		person, err := h.store.GetPerson(r.Context(), personSlug)
		if err != nil || person.Username == "" {
			// No login bound yet — mint against the person's best-known
			// identity is impossible; fail the invite cleanly so the admin
			// can link the identity and re-issue (invite already consumed —
			// the admin sees the failure in the invite list).
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusPreconditionFailed)
			json.NewEncoder(w).Encode(map[string]string{"error": "your account is not linked yet — ask your admin to finish account linking and send a new invite"})
			return
		}
		if h.maasClient == nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(map[string]string{"error": "key service unavailable"})
			return
		}
		if group == "" {
			group = h.cfg.DefaultGroup
		}
		created, err := h.maasClient.CreateAPIKey(r.Context(), person.Username, group, keyName)
		if err != nil {
			slog.Error("claim mint failed", "person", personSlug, "error", err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			json.NewEncoder(w).Encode(map[string]string{"error": "could not mint the key — your admin has been noted; ask for a new invite"})
			return
		}
		_ = h.store.SetInviteKey(r.Context(), inviteID, created.ID)
		_ = h.store.Audit(r.Context(), person.Username, "key.claim", personSlug, map[string]string{"key_id": created.ID})
		// The plaintext key: rendered once, in the claimant's browser.
		renderClaim(w, claimData{Done: true, Key: created.Key})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

type claimData struct {
	Name  string
	Error string
	Done  bool
	Key   string
}

func renderClaim(w http.ResponseWriter, d claimData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := claimTmpl.Execute(w, d); err != nil {
		slog.Error("claim page render failed", "error", err)
	}
}

// WhoAmIScope decorates /whoami responses with the org fields. Kept
// separate from HandleWhoAmI so keys.go stays what it was.
func WhoAmIScope(r *http.Request, store *storage.Store, cfg config.Config) (isManager bool, scopeSize int) {
	me := caller(r, cfg)
	if me == "" || IsAdmin(cfg, r) {
		return false, 0
	}
	list, isMgr, _, err := store.ScopeUsernames(r.Context(), me)
	if err != nil {
		return false, 0
	}
	return isMgr, len(list)
}
