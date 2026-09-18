package handler

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/noyitz/ai-gateway-metering-service/internal/config"
	"github.com/noyitz/ai-gateway-metering-service/internal/storage"
)

// QuotaHandler serves the monthly-budget surfaces: the super-admin policy
// console, each person's own quota state, and the one-hop approval inbox.
// Route layout (registered in cmd/main.go behind auth()):
//
//	GET|PATCH  /api/v1/admin/quota/policy            super-admin
//	GET|PUT|DELETE /api/v1/admin/quota/overrides      super-admin
//	GET        /api/v1/me/quota
//	POST|DELETE /api/v1/me/quota/request
//	GET        /api/v1/org/quota-requests
//	POST       /api/v1/org/quota-requests/{id}/approve|reject
//
// Identity discipline follows the org handlers: visibility and scope math
// run against caller() (impersonation-aware — "view as" a manager shows
// that manager's inbox), while audit entries and decision attribution use
// actor() (the real session identity).
type QuotaHandler struct {
	store *storage.Store
	cfg   config.Config
}

func NewQuotaHandler(store *storage.Store, cfg config.Config) *QuotaHandler {
	return &QuotaHandler{store: store, cfg: cfg}
}

// requireSuperAdminJSON is RequireSuperAdmin for fetch() callers: a 403
// with a plain reason instead of a 302 to /dashboard, which a JSON client
// cannot follow into anything useful. Returns false after writing.
func (h *QuotaHandler) requireSuperAdminJSON(w http.ResponseWriter, r *http.Request) bool {
	if IsSuperAdmin(h.cfg, r) {
		return true
	}
	http.Error(w, "super-admin only", http.StatusForbidden)
	return false
}

// callerUser returns the identity in effect, or writes a 401 and returns ok=false.
func (h *QuotaHandler) callerUser(w http.ResponseWriter, r *http.Request) (string, bool) {
	user := caller(r, h.cfg)
	if user == "" {
		http.Error(w, "who are you?", http.StatusUnauthorized)
		return "", false
	}
	return user, true
}

// decodeJSON caps request bodies: these payloads are a form's worth of
// fields, and the reason textarea should not accept megabytes.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	defer r.Body.Close()
	dec := json.NewDecoder(io.LimitReader(r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

// quotaError maps storage outcomes onto statuses. Anything not recognised
// is a server-side failure: logged, echoed as a bare 500.
func (h *QuotaHandler) quotaError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, storage.ErrQuotaAlreadyPending):
		http.Error(w, "you already have a pending request — cancel it before filing a new one", http.StatusConflict)
	case errors.Is(err, storage.ErrQuotaNoPending):
		http.Error(w, "this request is no longer pending — it may already have been decided", http.StatusConflict)
	case errors.Is(err, storage.ErrQuotaNotInDirectory):
		http.Error(w, "you are not in the directory — ask an admin to add you before requesting extra quota", http.StatusBadRequest)
	case errors.Is(err, sql.ErrNoRows):
		http.Error(w, "not found", http.StatusNotFound)
	default:
		slog.Error("quota operation failed", "error", err, "path", r.URL.Path, "user", caller(r, h.cfg))
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}
}

// HandleAdminPolicy: GET reads the single policy row; PATCH accepts
// {"default_monthly_usd": 300, "enforced": true} — either field alone.
// The enforced flag is the whole feature's kill switch; flipping it on
// starts denying over-limit users at the gateway on their next request.
func (h *QuotaHandler) HandleAdminPolicy(w http.ResponseWriter, r *http.Request) {
	if !h.requireSuperAdminJSON(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		p, err := h.store.GetQuotaPolicy(r.Context())
		if err != nil {
			h.quotaError(w, r, err)
			return
		}
		writeJSON(w, p)
	case http.MethodPatch, http.MethodPut:
		var body struct {
			DefaultMonthlyUSD *float64 `json:"default_monthly_usd"`
			Enforced          *bool    `json:"enforced"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		if body.DefaultMonthlyUSD == nil && body.Enforced == nil {
			http.Error(w, "nothing to change: set default_monthly_usd and/or enforced", http.StatusBadRequest)
			return
		}
		if body.DefaultMonthlyUSD != nil && *body.DefaultMonthlyUSD <= 0 {
			http.Error(w, "default_monthly_usd must be > 0", http.StatusBadRequest)
			return
		}
		p, err := h.store.UpdateQuotaPolicy(r.Context(), actor(r, h.cfg), body.DefaultMonthlyUSD, body.Enforced)
		if err != nil {
			h.quotaError(w, r, err)
			return
		}
		writeJSON(w, p)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// HandleAdminDenials: GET this month's blocked-request tallies — the
// enforcement liveness signal for admins: denials recorded while the flag
// is on means the gateway circuit is actually reaching us.
func (h *QuotaHandler) HandleAdminDenials(w http.ResponseWriter, r *http.Request) {
	if !h.requireSuperAdminJSON(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	total, byUser, err := h.store.QuotaDenialTotals(r.Context())
	if err != nil {
		h.quotaError(w, r, err)
		return
	}
	writeJSON(w, map[string]any{"total": total, "by_user": byUser})
}

// HandleAdminOverrides: GET lists every per-user (person slug) or per-group
// override; PUT upserts one {"scope","principal","monthly_usd"}; DELETE
// takes ?scope=&principal=. Deleting an override falls the principal back
// to group/default — it never means "no budget".
func (h *QuotaHandler) HandleAdminOverrides(w http.ResponseWriter, r *http.Request) {
	if !h.requireSuperAdminJSON(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		list, err := h.store.ListQuotaOverrides(r.Context())
		if err != nil {
			h.quotaError(w, r, err)
			return
		}
		if list == nil {
			list = []storage.QuotaOverride{}
		}
		writeJSON(w, list)
	case http.MethodPut, http.MethodPost:
		var body struct {
			Scope      string  `json:"scope"`
			Principal  string  `json:"principal"`
			MonthlyUSD float64 `json:"monthly_usd"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		if body.Scope != "user" && body.Scope != "group" {
			http.Error(w, "scope must be 'user' or 'group'", http.StatusBadRequest)
			return
		}
		if strings.TrimSpace(body.Principal) == "" {
			http.Error(w, "principal is required (person slug or group name)", http.StatusBadRequest)
			return
		}
		if body.MonthlyUSD <= 0 {
			http.Error(w, "monthly_usd must be > 0 — delete the override to fall back to the default", http.StatusBadRequest)
			return
		}
		if err := h.store.UpsertQuotaOverride(r.Context(), actor(r, h.cfg), body.Scope, body.Principal, body.MonthlyUSD); err != nil {
			h.quotaError(w, r, err)
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	case http.MethodDelete:
		scope := r.URL.Query().Get("scope")
		principal := r.URL.Query().Get("principal")
		if scope != "user" && scope != "group" {
			http.Error(w, "scope must be 'user' or 'group'", http.StatusBadRequest)
			return
		}
		if principal == "" {
			http.Error(w, "principal is required", http.StatusBadRequest)
			return
		}
		if err := h.store.DeleteQuotaOverride(r.Context(), actor(r, h.cfg), scope, principal); err != nil {
			h.quotaError(w, r, err)
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// HandleMe: the caller's own quota picture — limit, spend, whether they are
// over, and their request state. This is what the popup and banner render
// (whoami also carries it on boot; this endpoint is for refreshes).
func (h *QuotaHandler) HandleMe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	user, ok := h.callerUser(w, r)
	if !ok {
		return
	}
	view, err := h.store.GetQuotaView(r.Context(), user, IsSuperAdmin(h.cfg, r))
	if err != nil {
		h.quotaError(w, r, err)
		return
	}
	writeJSON(w, view)
}

// HandleMeRequest: POST files the escalation questionnaire (asked_usd +
// the escalation answers; see storage.QuotaRequestInput) — routed to the
// caller's directory manager, or the super-admin backstop when they have
// none. DELETE cancels the caller's pending request; a super-admin may pass
// ?user= to cancel someone else's.
func (h *QuotaHandler) HandleMeRequest(w http.ResponseWriter, r *http.Request) {
	user, ok := h.callerUser(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodPost:
		// The escalation questionnaire. `reason` is the historical column
		// carrying the per-project task description (Q1).
		var body struct {
			AskedUSD           float64 `json:"asked_usd"`
			Reason             string  `json:"reason"`
			ReductionSteps     string  `json:"reduction_steps"`
			EstimateBasis      string  `json:"estimate_basis"`
			Timeline           string  `json:"timeline"`
			FeasibleWithinBase string  `json:"feasible_within_base"`
			WhyNotEnough       string  `json:"why_not_enough"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		in := storage.QuotaRequestInput{
			AskedUSD:           body.AskedUSD,
			Tasks:              body.Reason,
			ReductionSteps:     body.ReductionSteps,
			EstimateBasis:      body.EstimateBasis,
			Timeline:           body.Timeline,
			FeasibleWithinBase: body.FeasibleWithinBase,
			WhyNotEnough:       body.WhyNotEnough,
		}
		// One source of truth for questionnaire completeness, in storage.
		if err := in.Validate(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		q, err := h.store.CreateQuotaRequest(r.Context(), user, in)
		if err != nil {
			h.quotaError(w, r, err)
			return
		}
		writeJSON(w, q)
	case http.MethodDelete:
		target, admin := user, IsSuperAdmin(h.cfg, r)
		if u := r.URL.Query().Get("user"); u != "" && admin {
			target = u
		} else if u != "" {
			http.Error(w, "only a super-admin can cancel someone else's request", http.StatusForbidden)
			return
		}
		if err := h.store.CancelPendingForPerson(r.Context(), target, actor(r, h.cfg), admin); err != nil {
			h.quotaError(w, r, err)
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// HandleOrgRequests: the approval inbox. A super-admin sees everything;
// everyone else sees only requests stamped with their slug as approver —
// i.e. filed by their direct reports. ?state=pending|decided filters.
func (h *QuotaHandler) HandleOrgRequests(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	user, ok := h.callerUser(w, r)
	if !ok {
		return
	}
	isSuper := IsSuperAdmin(h.cfg, r)
	_, _, callerSlug, err := h.store.ScopeUsernames(r.Context(), user)
	if err != nil {
		slog.Error("scope resolution failed for quota inbox", "user", user, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if callerSlug == "" && !isSuper {
		// No directory identity: no inbox. Not an error — plenty of
		// sign-ins are not in the tree, and they see no requests.
		writeJSON(w, []storage.QuotaRequest{})
		return
	}
	state := r.URL.Query().Get("state")
	if state != "" && state != "pending" && state != "decided" {
		http.Error(w, "state must be 'pending' or 'decided'", http.StatusBadRequest)
		return
	}
	list, err := h.store.ListQuotaRequests(r.Context(), callerSlug, isSuper, state)
	if err != nil {
		h.quotaError(w, r, err)
		return
	}
	if list == nil {
		list = []storage.QuotaRequest{}
	}
	writeJSON(w, list)
}

// HandleOrgRequestAction: POST /api/v1/org/quota-requests/{id}/approve
// {"approved_usd": N} or /reject {"comment": "..."}. Authority: the
// request's own approver, and only while the requester still sits in their
// subtree, or any super-admin. A manager who needs someone else's sign-off
// settles it offline — the request never moves.
func (h *QuotaHandler) HandleOrgRequestAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	user, ok := h.callerUser(w, r)
	if !ok {
		return
	}

	idStr, action, found := strings.Cut(strings.TrimPrefix(r.URL.Path, "/api/v1/org/quota-requests/"), "/")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if !found || err != nil || id <= 0 {
		http.Error(w, "expected /api/v1/org/quota-requests/{id}/approve|reject", http.StatusBadRequest)
		return
	}
	if action != "approve" && action != "reject" {
		http.Error(w, "action must be 'approve' or 'reject'", http.StatusBadRequest)
		return
	}

	q, err := h.store.GetQuotaRequest(r.Context(), id)
	if err != nil {
		h.quotaError(w, r, err)
		return
	}
	if !h.mayDecide(w, r, user, q) {
		return
	}

	switch action {
	case "approve":
		var body struct {
			ApprovedUSD float64 `json:"approved_usd"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		if body.ApprovedUSD <= 0 || body.ApprovedUSD > 100000 {
			http.Error(w, "approved_usd must be a positive amount up to 100000", http.StatusBadRequest)
			return
		}
		decided, err := h.store.ApproveQuotaRequest(r.Context(), id, actor(r, h.cfg), body.ApprovedUSD)
		if err != nil {
			h.quotaError(w, r, err)
			return
		}
		writeJSON(w, decided)
	case "reject":
		var body struct {
			Comment string `json:"comment"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		if strings.TrimSpace(body.Comment) == "" {
			http.Error(w, "a comment is required — the requester will see it", http.StatusBadRequest)
			return
		}
		if len(body.Comment) > 2000 {
			http.Error(w, "comment is too long (2000 characters max)", http.StatusBadRequest)
			return
		}
		decided, err := h.store.RejectQuotaRequest(r.Context(), id, actor(r, h.cfg), body.Comment)
		if err != nil {
			h.quotaError(w, r, err)
			return
		}
		writeJSON(w, decided)
	}
}

// mayDecide: the approver stamped at creation, still over the requester in
// the tree (defense in depth against a directory edit after filing), or any
// super-admin. Writes 403 and returns false otherwise.
func (h *QuotaHandler) mayDecide(w http.ResponseWriter, r *http.Request, user string, q storage.QuotaRequest) bool {
	if IsSuperAdmin(h.cfg, r) {
		return true
	}
	_, _, callerSlug, err := h.store.ScopeUsernames(r.Context(), user)
	if err != nil {
		slog.Error("scope resolution failed for quota decision", "user", user, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return false
	}
	if callerSlug == "" || q.ApproverSlug == "" || callerSlug != q.ApproverSlug {
		http.Error(w, "this request is not yours to decide", http.StatusForbidden)
		return false
	}
	inTree, err := h.store.InSubtree(r.Context(), callerSlug, q.PersonSlug)
	if err != nil {
		slog.Error("subtree check failed for quota decision", "user", user, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return false
	}
	if !inTree {
		http.Error(w, "this requester is no longer in your team", http.StatusForbidden)
		return false
	}
	return true
}
