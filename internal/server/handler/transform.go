package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"

	"github.com/go-chi/chi/v5"
	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/types"
)

// TransformHandler handles PDF watermark and merge endpoints.
type TransformHandler struct {
	dl *DownloadHandler
}

// NewTransformHandler creates a TransformHandler that shares the DownloadHandler's
// SMB path resolution logic.
func NewTransformHandler(dl *DownloadHandler) *TransformHandler {
	return &TransformHandler{dl: dl}
}

// Register mounts transform routes.
func (h *TransformHandler) Register(r chi.Router) {
	r.Post("/api/files/watermark", h.watermark)
	r.Post("/api/files/merge", h.merge)
}

// watermark applies a text watermark to a PDF and streams the result.
// POST /api/files/watermark
// Body: {"server_id":"", "path":"D:\\...", "text":"CONFIDENTIAL", "opacity":0.3}
func (h *TransformHandler) watermark(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ServerID string  `json:"server_id"`
		Path     string  `json:"path"`
		Text     string  `json:"text"`
		Opacity  float64 `json:"opacity"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	if body.Text == "" {
		body.Text = "CONFIDENTIAL"
	}
	if body.Opacity == 0 {
		body.Opacity = 0.3
	}

	localPath, cleanup, err := h.dl.fetchLocal(r.Context(), body.ServerID, body.Path, "diskmon-wm-src-*.pdf")
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

	wmDesc := fmt.Sprintf("opacity:%g", body.Opacity)
	wm, err := api.TextWatermark(body.Text, wmDesc, true, false, types.POINTS)
	if err != nil {
		jsonError(w, "build watermark failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if err := api.AddWatermarksFile(localPath, tmp.Name(), nil, wm, nil); err != nil {
		jsonError(w, "apply watermark failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", `attachment; filename="watermarked.pdf"`)
	http.ServeFile(w, r, tmp.Name())
}

// merge merges multiple PDFs and streams the combined result.
// POST /api/files/merge
// Body: {"server_id":"", "paths":["D:\\...","D:\\..."]}
func (h *TransformHandler) merge(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ServerID string   `json:"server_id"`
		Paths    []string `json:"paths"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	if len(body.Paths) == 0 {
		jsonError(w, "paths required", http.StatusBadRequest)
		return
	}

	var localPaths []string
	var cleanups []func()
	defer func() {
		for _, c := range cleanups {
			c()
		}
	}()
	for _, p := range body.Paths {
		lp, cleanup, err := h.dl.fetchLocal(r.Context(), body.ServerID, p, "diskmon-merge-src-*.pdf")
		if err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		cleanups = append(cleanups, cleanup)
		localPaths = append(localPaths, lp)
	}

	tmp, err := os.CreateTemp("", "diskmon-merge-*.pdf")
	if err != nil {
		jsonError(w, "create temp file failed", http.StatusInternalServerError)
		return
	}
	defer os.Remove(tmp.Name())
	tmp.Close()

	if err := api.MergeCreateFile(localPaths, tmp.Name(), false, nil); err != nil {
		jsonError(w, "merge failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", `attachment; filename="merged.pdf"`)
	http.ServeFile(w, r, tmp.Name())
}
