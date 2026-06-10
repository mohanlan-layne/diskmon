package handler

import (
	"archive/zip"
	"context"
	"database/sql"
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

// maxZipFiles caps how many files a single ZIP download may contain. Larger
// selections are rejected before any bytes are written.
const maxZipFiles = 100

// DownloadHandler handles single-file download and ZIP pack endpoints.
type DownloadHandler struct {
	db *sql.DB
	// smbMounts: server_id → volume → local mount path
	// e.g. "server-A" → "D:" → "/mnt/server-a-d"
	smbMounts map[string]map[string]string
}

// NewDownloadHandler creates a DownloadHandler.
func NewDownloadHandler(db *sql.DB, smbMounts map[string]map[string]string) *DownloadHandler {
	return &DownloadHandler{db: db, smbMounts: smbMounts}
}

// Register mounts download routes.
func (h *DownloadHandler) Register(r chi.Router) {
	r.Get("/api/files/dl", h.single)
	r.Post("/api/files/zip", h.zipPack)
}

// single serves a single file. If the server has AList mounts configured it
// redirects to the AList direct-download link (hiding the system path); other-
// wise it streams the file from the server pod's SMB mount.
// GET /api/files/dl?server_id=server-A&path=E:\smb\folder1\doc1\sub\file.pdf
func (h *DownloadHandler) single(w http.ResponseWriter, r *http.Request) {
	serverID := r.URL.Query().Get("server_id")
	path := r.URL.Query().Get("path")
	if serverID == "" || path == "" {
		jsonError(w, "server_id and path required", http.StatusBadRequest)
		return
	}

	// Prefer AList direct download when configured.
	if mounts, err := loadAListMounts(r.Context(), h.db, serverID); err == nil {
		if dlURL, ok := mounts.fileURL(path); ok {
			http.Redirect(w, r, dlURL, http.StatusFound)
			return
		}
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

	// AList-backed servers: the server pod has no local SMB mount, so the file
	// bytes are streamed from AList. The file list and count come from
	// file_catalog, which lets us enforce the cap and reject up front, before
	// any response body is written.
	if mounts, err := loadAListMounts(r.Context(), h.db, body.ServerID); err == nil {
		entries, over, err := h.collectAListEntries(r.Context(), body.ServerID, body.Paths, mounts)
		if err != nil {
			jsonError(w, "枚举待打包文件失败: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if over {
			jsonError(w, fmt.Sprintf("打包文件数超过上限 %d，请缩小选择范围后再下载", maxZipFiles), http.StatusBadRequest)
			return
		}
		if len(entries) == 0 {
			jsonError(w, "没有可打包的文件", http.StatusBadRequest)
			return
		}

		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", `attachment; filename="diskmon-pack.zip"`)
		z := newAListZipper(mounts.Base)
		zw := zip.NewWriter(w)
		defer zw.Close()
		for _, e := range entries {
			_ = z.addFile(r.Context(), zw, e.alistPath, e.zipName)
		}
		return
	}

	// Fallback: local SMB mount on the server pod.
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

// collectAListEntries enumerates, from file_catalog, the files to pack for the
// given selection and resolves each to its AList virtual path. It returns
// over=true as soon as the count would exceed maxZipFiles, so the caller can
// reject without scanning everything. A selected path may be a file (matched
// exactly) or a directory (matched by prefix); empty directories yield nothing.
func (h *DownloadHandler) collectAListEntries(ctx context.Context, serverID string, paths []string, mounts AListMounts) (entries []zipEntry, over bool, err error) {
	for _, p := range paths {
		pNorm := strings.TrimRight(strings.ReplaceAll(p, "/", `\`), `\`)
		if pNorm == "" {
			continue
		}
		likePat := escapeLike(pNorm+`\`) + "%"
		// One extra row beyond the cap lets us detect overflow.
		limit := maxZipFiles + 1 - len(entries)
		rows, qerr := h.db.QueryContext(ctx,
			`SELECT path FROM file_catalog
			 WHERE server_id=? AND is_dir=0 AND (path=? OR path LIKE ? ESCAPE '\\')
			 ORDER BY path LIMIT ?`,
			serverID, pNorm, likePat, limit)
		if qerr != nil {
			return nil, false, qerr
		}
		for rows.Next() {
			var fp string
			if scanErr := rows.Scan(&fp); scanErr != nil {
				rows.Close()
				return nil, false, scanErr
			}
			alistPath, ok := mounts.resolveURLPath(fp, false)
			if !ok {
				continue
			}
			entries = append(entries, zipEntry{alistPath: alistPath, zipName: zipNameFor(pNorm, fp)})
			if len(entries) > maxZipFiles {
				rows.Close()
				return entries, true, nil
			}
		}
		if rowsErr := rows.Err(); rowsErr != nil {
			rows.Close()
			return nil, false, rowsErr
		}
		rows.Close()
	}
	return entries, false, nil
}

// zipNameFor computes the archive entry name for file fp selected via p. The
// top-level name is p's last segment, with structure preserved beneath it:
//
//	p=E:\filecenter  fp=E:\filecenter\sub\a.pdf → filecenter/sub/a.pdf
//	p=E:\x\a.pdf     fp=E:\x\a.pdf              → a.pdf
func zipNameFor(p, fp string) string {
	top := lastSegment(p)
	if strings.EqualFold(fp, p) {
		return top
	}
	if len(fp) > len(p)+1 {
		rel := fp[len(p)+1:] // strip "p\"
		return top + "/" + strings.ReplaceAll(rel, `\`, "/")
	}
	return top
}

// escapeLike escapes a string for use as a literal inside a MySQL LIKE pattern
// (with the default '\' escape character).
func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
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

// fetchLocal returns a local filesystem path for winPath plus a cleanup func to
// call when done. For AList-backed servers (no local SMB mount) it downloads the
// file to a temp file; otherwise it returns the SMB mount path directly. The
// pattern is passed to os.CreateTemp (e.g. "diskmon-wm-*.pdf").
func (h *DownloadHandler) fetchLocal(ctx context.Context, serverID, winPath, pattern string) (string, func(), error) {
	if mounts, err := loadAListMounts(ctx, h.db, serverID); err == nil {
		alistPath, ok := mounts.resolveURLPath(winPath, false)
		if !ok {
			return "", nil, fmt.Errorf("路径不在任何 AList 挂载目录下: %s", winPath)
		}
		tmp, err := os.CreateTemp("", pattern)
		if err != nil {
			return "", nil, err
		}
		cleanup := func() { os.Remove(tmp.Name()) }
		if err := newAListZipper(mounts.Base).download(ctx, alistPath, tmp); err != nil {
			tmp.Close()
			cleanup()
			return "", nil, err
		}
		if err := tmp.Close(); err != nil {
			cleanup()
			return "", nil, err
		}
		return tmp.Name(), cleanup, nil
	}

	lp, err := h.resolveSMBPath(serverID, winPath)
	if err != nil {
		return "", nil, err
	}
	return lp, func() {}, nil
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
