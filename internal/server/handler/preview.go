package handler

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"
)

// PreviewHandler builds kkFileView preview URLs via AList HTTP links.
// AList URLs are read from servers.alist_urls (written at registration time).
type PreviewHandler struct {
	db            *sql.DB
	kkFileViewURL string
}

// NewPreviewHandler creates a PreviewHandler.
func NewPreviewHandler(db *sql.DB, kkFileViewURL string) *PreviewHandler {
	return &PreviewHandler{
		db:            db,
		kkFileViewURL: strings.TrimRight(kkFileViewURL, "/"),
	}
}

// Register mounts preview routes.
func (h *PreviewHandler) Register(r chi.Router) {
	r.Get("/api/files/preview", h.preview)
}

// preview redirects to the kkFileView preview URL.
// GET /api/files/preview?server_id=server-a&path=D:\drawings\file.pdf
func (h *PreviewHandler) preview(w http.ResponseWriter, r *http.Request) {
	serverID := r.URL.Query().Get("server_id")
	winPath := r.URL.Query().Get("path")
	if serverID == "" || winPath == "" {
		jsonError(w, "server_id and path required", http.StatusBadRequest)
		return
	}

	fileURL, err := h.resolveAListURL(r, serverID, winPath)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	if h.kkFileViewURL == "" {
		jsonOK(w, map[string]string{"url": fileURL})
		return
	}

	previewURL := h.kkFileViewURL + "/onlinePreview?url=" + url.QueryEscape(fileURL)
	http.Redirect(w, r, previewURL, http.StatusFound)
}

func (h *PreviewHandler) resolveAListURL(r *http.Request, serverID, winPath string) (string, error) {
	var raw []byte
	err := h.db.QueryRowContext(r.Context(),
		"SELECT alist_urls FROM servers WHERE server_id=?", serverID,
	).Scan(&raw)
	if err != nil || len(raw) == 0 {
		return "", fmt.Errorf("no AList URLs configured for server %s", serverID)
	}

	var alistURLs map[string]string
	if err := json.Unmarshal(raw, &alistURLs); err != nil {
		return "", fmt.Errorf("invalid alist_urls for server %s", serverID)
	}

	winPath = strings.ReplaceAll(winPath, "/", `\`)
	if len(winPath) < 2 || winPath[1] != ':' {
		return "", fmt.Errorf("invalid path: %s", winPath)
	}
	volume := strings.ToUpper(winPath[:2])
	rel := strings.TrimPrefix(winPath[2:], `\`)
	rel = strings.ReplaceAll(rel, `\`, "/")

	baseURL, ok := alistURLs[volume]
	if !ok {
		return "", fmt.Errorf("no AList URL for volume %s on server %s", volume, serverID)
	}

	return strings.TrimRight(baseURL, "/") + "/" + url.PathEscape(rel), nil
}
