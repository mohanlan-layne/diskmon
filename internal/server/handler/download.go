package handler

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-chi/chi/v5"
)

// DownloadHandler handles single-file download and ZIP pack endpoints.
type DownloadHandler struct {
	// smbMounts: server_id → volume → local mount path
	// e.g. "server-A" → "D:" → "/mnt/server-a-d"
	smbMounts map[string]map[string]string
}

// NewDownloadHandler creates a DownloadHandler.
func NewDownloadHandler(smbMounts map[string]map[string]string) *DownloadHandler {
	return &DownloadHandler{smbMounts: smbMounts}
}

// Register mounts download routes.
func (h *DownloadHandler) Register(r chi.Router) {
	r.Get("/api/files/dl", h.single)
	r.Post("/api/files/zip", h.zipPack)
}

// single streams a single file from the SMB mount.
// GET /api/files/dl?server_id=server-A&path=D:\drawings\1234\file.pdf
func (h *DownloadHandler) single(w http.ResponseWriter, r *http.Request) {
	serverID := r.URL.Query().Get("server_id")
	path := r.URL.Query().Get("path")
	if serverID == "" || path == "" {
		jsonError(w, "server_id and path required", http.StatusBadRequest)
		return
	}

	localPath, err := h.resolveSMBPath(serverID, path)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	f, err := os.Open(localPath)
	if err != nil {
		jsonError(w, "file not found", http.StatusNotFound)
		return
	}
	defer f.Close()

	info, _ := f.Stat()
	w.Header().Set("Content-Disposition",
		`attachment; filename="`+filepath.Base(localPath)+`"`)
	w.Header().Set("Content-Type", "application/octet-stream")
	if info != nil {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", info.Size()))
	}
	io.Copy(w, f) //nolint:errcheck
}

// zipPack streams a ZIP archive containing the requested files.
// POST /api/files/zip
// Body: {"server_id":"server-A","paths":["D:\\drawings\\...","..."]}
func (h *DownloadHandler) zipPack(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ServerID string   `json:"server_id"`
		Paths    []string `json:"paths"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	if body.ServerID == "" || len(body.Paths) == 0 {
		jsonError(w, "server_id and paths required", http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="diskmon-pack.zip"`)

	zw := zip.NewWriter(w)
	defer zw.Close()

	for _, path := range body.Paths {
		localPath, err := h.resolveSMBPath(body.ServerID, path)
		if err != nil {
			continue
		}
		addPathToZip(zw, localPath)
	}
}

// addPathToZip adds a single file or, if localPath is a directory, every file
// beneath it — preserving the directory structure under the folder's own name
// (e.g. zipping "…/sub" yields entries "sub/a.txt", "sub/inner/b.txt").
func addPathToZip(zw *zip.Writer, localPath string) {
	info, err := os.Stat(localPath)
	if err != nil {
		return
	}
	if !info.IsDir() {
		_ = addFileToZip(zw, localPath, filepath.Base(localPath))
		return
	}

	// Walk the folder; entry names are relative to the folder's parent so the
	// folder name itself becomes the top-level directory in the archive.
	root := filepath.Dir(localPath)
	_ = filepath.WalkDir(localPath, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return nil
		}
		_ = addFileToZip(zw, p, filepath.ToSlash(rel))
		return nil
	})
}

func addFileToZip(zw *zip.Writer, localPath, zipName string) error {
	f, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer f.Close()

	w, err := zw.Create(zipName)
	if err != nil {
		return err
	}
	_, err = io.Copy(w, f)
	return err
}

// resolveSMBPath maps a Windows path (server_id + "D:\foo\bar") to its local
// mount point path (e.g. "/mnt/server-a-d/foo/bar").
func (h *DownloadHandler) resolveSMBPath(serverID, winPath string) (string, error) {
	mounts, ok := h.smbMounts[serverID]
	if !ok {
		return "", fmt.Errorf("no SMB mounts configured for server %s", serverID)
	}

	// Extract volume prefix, e.g. "D:".
	winPath = strings.ReplaceAll(winPath, "/", `\`)
	if len(winPath) < 2 || winPath[1] != ':' {
		return "", fmt.Errorf("invalid path: %s", winPath)
	}
	volume := strings.ToUpper(winPath[:2])
	rel := strings.TrimPrefix(winPath[2:], `\`)
	rel = strings.ReplaceAll(rel, `\`, string(filepath.Separator))

	mount, ok := mounts[volume]
	if !ok {
		return "", fmt.Errorf("no mount for volume %s on server %s", volume, serverID)
	}

	return filepath.Join(mount, rel), nil
}
