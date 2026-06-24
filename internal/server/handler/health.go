package handler

import (
	"database/sql"
	"net/http"

	"github.com/go-chi/chi/v5"
)

type HealthHandler struct {
	db *sql.DB
}

func NewHealthHandler(db *sql.DB) *HealthHandler {
	return &HealthHandler{db: db}
}

func (h *HealthHandler) Register(r chi.Router) {
	r.Get("/healthz", h.healthz)
}

func (h *HealthHandler) healthz(w http.ResponseWriter, r *http.Request) {
	if err := h.db.PingContext(r.Context()); err != nil {
		jsonError(w, "db unreachable", http.StatusServiceUnavailable)
		return
	}
	jsonOK(w, map[string]string{"status": "ok"})
}
