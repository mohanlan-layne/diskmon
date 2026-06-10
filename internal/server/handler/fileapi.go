package handler

import (
	"context"
	"database/sql"
	"encoding/base64"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/types"
)

// FileHandler exposes per-file operations keyed by the catalog file id (globally
// unique), so external callers never deal with physical paths: download,
// preview and watermark.
type FileHandler struct {
	db    *sql.DB
	dl    *DownloadHandler
	kkURL string // browser-reachable kkFileView base for preview
}

// NewFileHandler creates a FileHandler.
func NewFileHandler(db *sql.DB, dl *DownloadHandler, kkURL string) *FileHandler {
	return &FileHandler{db: db, dl: dl, kkURL: strings.TrimRight(kkURL, "/")}
}

// Register mounts the file-id routes.
func (h *FileHandler) Register(r chi.Router) {
	r.Get("/api/file/{id}/download", h.download)
	r.Get("/api/file/{id}/preview", h.preview)
	r.Get("/api/file/{id}/watermark", h.watermark)
}

// lookupFile resolves a catalog file id to its server and physical path.
func lookupFile(ctx context.Context, db *sql.DB, id string) (serverID, path, ext string, isDir bool, err error) {
	err = db.QueryRowContext(ctx,
		"SELECT server_id, path, COALESCE(ext,''), is_dir FROM file_catalog WHERE id=? LIMIT 1", id,
	).Scan(&serverID, &path, &ext, &isDir)
	return
}

// download streams/redirects a file by id.
// GET /api/file/{id}/download
func (h *FileHandler) download(w http.ResponseWriter, r *http.Request) {
	serverID, path, _, _, err := lookupFile(r.Context(), h.db, chi.URLParam(r, "id"))
	if err != nil {
		jsonError(w, "文件不存在", http.StatusNotFound)
		return
	}
	h.dl.serveByPath(w, r, serverID, path)
}

// preview redirects to the kkFileView preview of a file by id.
// GET /api/file/{id}/preview
func (h *FileHandler) preview(w http.ResponseWriter, r *http.Request) {
	serverID, path, _, _, err := lookupFile(r.Context(), h.db, chi.URLParam(r, "id"))
	if err != nil {
		jsonError(w, "文件不存在", http.StatusNotFound)
		return
	}
	mounts, err := loadAListMounts(r.Context(), h.db, serverID)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	fileURL, ok := mounts.previewFileURL(path)
	if !ok {
		jsonError(w, "该文件不在任何 AList 挂载目录下", http.StatusBadRequest)
		return
	}
	if h.kkURL == "" {
		jsonOK(w, map[string]string{"url": fileURL})
		return
	}
	encoded := base64.StdEncoding.EncodeToString([]byte(fileURL))
	http.Redirect(w, r, h.kkURL+"/onlinePreview?url="+url.QueryEscape(encoded), http.StatusFound)
}

// watermark applies a text watermark to a PDF by id and streams the result.
// GET /api/file/{id}/watermark?text=机密&size=48&pos=c&opacity=0.3&rotation=45
func (h *FileHandler) watermark(w http.ResponseWriter, r *http.Request) {
	text := r.URL.Query().Get("text")
	if text == "" {
		jsonError(w, "text 必填", http.StatusBadRequest)
		return
	}
	serverID, path, ext, _, err := lookupFile(r.Context(), h.db, chi.URLParam(r, "id"))
	if err != nil {
		jsonError(w, "文件不存在", http.StatusNotFound)
		return
	}
	if strings.ToLower(ext) != ".pdf" {
		jsonError(w, "水印仅支持 PDF 文件", http.StatusBadRequest)
		return
	}

	q := r.URL.Query()
	size, _ := strconv.Atoi(q.Get("size"))
	opacity, _ := strconv.ParseFloat(q.Get("opacity"), 64)
	desc := watermarkDesc(size, q.Get("pos"), opacity, q.Get("rotation"))

	src, cleanup, err := h.dl.fetchLocal(r.Context(), serverID, path, "diskmon-wm-src-*.pdf")
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	defer cleanup()

	tmp, err := os.CreateTemp("", "diskmon-wm-*.pdf")
	if err != nil {
		jsonError(w, "create temp file failed", http.StatusInternalServerError)
		return
	}
	defer os.Remove(tmp.Name())
	tmp.Close()

	wm, err := api.TextWatermark(text, desc, true, false, types.POINTS)
	if err != nil {
		jsonError(w, "build watermark failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := api.AddWatermarksFile(src, tmp.Name(), nil, wm, nil); err != nil {
		jsonError(w, "apply watermark failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", `attachment; filename="watermarked.pdf"`)
	http.ServeFile(w, r, tmp.Name())
}

// watermarkDesc builds a pdfcpu watermark description from the request params.
func watermarkDesc(size int, pos string, opacity float64, rotation string) string {
	var parts []string
	if size > 0 {
		parts = append(parts, "points:"+strconv.Itoa(size))
	}
	if pos == "" {
		pos = "c"
	}
	parts = append(parts, "pos:"+pos)
	if opacity <= 0 || opacity > 1 {
		opacity = 0.3
	}
	parts = append(parts, "opacity:"+strconv.FormatFloat(opacity, 'g', -1, 64))
	if rotation != "" {
		parts = append(parts, "rotation:"+rotation)
	}
	return strings.Join(parts, ", ")
}
