package handler

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/noyitz/ai-gateway-metering-service/internal/config"
	"github.com/noyitz/ai-gateway-metering-service/internal/dashboard"
)

const (
	cookieName   = "metering_session"
	cookieMaxAge = 7 * 24 * time.Hour

	// realUserHeader carries the true session identity while an admin is
	// "viewing as" another user, so the UI can render a banner and the
	// whoami endpoint can report both identities.
	realUserHeader = "X-Forwarded-Real-User"
)

type sessionPayload struct {
	Username string   `json:"username"`
	Groups   []string `json:"groups"`
	// As, when set by an admin via HandleImpersonate, swaps the identity
	// header to this user for the rest of the session.
	As  string `json:"as,omitempty"`
	Exp int64  `json:"exp"`
}

type AuthHandler struct {
	cfg        config.Config
	secret     []byte
	httpClient *http.Client
}

func NewAuthHandler(cfg config.Config) *AuthHandler {
	secret := []byte(os.Getenv("SESSION_SECRET"))
	if len(secret) == 0 {
		secret = make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			panic("failed to generate session secret: " + err.Error())
		}
		slog.Warn("SESSION_SECRET not set — using random secret (sessions won't survive restarts)")
	}
	return &AuthHandler{
		cfg:        cfg,
		secret:     secret,
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

func (h *AuthHandler) sign(data []byte) string {
	mac := hmac.New(sha256.New, h.secret)
	mac.Write(data)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (h *AuthHandler) verify(data []byte, sig string) bool {
	expected := h.sign(data)
	return hmac.Equal([]byte(expected), []byte(sig))
}

func (h *AuthHandler) setCookie(w http.ResponseWriter, username string, groups []string, as string) {
	payload := sessionPayload{
		Username: username,
		Groups:   groups,
		As:       as,
		Exp:      time.Now().Add(cookieMaxAge).Unix(),
	}
	data, _ := json.Marshal(payload)
	sig := h.sign(data)
	value := base64.RawURLEncoding.EncodeToString(data) + "." + sig

	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    value,
		Path:     "/",
		MaxAge:   int(cookieMaxAge.Seconds()),
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}

func (h *AuthHandler) readCookie(r *http.Request) *sessionPayload {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return nil
	}
	parts := strings.SplitN(c.Value, ".", 2)
	if len(parts) != 2 {
		return nil
	}
	data, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil
	}
	if !h.verify(data, parts[1]) {
		return nil
	}
	var payload sessionPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil
	}
	if time.Now().Unix() > payload.Exp {
		return nil
	}
	return &payload
}

func (h *AuthHandler) HandleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		data, err := fs.ReadFile(dashboard.FS, "login.html")
		if err != nil {
			http.Error(w, "login page not found", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(data)
		return
	}

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	r.ParseForm()
	apiKey := strings.TrimSpace(r.FormValue("api_key"))
	if apiKey == "" {
		http.Redirect(w, r, "/login?error=invalid", http.StatusFound)
		return
	}

	validateURL := os.Getenv("MAAS_VALIDATE_URL")
	if validateURL == "" {
		validateURL = "http://maas-api:8080/internal/v1/api-keys/validate"
	}

	body, _ := json.Marshal(map[string]string{"key": apiKey})
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, validateURL, bytes.NewReader(body))
	if err != nil {
		slog.Error("failed to build validate request", "error", err)
		http.Redirect(w, r, "/login?error=invalid", http.StatusFound)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := h.httpClient.Do(req)
	if err != nil {
		slog.Error("maas-api validate failed", "error", err)
		http.Redirect(w, r, "/login?error=invalid", http.StatusFound)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		http.Redirect(w, r, "/login?error=invalid", http.StatusFound)
		return
	}

	var result struct {
		Valid    bool     `json:"valid"`
		Username string   `json:"username"`
		Groups   []string `json:"groups"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil || !result.Valid || result.Username == "" {
		http.Redirect(w, r, "/login?error=invalid", http.StatusFound)
		return
	}

	slog.Info("user logged in", "username", result.Username)
	h.setCookie(w, result.Username, result.Groups, "")
	http.Redirect(w, r, "/dashboard", http.StatusFound)
}

// HandleImpersonate lets an admin open another user's view. With ?as=USER the
// target is stored in the signed session and the browser is sent to that
// user's account page; without it any active impersonation is cleared and the
// browser returns to the dashboard. The admin check uses the real session
// identity (not the swapped header), so it still works mid-impersonation.
func (h *AuthHandler) HandleImpersonate(w http.ResponseWriter, r *http.Request) {
	session := h.readCookie(r)
	if session == nil {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	isAdmin := false
	for _, admin := range h.cfg.AdminUsers {
		if session.Username == admin {
			isAdmin = true
			break
		}
	}
	if !isAdmin {
		http.Redirect(w, r, "/dashboard", http.StatusFound)
		return
	}

	target := strings.TrimSpace(r.URL.Query().Get("as"))
	if target == "" {
		h.setCookie(w, session.Username, session.Groups, "")
		http.Redirect(w, r, "/dashboard", http.StatusFound)
		return
	}

	slog.Info("admin viewing as user", "admin", session.Username, "as", target)
	h.setCookie(w, session.Username, session.Groups, target)
	// /dashboard serves the personal user page while impersonating (the
	// operator dashboard is only shown for a real, non-swapped admin view).
	http.Redirect(w, r, "/dashboard", http.StatusFound)
}

func (h *AuthHandler) HandleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/login", http.StatusFound)
}

// RequireAuth gates a handler behind a valid session cookie. On success it
// sets the identity headers so downstream handlers (IsAdmin, whoami, etc.)
// work unchanged.
func (h *AuthHandler) RequireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session := h.readCookie(r)
		if session == nil {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		r.Header.Set(h.cfg.UserHeader, session.Username)
		if len(session.Groups) > 0 {
			r.Header.Set(h.cfg.GroupsHeader, fmt.Sprintf(`["%s"]`, strings.Join(session.Groups, `","`)))
		}
		// Admin "view as" mode: swap the identity header to the target user
		// so every downstream handler (per-user scoping, IsAdmin) behaves
		// exactly as it would for that user. The real identity is preserved
		// in realUserHeader for the UI banner and the impersonate-clear
		// endpoint. Non-admins can never set this — only HandleImpersonate
		// writes the claim, and the cookie is HMAC-signed.
		if session.As != "" && IsAdmin(h.cfg, r) {
			r.Header.Set(realUserHeader, session.Username)
			r.Header.Set(h.cfg.UserHeader, session.As)
		}
		next(w, r)
	}
}

// AuthenticatedUser returns the username from the session cookie, or "".
func (h *AuthHandler) AuthenticatedUser(r *http.Request) string {
	if s := h.readCookie(r); s != nil {
		return s.Username
	}
	return ""
}
