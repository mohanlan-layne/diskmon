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
	SmbShare  string          `json:"smb_share"`
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
	r.Post("/api/servers/{id}/alist", h.configureAList)
	r.Get("/api/alist/ping", h.alistPing)
}

// configureAList mounts each monitored directory of a server into AList as a
// separate SMB-driver storage, exposing only the leaf name (e.g. /doc1) so the
// real system path (E:\smb\folder1) stays hidden. The resulting prefix→mount
// map is saved to servers.alist_urls for preview/download resolution.
//
// POST /api/servers/{id}/alist
// Body: {"smb_share":"smb","smb_root":"E:\\smb",
//        "dirs":["E:\\smb\\folder1\\doc1","E:\\smb\\folder1\\doc2"]}
func (h *ServersHandler) configureAList(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var body struct {
		SmbShare string   `json:"smb_share"`
		SmbRoot  string   `json:"smb_root"`
		Dirs     []string `json:"dirs"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	if body.SmbShare == "" || body.SmbRoot == "" || len(body.Dirs) == 0 {
		jsonError(w, "smb_share、smb_root、dirs 均必填", http.StatusBadRequest)
		return
	}
	if h.alistCfg.URL == "" || h.alistCfg.Password == "" {
		jsonError(w, "AList 未配置（server 端 alist.url / 密码为空）", http.StatusBadRequest)
		return
	}

	ctx := r.Context()

	// SMB credentials come from the server record.
	var smbHost, smbUser, smbPass string
	if err := h.db.QueryRowContext(ctx,
		"SELECT COALESCE(smb_host,''), COALESCE(smb_user,''), COALESCE(smb_pass,'') FROM servers WHERE server_id=?", id,
	).Scan(&smbHost, &smbUser, &smbPass); err != nil {
		jsonError(w, "server 不存在或无 SMB 凭据", http.StatusBadRequest)
		return
	}
	if smbHost == "" {
		jsonError(w, "该 server 未配置 SMB 主机", http.StatusBadRequest)
		return
	}

	ac, err := alist.New(h.alistCfg.URL, h.alistCfg.Username, h.alistCfg.Password)
	if err != nil {
		jsonError(w, "连接 AList 失败: "+err.Error(), http.StatusBadGateway)
		return
	}

	// File links must be browser-reachable: prefer the public URL, fall back to
	// the internal one. Admin API calls above still use the internal URL.
	base := h.alistCfg.PublicURL
	if base == "" {
		base = h.alistCfg.URL
	}
	mounts := AListMounts{Base: strings.TrimRight(base, "/")}
	for _, dir := range body.Dirs {
		rel, ok := splitByPrefix(dir, body.SmbRoot)
		if !ok {
			jsonError(w, fmt.Sprintf("目录 %s 不在 SMB 根 %s 之下", dir, body.SmbRoot), http.StatusBadRequest)
			return
		}
		mountPath := "/" + lastSegment(dir)
		// Idempotent: drop any existing storage at this mount before recreating.
		if _, err := ac.RemoveStorageByMount(ctx, mountPath); err != nil {
			jsonError(w, fmt.Sprintf("清理旧挂载 %s 失败: %v", mountPath, err), http.StatusBadGateway)
			return
		}
		if _, err := ac.AddSMBStorage(ctx, mountPath, smbHost, body.SmbShare, rel, smbUser, smbPass); err != nil {
			jsonError(w, fmt.Sprintf("AList 挂载 %s 失败: %v", dir, err), http.StatusBadGateway)
			return
		}
		mounts.Mounts = append(mounts.Mounts, AListMount{
			Prefix: strings.ReplaceAll(strings.TrimRight(dir, `\/`), "/", `\`),
			Mount:  mountPath,
		})
	}

	guestErr := ac.EnsureGuestRead(ctx)

	stored, _ := marshalAListMounts(mounts)
	if _, err := h.db.ExecContext(ctx,
		"UPDATE servers SET alist_urls=?, sys_root=?, updated_at=NOW() WHERE server_id=?",
		stored, body.SmbRoot, id); err != nil {
		jsonError(w, "保存 alist_urls 失败: "+err.Error(), http.StatusInternalServerError)
		return
	}

	resp := map[string]any{"mounts": mounts.Mounts}
	if guestErr != nil {
		resp["alist_warning"] = "存储已建，但开启 guest 访问失败: " + guestErr.Error()
	}
	jsonOK(w, resp)
}

// lastSegment returns the final path component of a Windows/Unix path.
func lastSegment(p string) string {
	p = strings.TrimRight(strings.ReplaceAll(p, "/", `\`), `\`)
	if i := strings.LastIndex(p, `\`); i >= 0 {
		return p[i+1:]
	}
	return p
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
		`SELECT id, server_id, COALESCE(name,''), COALESCE(smb_host,''), COALESCE(smb_share,''),
		        COALESCE(smb_user,''), COALESCE(sys_root,''), volumes, COALESCE(api_addr,''), alist_urls,
		        created_at, updated_at
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
		if err := rows.Scan(&s.ID, &s.ServerID, &s.Name, &s.SmbHost, &s.SmbShare, &s.SmbUser,
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
		ServerID string          `json:"server_id"`
		Name     string          `json:"name"`
		SmbHost  string          `json:"smb_host"`
		SmbShare string          `json:"smb_share"`
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
	if body.ServerID == "" {
		jsonError(w, "server_id required", http.StatusBadRequest)
		return
	}

	ctx := r.Context()

	// AList storages are configured separately via POST /api/servers/{id}/alist.
	res, err := h.db.ExecContext(ctx, `
		INSERT INTO servers (server_id, name, smb_host, smb_share, smb_user, smb_pass, sys_root,
		                     volumes, api_addr, api_token)
		VALUES (?,?,?,?,?,?,?,?,?,?)`,
		body.ServerID, body.Name, body.SmbHost, body.SmbShare, body.SmbUser, body.SmbPass,
		body.SysRoot, nullJSON(body.Volumes),
		nullStr(body.APIAddr), nullStr(body.APIToken),
	)
	if err != nil {
		jsonError(w, "insert failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	id, _ := res.LastInsertId()

	if err := ensurePartition(ctx, h.db, body.ServerID); err != nil {
		_ = err
	}

	w.WriteHeader(http.StatusCreated)
	jsonOK(w, map[string]any{"id": id})
}

func (h *ServersHandler) get(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var s ServerRow
	var volumesBytes, alistURLsBytes []byte
	err := h.db.QueryRowContext(r.Context(),
		`SELECT id, server_id, COALESCE(name,''), COALESCE(smb_host,''), COALESCE(smb_share,''),
		        COALESCE(smb_user,''), COALESCE(sys_root,''), volumes, COALESCE(api_addr,''), alist_urls,
		        created_at, updated_at
		 FROM servers WHERE server_id=?`, id,
	).Scan(&s.ID, &s.ServerID, &s.Name, &s.SmbHost, &s.SmbShare, &s.SmbUser,
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

// update partially updates a server record: only the fields present in the body
// (non-null) are changed; omitted fields keep their current value (COALESCE), so
// editing e.g. the name never wipes the stored SMB password or API token, which
// the list endpoint does not return.
func (h *ServersHandler) update(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var body struct {
		Name     *string `json:"name"`
		SmbHost  *string `json:"smb_host"`
		SmbShare *string `json:"smb_share"`
		SmbUser  *string `json:"smb_user"`
		SmbPass  *string `json:"smb_pass"`
		SysRoot  *string `json:"sys_root"`
		APIAddr  *string `json:"api_addr"`
		APIToken *string `json:"api_token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	_, err := h.db.ExecContext(r.Context(), `
		UPDATE servers SET
		    name=COALESCE(?,name), smb_host=COALESCE(?,smb_host),
		    smb_share=COALESCE(?,smb_share), smb_user=COALESCE(?,smb_user),
		    smb_pass=COALESCE(?,smb_pass), sys_root=COALESCE(?,sys_root),
		    api_addr=COALESCE(?,api_addr), api_token=COALESCE(?,api_token),
		    updated_at=NOW()
		WHERE server_id=?`,
		body.Name, body.SmbHost, body.SmbShare, body.SmbUser, body.SmbPass,
		body.SysRoot, body.APIAddr, body.APIToken, id,
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
