package handler

import (
	"context"
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
// Scope model (the whole feature's security hinges on this). The two
// surfaces deliberately differ:
//   - Usage page (/dashboard, ApplyScope): admins — managers included —
//     see every user; everyone else sees ONLY their own spend. Team
//     visibility is never a Usage-page feature.
//   - Team views (/manager, /api/v1/org/*): super-admins see the whole
//     organisation; managers see their own subtree (their people plus
//     subordinate managers' people — even when the manager is also an
//     admin); everyone else themselves only. A plain admin with no
//     reports sees no more than any other user here, and the nav hides
//     the My-team tab for them entirely.
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
// Admins pass through untouched — the Usage page is the admin's all-users
// view whether or not they also manage people. Everyone ELSE, managers
// included, sees only their own spend here: a manager looking at team
// spend goes to the My-team page, not the Usage filters. The requested
// user list (if any) must sit inside the caller's own identities or the
// request is rejected with 403. The result feeds the existing
// `username = ANY(string_to_array($n, ','))` storage filter unchanged.
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
	list := selfScope(r.Context(), store, me)
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

// selfScope resolves the usernames a non-admin caller may see on the Usage
// page: their own directory identities when they're in the directory (one
// person can hold several sign-in usernames), otherwise the session
// username alone. Managers get no subtree here — that is the whole point.
// Fails closed to the session username on any directory error.
func selfScope(ctx context.Context, store *storage.Store, me string) []string {
	_, _, slug, err := store.ScopeUsernames(ctx, me)
	if err != nil {
		// Fail closed to self: a directory outage must never widen anyone's
		// visibility beyond what existed before this feature.
		slog.Error("scope resolution failed, falling back to self", "user", me, "error", err)
		return []string{me}
	}
	if slug == "" {
		return []string{me}
	}
	own, err := store.PersonUsernames(ctx, slug)
	if err != nil || len(own) == 0 {
		if err != nil {
			slog.Error("own-identities lookup failed, falling back to self", "user", me, "error", err)
		}
		return []string{me}
	}
	return own
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
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(data)
}

// HandleScope: GET /api/v1/org/scope — the page's bootstrap call.
func (h *OrgHandler) HandleScope(w http.ResponseWriter, r *http.Request) {
	me := caller(r, h.cfg)
	resp := map[string]any{
		"username":     me,
		"scope":        "self",
		"isAdmin":      IsAdmin(h.cfg, r),
		"isSuperAdmin": IsSuperAdmin(h.cfg, r),
		"isManager":    false,
		"scopeSize":    1,
		"roots":        []string{},
		"personSlug":   "",
	}
	if me == "" {
		writeJSON(w, resp)
		return
	}
	// Only super-admins get the org-wide view on the team page. A plain
	// admin — even one who manages people — resolves below exactly like
	// everyone else: their own subtree if the directory has reports under
	// them, otherwise self. That's what makes "manager who is also an
	// admin" show HIS team here while still seeing all users on Usage.
	if IsSuperAdmin(h.cfg, r) {
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

// scopeRoot resolves which subtree a ?root= request may read. Only
// super-admins may name any slug; with no root they see the whole
// organisation (empty slug). A plain admin is NOT special here — they
// resolve through the manager path below, so an admin who also manages
// people sees their own branch, nothing wider. A manager may name
// their own slug or any slug INSIDE their subtree — that is the manager of
// managers drill-down: focusing the team view on one subordinate manager's
// branch. Anyone outside the tree gets a 403; people with no directory entry
// get their own name — which has no tree, so callers get an empty chart
// rather than a 403 on a page they're allowed to open.
func (h *OrgHandler) scopeRoot(w http.ResponseWriter, r *http.Request) (string, bool) {
	me := caller(r, h.cfg)
	reqRoot := storage.SlugNorm(r.URL.Query().Get("root"))
	if IsSuperAdmin(h.cfg, r) {
		// No explicit root means the WHOLE organisation — the empty
		// string, which the store spells as "every root". It used to
		// silently mean "the alphabetically-first root's branch", so
		// the manager page showed admins an unrelated person's team.
		return reqRoot, true
	}
	_, _, slug, err := h.store.ScopeUsernames(r.Context(), me)
	if err != nil || slug == "" {
		http.Error(w, "not in directory", http.StatusNotFound)
		return "", false
	}
	if reqRoot != "" && reqRoot != slug {
		inside, err := h.store.InSubtree(r.Context(), slug, reqRoot)
		if err != nil {
			slog.Error("subtree check failed", "root", slug, "target", reqRoot, "error", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return "", false
		}
		if !inside {
			// Explicitly outside the caller's tree.
			http.Error(w, "outside your visibility scope", http.StatusForbidden)
			return "", false
		}
		return reqRoot, true
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

// HandleOrgPerson: GET /api/v1/org/person?slug=&range= — one person's
// per-model breakdown, the drill-down behind a row in the team table. The
// permission is the same one that governs the tree: a super-admin may open
// anyone; everyone else — plain admins included — may open a person inside
// their own subtree (which always includes themselves).
func (h *OrgHandler) HandleOrgPerson(w http.ResponseWriter, r *http.Request) {
	slug := storage.SlugNorm(r.URL.Query().Get("slug"))
	if slug == "" {
		http.Error(w, "slug required", http.StatusBadRequest)
		return
	}
	if !IsSuperAdmin(h.cfg, r) {
		_, _, mine, err := h.store.ScopeUsernames(r.Context(), caller(r, h.cfg))
		if err != nil {
			slog.Error("scope resolution failed", "error", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		if mine == "" {
			http.Error(w, "not in directory", http.StatusNotFound)
			return
		}
		if mine != slug {
			inside, err := h.store.InSubtree(r.Context(), mine, slug)
			if err != nil {
				slog.Error("subtree check failed", "error", err)
				http.Error(w, "internal server error", http.StatusInternalServerError)
				return
			}
			if !inside {
				http.Error(w, "outside your visibility scope", http.StatusForbidden)
				return
			}
		}
	}
	person, err := h.store.GetPerson(r.Context(), slug)
	if err == sql.ErrNoRows {
		http.Error(w, "no such person", http.StatusNotFound)
		return
	}
	if err != nil {
		slog.Error("get person failed", "slug", slug, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	usernames, err := h.store.PersonUsernames(r.Context(), slug)
	if err != nil {
		slog.Error("person identities failed", "slug", slug, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	since, until := parseTimeWindow(r)
	models, err := h.store.GetPersonModelUsage(r.Context(), usernames, since, until, "")
	if err != nil {
		slog.Error("person usage query failed", "slug", slug, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if models == nil {
		models = []storage.OrgPersonModelRow{}
	}
	writeJSON(w, map[string]any{
		"slug":        person.Slug,
		"fullName":    person.FullName,
		"usernames":   usernames,
		"noIdentity":  len(usernames) == 0,
		"models":      models,
	})
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

// HandleInvites: GET list; POST {"person_slug","key_name"} creates an invite
// for that person, stamped with their directory group (no group input is
// accepted), and returns its URL EXACTLY ONCE.
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
			KeyName    string `json:"key_name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.PersonSlug == "" {
			http.Error(w, "person_slug required", http.StatusBadRequest)
			return
		}
		slug := storage.SlugNorm(body.PersonSlug)
		// The invite's group IS the person's directory group — always. The
		// old group dropdown let one mis-clicked selection stamp a key for
		// a group the person is not in (an ai-eng invite went out to a
		// global-eng person), and the gateway honours the key's group, not
		// the person's. There is no override anymore: fix the person, not
		// the invite.
		person, err := h.store.GetPerson(r.Context(), slug)
		if err != nil {
			http.Error(w, "person not found — add them in People & Org first", http.StatusNotFound)
			return
		}
		if person.GroupName == "" {
			http.Error(w, person.FullName+" has no group set — set it in People & Org first", http.StatusBadRequest)
			return
		}
		if body.KeyName == "" {
			body.KeyName = "invite " + time.Now().UTC().Format("2006-01-02")
		}
		token, id, err := h.store.CreateInvite(r.Context(), slug, person.GroupName, body.KeyName, actor(r, h.cfg), h.inviteTTL)
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
.hint{color:#8b949e;font-size:13px}
</style></head><body>
<div class="card">
{{if .Done}}
<h1>Done — this is the only time this key will be shown</h1>
<p>Copy it now into your client's config, then close this page. It cannot be
recovered from this service: we keep only a hash. Lost it? Rotate it from the admin's Keys screen.</p>
<code id="k">{{.Key}}</code>
<button onclick="navigator.clipboard.writeText(document.getElementById('k').textContent).then(()=>this.textContent='Copied')">Copy key</button>
<p>Setup recipes for Claude Code, OpenCode and Codex — each with its own route,
auth header and base-URL placement — live on the
<a href="/welcome" style="color:#4da0f8">welcome page</a>.</p>
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
  try{
    const r=await fetch(location.pathname,{method:'POST',headers:{'Content-Type':'application/json'}});
    const body=await r.text();
    if(!r.ok){
      let msg='claim failed';
      try{msg=JSON.parse(body).error||msg;}catch(e){}
      document.querySelector('.card').innerHTML='<h1>Invite not usable</h1><p class="err">'+msg+'</p><p class="hint">If the message says the link is still valid, <a href="">reload this page</a> and click again.</p>';
      return;
    }
    // Success answers with the full claim page (the key is rendered once,
    // server-side, and never stored) — swap it in; a reload would re-runs
    // GET and the invite is now spent.
    document.open();document.write(body);document.close();
  }catch(e){
    b.disabled=false;b.textContent='Generate my key';
    document.querySelector('.card').insertAdjacentHTML('beforeend','<p class="err">Network problem — nothing was generated. Click again.</p>');
  }
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
			// Preview what POST will resolve, so a broken identity surfaces
			// BEFORE the claimant spends their one click.
			_, login, err := h.resolveClaimLogin(r.Context(), iv.PersonSlug)
			if err != nil || login == "" {
				renderClaim(w, claimData{Error: "Your directory record has no email to mint a key against. Ask your admin to add one and re-send the invite."})
				return
			}
			renderClaim(w, claimData{Name: iv.PersonName})
		} else {
			renderClaim(w, claimData{Error: "This invite is " + iv.Status + "."})
		}
	case http.MethodPost:
		// Resolve the mint target BEFORE consuming: ClaimInvite remains the
		// atomic single-use gate below, but an unfixable precondition (no
		// email to mint against) must not burn the link — a re-issue would
		// hit the same wall, and the claimant eats a dead link for free.
		iv, err := h.store.InviteByTokenHash(r.Context(), token)
		if err != nil || iv.Status != "pending" {
			writeClaimError(w, http.StatusGone, "this invite is invalid, already claimed, revoked, or expired")
			return
		}
		person, login, err := h.resolveClaimLogin(r.Context(), iv.PersonSlug)
		if err != nil {
			slog.Error("claim identity resolve failed", "person", iv.PersonSlug, "error", err)
			writeClaimError(w, http.StatusInternalServerError, "could not resolve your account — retry or contact your admin")
			return
		}
		if login == "" {
			writeClaimError(w, http.StatusPreconditionFailed, "your directory record has no email to mint a key against — ask your admin to add one and re-send the invite")
			return
		}
		if h.maasClient == nil {
			writeClaimError(w, http.StatusServiceUnavailable, "key service unavailable")
			return
		}
		group := iv.GroupName
		if group == "" {
			group = h.cfg.DefaultGroup
		}
		inviteID, _, _, _, err := h.store.ClaimInvite(r.Context(), token)
		if err != nil {
			writeClaimError(w, http.StatusGone, err.Error())
			return
		}
		created, err := h.maasClient.CreateAPIKey(r.Context(), login, group, iv.KeyName)
		if err != nil {
			slog.Error("claim mint failed", "person", iv.PersonSlug, "error", err)
			// Hand the token back: a transient maas-api failure becomes
			// "click again" instead of "beg the admin for a new invite".
			if rerr := h.store.ReleaseInviteClaim(r.Context(), inviteID); rerr != nil {
				slog.Error("invite release failed", "invite", inviteID, "error", rerr)
			}
			writeClaimError(w, http.StatusBadGateway, "could not mint the key — this link is still valid, retry or contact your admin")
			return
		}
		_ = h.store.SetInviteKey(r.Context(), inviteID, created.ID)
		_ = h.store.Audit(r.Context(), login, "key.claim", iv.PersonSlug, map[string]string{"key_id": created.ID})
		// Reports read names from user_profiles, not the directory — seed
		// theirs now so usage shows named from the first request. The
		// Display Names card can still overwrite this afterwards.
		if person.FirstName != "" || person.LastName != "" {
			if _, err := h.store.UpsertUserProfiles(r.Context(), []storage.UserProfile{
				{Username: login, FirstName: person.FirstName, LastName: person.LastName},
			}); err != nil {
				slog.Warn("claim name profile upsert failed", "login", login, "error", err)
			}
		}
		// The plaintext key: rendered once, in the claimant's browser.
		renderClaim(w, claimData{Done: true, Key: created.Key})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// resolveClaimLogin decides which MaaS username a claim mints against: the
// person's linked login, or — when no link exists (manual adds pre-dating
// auto-link, roster rows without a unique candidate) — their directory
// email, linked on the spot. MaaS usernames ARE emails, so this is not a
// guess; LinkIdentity is the same write the admin's identity picker makes.
func (h *OrgHandler) resolveClaimLogin(ctx context.Context, slug string) (storage.Person, string, error) {
	person, err := h.store.GetPerson(ctx, slug)
	if err != nil {
		return person, "", err
	}
	if person.Username != "" {
		return person, person.Username, nil
	}
	if person.Email != "" {
		if err := h.store.LinkIdentity(ctx, person.Email, slug, "invite-claim"); err != nil {
			return person, "", err
		}
		return person, person.Email, nil
	}
	return person, "", nil
}

func writeClaimError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

type claimData struct {
	Name  string
	Error string
	Done  bool
	Key   string
}

func renderClaim(w http.ResponseWriter, d claimData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	if err := claimTmpl.Execute(w, d); err != nil {
		slog.Error("claim page render failed", "error", err)
	}
}

// WhoAmIScope decorates /whoami responses with the org fields. Kept
// separate from HandleWhoAmI so keys.go stays what it was.
//
// It resolves for admins too — admins are no longer blanket-special here:
// the tabs use isManager to decide whether the admin caller has a My-team
// tab at all (manager-admin: yes, their own branch; plain admin: no).
func WhoAmIScope(r *http.Request, store *storage.Store, cfg config.Config) (isManager bool, scopeSize int) {
	me := caller(r, cfg)
	if me == "" {
		return false, 0
	}
	list, isMgr, _, err := store.ScopeUsernames(r.Context(), me)
	if err != nil {
		return false, 0
	}
	return isMgr, len(list)
}
