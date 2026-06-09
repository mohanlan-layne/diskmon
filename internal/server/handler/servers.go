// Package handler implements the diskmon HTTP API handlers.
package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-sql-driver/mysql"

	"diskmon/internal/alist"
	"diskmon/internal/config"
)

// ServerRow mirrors the servers table.
type ServerRow struct {
	ID        int             `json:"id"`
	ServerID  string          `json:"server_id"`
	Name      string          `json:"name"`
	SmbHost   string          `json:"smb_host"`
	SmbUser   string          `json:"smb_user"`
	SysRoot   string          `json:"sys_root"`
	Volumes   json.RawMessage `json:"volumes"`
	APIAddr   string          `json:"api_addr,omitempty"`
	AListURLs json.RawMessage `json:"alist_urls,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// ServersHandler handles /api/servers routes.
type ServersHandler struct {
	db       *sql.DB
	alistCfg config.AListConfig
}

// NewServersHandler creates a ServersHandler.
func NewServersHandler(db *sql.DB, alistCfg config.AListConfig) *ServersHandler {
	return &ServersHandler{db: db, alistCfg: alistCfg}
}

// Register mounts all servers routes onto the given router.
func (h *ServersHandler) Register(r chi.Router) {
	r.Get("/api/servers", h.list)
	r.Post("/api/servers", h.create)
	r.Get("/api/servers/{id}", h.get)
	r.Put("/api/servers/{id}", h.update)
	r.Delete("/api/servers/{id}", h.delete)
	r.Get("/api/alist/ping", h.alistPing)
}

// alistPing tests AList connectivity and returns configured URL + login result.
// GET /api/alist/ping
func (h *ServersHandler) alistPing(w http.ResponseWriter, r *http.Request) {
	if h.alistCfg.URL == "" {
		jsonOK(w, map[string]any{
			"configured": false,
			"message":    "AList 未配置（alist.url 为空）",
		})
		return
	}

	start := time.Now()
	_, err := alist.New(h.alistCfg.URL, h.alistCfg.Username, h.alistCfg.Password)
	elapsed := time.Since(start).Milliseconds()

	if err != nil {
		jsonOK(w, map[string]any{
			"configured": true,
			"ok":         false,
			"url":        h.alistCfg.URL,
			"username":   h.alistCfg.Username,
			"latency_ms": elapsed,
			"error":      err.Error(),
		})
		return
	}

	jsonOK(w, map[string]any{
		"configured": true,
		"ok":         true,
		"url":        h.alistCfg.URL,
		"username":   h.alistCfg.Username,
		"latency_ms": elapsed,
		"message":    "登录成功",
	})
}

func (h *ServersHandler) list(w http.ResponseWriter, r *http.Request) {
	rows, err := h.db.QueryContext(r.Context(),
		`SELECT id, server_id, name, smb_host, smb_user, sys_root, volumes,
		        COALESCE(api_addr,''), alist_urls, created_at, updated_at
		 FROM servers ORDER BY id`)
	if err != nil {
		jsonError(w, "query failed", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var servers []ServerRow
	for rows.Next() {
		var s ServerRow
		var volumesBytes, alistURLsBytes []byte
		if err := rows.Scan(&s.ID, &s.ServerID, &s.Name, &s.SmbHost, &s.SmbUser,
			&s.SysRoot, &volumesBytes, &s.APIAddr, &alistURLsBytes,
			&s.CreatedAt, &s.UpdatedAt); err != nil {
			jsonError(w, "scan failed", http.StatusInternalServerError)
			return
		}
		if volumesBytes != nil {
			s.Volumes = json.RawMessage(volumesBytes)
		}
		if alistURLsBytes != nil {
			s.AListURLs = json.RawMessage(alistURLsBytes)
		}
		servers = append(servers, s)
	}
	jsonOK(w, servers)
}

func (h *ServersHandler) create(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ServerID        string            `json:"server_id"`
		Name            string            `json:"name"`
		SmbHost         string            `json:"smb_host"`
		SmbUser         string            `json:"smb_user"`
		SmbPass         string            `json:"smb_pass"`
		SysRoot         string            `json:"sys_root"`
		Volumes         json.RawMessage   `json:"volumes"`
		APIAddr         string            `json:"api_addr"`
		APIToken        string            `json:"api_token"`
		AListLocalPaths map[string]string `json:"alist_local_paths"` // volume → path in AList pod
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	if body.ServerID == "" {
		jsonError(w, "server_id required", http.StatusBadRequest)
		return
	}

	ctx := r.Context()

	alistURLs, alistErr := h.registerAList(ctx, body.ServerID, body.AListLocalPaths)

	var alistURLsJSON any
	if len(alistURLs) > 0 {
		b, _ := json.Marshal(alistURLs)
		alistURLsJSON = string(b)
	}

	res, err := h.db.ExecContext(ctx, `
		INSERT INTO servers (server_id, name, smb_host, smb_user, smb_pass, sys_root,
		                     volumes, api_addr, api_token, alist_urls)
		VALUES (?,?,?,?,?,?,?,?,?,?)`,
		body.ServerID, body.Name, body.SmbHost, body.SmbUser, body.SmbPass,
		body.SysRoot, nullJSON(body.Volumes),
		nullStr(body.APIAddr), nullStr(body.APIToken), alistURLsJSON,
	)
	if err != nil {
		jsonError(w, "insert failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	id, _ := res.LastInsertId()

	if err := ensurePartition(ctx, h.db, body.ServerID); err != nil {
		_ = err
	}

	resp := map[string]any{"id": id}
	if alistErr != nil {
		resp["alist_warning"] = fmt.Sprintf("AList registration skipped: %v", alistErr)
	}
	if len(alistURLs) > 0 {
		resp["alist_urls"] = alistURLs
	}
	w.WriteHeader(http.StatusCreated)
	jsonOK(w, resp)
}

func (h *ServersHandler) get(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var s ServerRow
	var volumesBytes, alistURLsBytes []byte
	err := h.db.QueryRowContext(r.Context(),
		`SELECT id, server_id, name, smb_host, smb_user, sys_root, volumes,
		        COALESCE(api_addr,''), alist_urls, created_at, updated_at
		 FROM servers WHERE server_id=?`, id,
	).Scan(&s.ID, &s.ServerID, &s.Name, &s.SmbHost, &s.SmbUser,
		&s.SysRoot, &volumesBytes, &s.APIAddr, &alistURLsBytes,
		&s.CreatedAt, &s.UpdatedAt)
	if err != nil {
		jsonError(w, "not found", http.StatusNotFound)
		return
	}
	if volumesBytes != nil {
		s.Volumes = json.RawMessage(volumesBytes)
	}
	if alistURLsBytes != nil {
		s.AListURLs = json.RawMessage(alistURLsBytes)
	}
	jsonOK(w, s)
}

func (h *ServersHandler) update(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var body struct {
		Name     string          `json:"name"`
		SmbHost  string          `json:"smb_host"`
		SmbUser  string          `json:"smb_user"`
		SmbPass  string          `json:"smb_pass"`
		SysRoot  string          `json:"sys_root"`
		Volumes  json.RawMessage `json:"volumes"`
		APIAddr  string          `json:"api_addr"`
		APIToken string          `json:"api_token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	_, err := h.db.ExecContext(r.Context(), `
		UPDATE servers SET name=?, smb_host=?, smb_user=?, smb_pass=?,
		       sys_root=?, volumes=?, api_addr=?, api_token=?, updated_at=NOW()
		WHERE server_id=?`,
		body.Name, body.SmbHost, body.SmbUser, body.SmbPass,
		body.SysRoot, nullJSON(body.Volumes),
		nullStr(body.APIAddr), nullStr(body.APIToken), id,
	)
	if err != nil {
		jsonError(w, "update failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *ServersHandler) delete(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	ctx := r.Context()
	_, err := h.db.ExecContext(ctx, "DELETE FROM servers WHERE server_id=?", id)
	if err != nil {
		jsonError(w, "delete failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Drop the catalog partition (best-effort).
	pname := "p_" + safeName(id)
	_, _ = h.db.ExecContext(ctx,
		fmt.Sprintf("ALTER TABLE file_catalog DROP PARTITION %s", pname))
	w.WriteHeader(http.StatusNoContent)
}

// registerAList calls the AList API to create Local storages for each volume.
func (h *ServersHandler) registerAList(ctx context.Context, serverID string, localPaths map[string]string) (map[string]string, error) {
	if len(localPaths) == 0 || h.alistCfg.URL == "" || h.alistCfg.Password == "" {
		return nil, nil
	}
	ac, err := alist.New(h.alistCfg.URL, h.alistCfg.Username, h.alistCfg.Password)
	if err != nil {
		return nil, fmt.Errorf("connect alist: %w", err)
	}
	urls := make(map[string]string, len(localPaths))
	for volume, localPath := range localPaths {
		mountPath := "/" + safeName(serverID) + "-" + volumeSafe(volume)
		if _, err := ac.AddLocalStorage(ctx, mountPath, localPath); err != nil {
			return urls, fmt.Errorf("alist add %s: %w", volume, err)
		}
		urls[volume] = alist.FileURL(h.alistCfg.URL, mountPath, "")
	}
	return urls, nil
}

func ensurePartition(ctx context.Context, db *sql.DB, serverID string) error {
	pname := "p_" + safeName(serverID)
	ddl := fmt.Sprintf(
		"ALTER TABLE file_catalog ADD PARTITION (PARTITION %s VALUES IN ('%s'))",
		pname, serverID)
	_, err := db.ExecContext(ctx, ddl)
	if err != nil {
		if me, ok := err.(*mysql.MySQLError); ok && me.Number == 1517 {
			return nil // partition already exists
		}
		return err
	}
	return nil
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func volumeSafe(v string) string {
	return strings.ToLower(strings.ReplaceAll(v, ":", ""))
}
