package handler

import (
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/go-chi/chi/v5"
)

// ClientsHandler proxies management calls to the embedded client API.
type ClientsHandler struct {
	db   *sql.DB
	http *http.Client
}

// NewClientsHandler creates a ClientsHandler.
func NewClientsHandler(db *sql.DB) *ClientsHandler {
	return &ClientsHandler{
		db:   db,
		http: &http.Client{Timeout: 30 * time.Second},
	}
}

// Register mounts client management routes.
func (h *ClientsHandler) Register(r chi.Router) {
	r.Get("/api/clients/{id}/logs", h.logs)
	r.Post("/api/clients/{id}/rescan", h.rescan)
	r.Post("/api/clients/{id}/restart", h.restart)
	r.Get("/api/clients/{id}/health", h.health)
}

func (h *ClientsHandler) logs(w http.ResponseWriter, r *http.Request) {
	lines := r.URL.Query().Get("lines")
	targetURL, token, err := h.clientURL(r, chi.URLParam(r, "id"), "/logs")
	if err != nil {
		jsonError(w, err.Error(), http.StatusNotFound)
		return
	}
	if lines != "" {
		targetURL += "?lines=" + url.QueryEscape(lines)
	}
	h.proxy(w, r, http.MethodGet, targetURL, token)
}

func (h *ClientsHandler) rescan(w http.ResponseWriter, r *http.Request) {
	targetURL, token, err := h.clientURL(r, chi.URLParam(r, "id"), "/rescan")
	if err != nil {
		jsonError(w, err.Error(), http.StatusNotFound)
		return
	}
	h.proxy(w, r, http.MethodPost, targetURL, token)
}

func (h *ClientsHandler) restart(w http.ResponseWriter, r *http.Request) {
	targetURL, token, err := h.clientURL(r, chi.URLParam(r, "id"), "/restart")
	if err != nil {
		jsonError(w, err.Error(), http.StatusNotFound)
		return
	}
	h.proxy(w, r, http.MethodPost, targetURL, token)
}

func (h *ClientsHandler) health(w http.ResponseWriter, r *http.Request) {
	targetURL, token, err := h.clientURL(r, chi.URLParam(r, "id"), "/health")
	if err != nil {
		jsonError(w, err.Error(), http.StatusNotFound)
		return
	}
	h.proxy(w, r, http.MethodGet, targetURL, token)
}

func (h *ClientsHandler) clientURL(r *http.Request, serverID, path string) (string, string, error) {
	var apiAddr, apiToken string
	err := h.db.QueryRowContext(r.Context(),
		`SELECT COALESCE(api_addr,''), COALESCE(api_token,'') FROM servers WHERE server_id=?`,
		serverID,
	).Scan(&apiAddr, &apiToken)
	if err != nil || apiAddr == "" {
		return "", "", fmt.Errorf("client not found or api_addr not set for %s", serverID)
	}
	return "http://" + apiAddr + path, apiToken, nil
}

func (h *ClientsHandler) proxy(w http.ResponseWriter, r *http.Request, method, targetURL, token string) {
	req, err := http.NewRequestWithContext(r.Context(), method, targetURL, nil)
	if err != nil {
		jsonError(w, "build request: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := h.http.Do(req)
	if err != nil {
		jsonError(w, "client unreachable: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body) //nolint:errcheck
}
