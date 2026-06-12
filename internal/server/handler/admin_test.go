package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// newAdminRouter wires an AdminHandler the same way main.go does: public
// login/logout endpoints plus a protected group containing a page route and an
// /api/* route.
func newAdminRouter() (*AdminHandler, http.Handler) {
	h := NewAdminHandler("admin", "secret")
	r := chi.NewRouter()
	h.Register(r)
	r.Group(func(r chi.Router) {
		r.Use(h.RequireAuth)
		r.Get("/admin/", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("page")) })
		r.Get("/api/servers", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("[]")) })
	})
	return h, r
}

func TestAdminAuthFlow(t *testing.T) {
	_, r := newAdminRouter()
	srv := httptest.NewServer(r)
	defer srv.Close()

	// No cookie: page route redirects, api route 401s.
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	resp, err := noRedirect.Get(srv.URL + "/admin/")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("page without session: got %d, want 302", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/admin/login" {
		t.Fatalf("redirect target = %q, want /admin/login", loc)
	}

	resp, _ = noRedirect.Get(srv.URL + "/api/servers")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("api without session: got %d, want 401", resp.StatusCode)
	}

	// Wrong password is rejected.
	resp, _ = http.Post(srv.URL+"/admin/login", "application/json", strings.NewReader(`{"user":"admin","password":"nope"}`))
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad login: got %d, want 401", resp.StatusCode)
	}

	// Correct login issues a session cookie that unlocks both routes.
	jar := &http.Client{}
	resp, err = jar.Post(srv.URL+"/admin/login", "application/json", strings.NewReader(`{"user":"admin","password":"secret"}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("good login: got %d, want 200", resp.StatusCode)
	}
	var token *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			token = c
		}
	}
	if token == nil {
		t.Fatal("login did not set a session cookie")
	}

	authed := func(path string) int {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		req.AddCookie(token)
		rr, _ := jar.Do(req)
		return rr.StatusCode
	}
	if got := authed("/admin/"); got != http.StatusOK {
		t.Fatalf("page with session: got %d, want 200", got)
	}
	if got := authed("/api/servers"); got != http.StatusOK {
		t.Fatalf("api with session: got %d, want 200", got)
	}

	// Logout invalidates the session.
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/admin/logout", nil)
	req.AddCookie(token)
	if _, err := jar.Do(req); err != nil {
		t.Fatal(err)
	}
	if got := authed("/api/servers"); got != http.StatusUnauthorized {
		t.Fatalf("api after logout: got %d, want 401", got)
	}
}
