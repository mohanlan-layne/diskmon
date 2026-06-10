package handler

import (
	"archive/zip"
	"database/sql"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
)

// BizHandler exposes part-number (biz_key) oriented endpoints for external
// systems (ERP/MES): list and one-shot ZIP of all files under a part number,
// without the caller needing to know individual file paths.
type BizHandler struct {
	db *sql.DB
	dl *DownloadHandler
}

// NewBizHandler creates a BizHandler. It reuses the DownloadHandler's AList ZIP
// streaming and file-enumeration logic.
func NewBizHandler(db *sql.DB, dl *DownloadHandler) *BizHandler {
	return &BizHandler{db: db, dl: dl}
}

// Register mounts the part-number routes.
func (h *BizHandler) Register(r chi.Router) {
	r.Get("/api/biz/{server_id}/{biz_key}/files", h.files)
	r.Get("/api/biz/{server_id}/{biz_key}/zip", h.zip)
}

// files returns every catalog entry under a part number.
// GET /api/biz/{server_id}/{biz_key}/files
func (h *BizHandler) files(w http.ResponseWriter, r *http.Request) {
	serverID := chi.URLParam(r, "server_id")
	bizKey := chi.URLParam(r, "biz_key")

	rows, err := h.db.QueryContext(r.Context(),
		`SELECT server_id, volume, path, is_dir, size, ext, biz_key, updated_at, synced_at
		 FROM file_catalog WHERE server_id=? AND biz_key=? ORDER BY path LIMIT 2000`,
		serverID, bizKey)
	if err != nil {
		jsonError(w, "query failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var files []FileRow
	for rows.Next() {
		var f FileRow
		var size sql.NullInt64
		var extVal, bk sql.NullString
		if err := rows.Scan(&f.ServerID, &f.Volume, &f.Path, &f.IsDir, &size,
			&extVal, &bk, &f.UpdatedAt, &f.SyncedAt); err != nil {
			jsonError(w, "scan failed", http.StatusInternalServerError)
			return
		}
		if size.Valid {
			f.Size = &size.Int64
		}
		if extVal.Valid {
			f.Ext = &extVal.String
		}
		if bk.Valid {
			f.BizKey = &bk.String
		}
		files = append(files, f)
	}
	jsonOK(w, files)
}

// zip streams a ZIP of every file under a part number, named <biz_key>.zip.
// GET /api/biz/{server_id}/{biz_key}/zip
func (h *BizHandler) zip(w http.ResponseWriter, r *http.Request) {
	serverID := chi.URLParam(r, "server_id")
	bizKey := chi.URLParam(r, "biz_key")

	// The part-number folder is the shortest directory record carrying this
	// biz_key; packing it preserves the folder structure under its own name.
	var folder string
	err := h.db.QueryRowContext(r.Context(),
		`SELECT path FROM file_catalog
		 WHERE server_id=? AND biz_key=? AND is_dir=1
		 ORDER BY CHAR_LENGTH(path) LIMIT 1`, serverID, bizKey).Scan(&folder)
	if err == sql.ErrNoRows || folder == "" {
		jsonError(w, "料号不存在或无文件: "+bizKey, http.StatusNotFound)
		return
	}
	if err != nil {
		jsonError(w, "query failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	mounts, err := loadAListMounts(r.Context(), h.db, serverID)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	entries, over, err := h.dl.collectAListEntries(r.Context(), serverID, []string{folder}, mounts)
	if err != nil {
		jsonError(w, "枚举待打包文件失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if over {
		jsonError(w, fmt.Sprintf("料号 %s 文件数超过上限 %d", bizKey, maxZipFiles), http.StatusBadRequest)
		return
	}
	if len(entries) == 0 {
		jsonError(w, "料号无可打包文件: "+bizKey, http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.zip"`, bizKey))
	z := newAListZipper(mounts.Base)
	zw := zip.NewWriter(w)
	defer zw.Close()
	for _, e := range entries {
		_ = z.addFile(r.Context(), zw, e.alistPath, e.zipName)
	}
}
