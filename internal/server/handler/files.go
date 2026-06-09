package handler

import (
	"database/sql"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

// FileRow is one row from file_catalog.
type FileRow struct {
	ServerID  string     `json:"server_id"`
	Volume    string     `json:"volume"`
	Path      string     `json:"path"`
	IsDir     bool       `json:"is_dir"`
	Size      *int64     `json:"size"`
	Ext       *string    `json:"ext"`
	BizKey    *string    `json:"biz_key"`
	UpdatedAt time.Time  `json:"updated_at"`
	SyncedAt  time.Time  `json:"synced_at"`
}

// FilesHandler handles /api/files routes.
type FilesHandler struct {
	db *sql.DB
}

// NewFilesHandler creates a FilesHandler.
func NewFilesHandler(db *sql.DB) *FilesHandler {
	return &FilesHandler{db: db}
}

// Register mounts file query routes.
func (h *FilesHandler) Register(r chi.Router) {
	r.Get("/api/files", h.list)
}

// list handles GET /api/files with optional filters:
//
//	server_id, biz_key, ext, is_dir, path_prefix, limit, offset
func (h *FilesHandler) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	serverID := q.Get("server_id")
	bizKey := q.Get("biz_key")
	ext := q.Get("ext")
	pathPrefix := q.Get("path_prefix")
	isDirStr := q.Get("is_dir")

	limit := 100
	offset := 0
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			if n > 1000 {
				n = 1000
			}
			limit = n
		}
	}
	if v := q.Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			offset = n
		}
	}

	var where []string
	var args []any

	if serverID != "" {
		where = append(where, "server_id=?")
		args = append(args, serverID)
	}
	if bizKey != "" {
		where = append(where, "biz_key=?")
		args = append(args, bizKey)
	}
	if ext != "" {
		where = append(where, "ext=?")
		args = append(args, ext)
	}
	if pathPrefix != "" {
		where = append(where, "path LIKE ?")
		args = append(args, pathPrefix+"%")
	}
	if isDirStr != "" {
		isDir := isDirStr == "true" || isDirStr == "1"
		where = append(where, "is_dir=?")
		args = append(args, isDir)
	}

	query := `SELECT server_id, volume, path, is_dir, size, ext, biz_key, updated_at, synced_at
	          FROM file_catalog`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY path LIMIT ? OFFSET ?"
	args = append(args, limit, offset)

	rows, err := h.db.QueryContext(r.Context(), query, args...)
	if err != nil {
		jsonError(w, "query failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var files []FileRow
	for rows.Next() {
		var f FileRow
		var size sql.NullInt64
		var extVal, bizKey sql.NullString
		if err := rows.Scan(&f.ServerID, &f.Volume, &f.Path, &f.IsDir, &size,
			&extVal, &bizKey, &f.UpdatedAt, &f.SyncedAt); err != nil {
			jsonError(w, "scan failed", http.StatusInternalServerError)
			return
		}
		if size.Valid {
			f.Size = &size.Int64
		}
		if extVal.Valid {
			f.Ext = &extVal.String
		}
		if bizKey.Valid {
			f.BizKey = &bizKey.String
		}
		files = append(files, f)
	}
	jsonOK(w, files)
}
