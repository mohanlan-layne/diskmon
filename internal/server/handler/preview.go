package handler

import (
	"database/sql"
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

// preview redirects to the kkFileView preview URL, resolving the file through
// the server's AList SMB mounts.
// GET /api/files/preview?server_id=server-a&path=E:\smb\folder1\doc1\sub\a.pdf
func (h *PreviewHandler) preview(w http.ResponseWriter, r *http.Request) {
	serverID := r.URL.Query().Get("server_id")
	winPath := r.URL.Query().Get("path")
	if serverID == "" || winPath == "" {
		jsonError(w, "server_id and path required", http.StatusBadRequest)
		return
	}

	mounts, err := loadAListMounts(r.Context(), h.db, serverID)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	fileURL, ok := mounts.fileURL(winPath)
	if !ok {
		jsonError(w, "该路径不在任何 AList 挂载目录下: "+winPath, http.StatusBadRequest)
		return
	}

	if h.kkFileViewURL == "" {
		jsonOK(w, map[string]string{"url": fileURL})
		return
	}

	previewURL := h.kkFileViewURL + "/onlinePreview?url=" + url.QueryEscape(fileURL)
	http.Redirect(w, r, previewURL, http.StatusFound)
}
