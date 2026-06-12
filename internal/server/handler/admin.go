package handler

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
)

const (
	sessionCookie = "diskmon_session"
	sessionTTL    = 12 * time.Hour
)

// AdminHandler guards the internal management UI and its configuration APIs
// (server/client config) with a single hard-configured account. Authentication
// is a login form that issues an opaque, in-memory session token stored in an
// HttpOnly cookie; tokens are lost on restart, which simply forces re-login.
type AdminHandler struct {
	user     string
	password string

	mu       sync.Mutex
	sessions map[string]time.Time // token → expiry
}

// NewAdminHandler creates the handler with the configured credentials.
func NewAdminHandler(user, password string) *AdminHandler {
	return &AdminHandler{
		user:     user,
		password: password,
		sessions: make(map[string]time.Time),
	}
}

// Register mounts the login/logout endpoints. These are public (no auth) so the
// user can reach the login form and submit credentials.
func (h *AdminHandler) Register(r chi.Router) {
	r.Post("/admin/login", h.login)
	r.Post("/admin/logout", h.logout)
}

// RequireAuth is the middleware applied to the management group. Requests
// without a valid session are redirected to the login page (for HTML routes) or
// rejected with 401 (for /api/* calls made by the page's JavaScript).
func (h *AdminHandler) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.valid(r) {
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			jsonError(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, "/admin/login", http.StatusFound)
	})
}

func (h *AdminHandler) login(w http.ResponseWriter, r *http.Request) {
	var req struct {
		User     string `json:"user"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "bad request", http.StatusBadRequest)
		return
	}
	userOK := subtle.ConstantTimeCompare([]byte(req.User), []byte(h.user)) == 1
	passOK := subtle.ConstantTimeCompare([]byte(req.Password), []byte(h.password)) == 1
	if !userOK || !passOK {
		jsonError(w, "用户名或密码错误", http.StatusUnauthorized)
		return
	}

	token := newToken()
	h.mu.Lock()
	h.sessions[token] = time.Now().Add(sessionTTL)
	h.mu.Unlock()

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionTTL.Seconds()),
	})
	jsonOK(w, map[string]bool{"ok": true})
}

func (h *AdminHandler) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		h.mu.Lock()
		delete(h.sessions, c.Value)
		h.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
	jsonOK(w, map[string]bool{"ok": true})
}

// valid reports whether the request carries a live session cookie, evicting it
// if it has expired.
func (h *AdminHandler) valid(r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	exp, ok := h.sessions[c.Value]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(h.sessions, c.Value)
		return false
	}
	return true
}

func newToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
