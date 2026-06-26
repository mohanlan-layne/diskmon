package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/hirochachacha/go-smb2"

	"diskmon/internal/alist"
	"diskmon/internal/bizrule"
	"diskmon/internal/model"
)

// RefreshDirHandler scans one directory via SMB (direct connect → local mount →
// AList fallback) and upserts its immediate children into file_catalog, including
// a correct biz_key derived from the server's volume rule.
type RefreshDirHandler struct {
	db        *sql.DB
	smbMounts map[string]map[string]string // server_id → volume → local mount path
	hc        *http.Client
}

// NewRefreshDirHandler creates a RefreshDirHandler.
func NewRefreshDirHandler(db *sql.DB, smbMounts map[string]map[string]string) *RefreshDirHandler {
	return &RefreshDirHandler{
		db:        db,
		smbMounts: smbMounts,
		hc:        &http.Client{Timeout: 20 * time.Second},
	}
}

// Register mounts the endpoint (caller is responsible for auth middleware).
func (h *RefreshDirHandler) Register(r chi.Router) {
	r.Post("/api/files/refresh-dir", h.handle)
}

// handle processes POST /api/files/refresh-dir.
// Body: {"server_id":"...","path":"D:\\drawings\\folder","is_dir":true}
func (h *RefreshDirHandler) handle(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ServerID string `json:"server_id"`
		Path     string `json:"path"`
		IsDir    bool   `json:"is_dir"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	if body.ServerID == "" || body.Path == "" {
		jsonError(w, "server_id and path required", http.StatusBadRequest)
		return
	}

	dirPath := strings.TrimRight(strings.ReplaceAll(body.Path, "/", `\`), `\`)
	if !body.IsDir {
		dirPath = winParentDir(dirPath)
	}
	if dirPath == "" {
		jsonError(w, "cannot determine directory", http.StatusBadRequest)
		return
	}
	volume := winVolumeOf(dirPath)

	ctx := r.Context()

	// Load server credentials and volume config from DB.
	srv, err := h.loadServer(ctx, body.ServerID)
	if err != nil {
		jsonError(w, "server not found: "+err.Error(), http.StatusNotFound)
		return
	}

	// Build the biz_key rule for this volume (best-effort; falls back to noop).
	rule := h.buildRule(ctx, body.ServerID, volume, srv)

	// List the directory (SMB direct → local mount → AList).
	children, err := h.listDir(ctx, body.ServerID, dirPath, srv)
	if err != nil {
		jsonError(w, "list directory failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Build catalog entries.
	now := time.Now().UTC()
	entries := make([]model.FileEntry, 0, len(children)+1)
	// Always include the directory itself.
	entries = append(entries, model.FileEntry{
		ServerID:  body.ServerID,
		Volume:    volume,
		Path:      dirPath,
		IsDir:     true,
		BizKey:    rule.ExtractBizKey(dirPath),
		UpdatedAt: now,
	})
	for _, c := range children {
		childPath := dirPath + `\` + c.name
		ext := ""
		if !c.isDir {
			if e := strings.ToLower(filepath.Ext(c.name)); len(e) <= 20 {
				ext = e
			}
		}
		var size *int64
		if !c.isDir && c.size > 0 {
			s := c.size
			size = &s
		}
		entries = append(entries, model.FileEntry{
			ServerID:  body.ServerID,
			Volume:    volume,
			Path:      childPath,
			IsDir:     c.isDir,
			Size:      size,
			Ext:       ext,
			BizKey:    rule.ExtractBizKey(childPath),
			UpdatedAt: now,
		})
	}

	if err := h.upsert(ctx, entries); err != nil {
		jsonError(w, "upsert failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	jsonOK(w, map[string]any{"dir": dirPath, "entries": len(entries)})
}

// ── server info ──────────────────────────────────────────────────────────────

type serverInfo struct {
	smbHost  string
	smbShare string
	smbUser  string
	smbPass  string
	sysRoot  string
	apiAddr  string
	apiToken string
	volumes  []struct {
		Name    string `json:"name"`
		BizRule string `json:"biz_rule"`
	}
}

func (h *RefreshDirHandler) loadServer(ctx context.Context, serverID string) (serverInfo, error) {
	var s serverInfo
	var volumesJSON []byte
	err := h.db.QueryRowContext(ctx,
		`SELECT COALESCE(smb_host,''), COALESCE(smb_share,''),
		        COALESCE(smb_user,''), COALESCE(smb_pass,''),
		        COALESCE(sys_root,''), COALESCE(api_addr,''), COALESCE(api_token,''),
		        COALESCE(volumes,'[]')
		 FROM servers WHERE server_id=?`, serverID,
	).Scan(&s.smbHost, &s.smbShare, &s.smbUser, &s.smbPass,
		&s.sysRoot, &s.apiAddr, &s.apiToken, &volumesJSON)
	if err != nil {
		return s, err
	}
	_ = json.Unmarshal(volumesJSON, &s.volumes)
	return s, nil
}

// ── biz_key rule builder ─────────────────────────────────────────────────────

func (h *RefreshDirHandler) buildRule(ctx context.Context, serverID, volume string, srv serverInfo) bizrule.VolumeRule {
	var ruleName string
	for _, v := range srv.volumes {
		if strings.EqualFold(v.Name, volume) {
			ruleName = v.BizRule
			break
		}
	}
	switch ruleName {
	case "partnumber":
		return bizrule.PartNumberRule{}
	case "filename":
		return bizrule.FileNameRule{}
	case "drawings":
		root := h.fetchDrawingsRoot(ctx, srv, volume)
		if root != "" {
			return &bizrule.DrawingsRule{Root: root}
		}
	}
	return bizrule.NoopRule{}
}

// fetchDrawingsRoot calls the client's management API to find the monitored
// include_dir for the given volume (the DrawingsRule root). Returns "" on failure
// so the caller can fall back to a NoopRule rather than blocking the refresh.
func (h *RefreshDirHandler) fetchDrawingsRoot(ctx context.Context, srv serverInfo, volume string) string {
	if srv.apiAddr == "" {
		return ""
	}
	addr := srv.apiAddr
	if !strings.HasPrefix(addr, "http") {
		addr = "http://" + addr
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, addr+"/config", nil)
	if err != nil {
		return ""
	}
	if srv.apiToken != "" {
		req.Header.Set("Authorization", "Bearer "+srv.apiToken)
	}
	resp, err := h.hc.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()

	var cfg struct {
		Filters struct {
			IncludeDirs []string `json:"include_dirs"`
		} `json:"filters"`
	}
	if json.NewDecoder(resp.Body).Decode(&cfg) != nil {
		return ""
	}
	for _, dir := range cfg.Filters.IncludeDirs {
		if strings.HasPrefix(strings.ToLower(dir), strings.ToLower(volume)) {
			return dir
		}
	}
	return ""
}

// ── directory listing ─────────────────────────────────────────────────────────

type dirEntry struct {
	name  string
	isDir bool
	size  int64
}

// listDir tries AList first, then SMB direct connect, then local mount.
func (h *RefreshDirHandler) listDir(ctx context.Context, serverID, dirPath string, srv serverInfo) ([]dirEntry, error) {
	// Primary: AList /api/fs/list — single-level, non-recursive, fast.
	if mounts, err := loadAListMounts(ctx, h.db, serverID); err == nil {
		entries, err := h.listViaAList(ctx, dirPath, mounts)
		if err == nil {
			return entries, nil
		}
	}

	// Fallback 1: direct SMB2 connection using stored credentials.
	if srv.smbHost != "" && srv.smbShare != "" {
		entries, err := h.listViaSMB(dirPath, srv)
		if err == nil {
			return entries, nil
		}
	}

	// Fallback 2: local SMB mount on the server pod.
	if mounts, ok := h.smbMounts[serverID]; ok {
		entries, err := h.listViaLocalMount(dirPath, mounts)
		if err == nil {
			return entries, nil
		}
	}

	return nil, fmt.Errorf("all listing methods failed for %s", dirPath)
}

func (h *RefreshDirHandler) listViaSMB(dirPath string, srv serverInfo) ([]dirEntry, error) {
	conn, err := net.DialTimeout("tcp", srv.smbHost+":445", 10*time.Second)
	if err != nil {
		return nil, fmt.Errorf("smb dial: %w", err)
	}
	defer conn.Close()

	domain, user := parseSMBUser(srv.smbUser)
	d := &smb2.Dialer{
		Initiator: &smb2.NTLMInitiator{
			User:     user,
			Password: srv.smbPass,
			Domain:   domain,
		},
	}
	session, err := d.Dial(conn)
	if err != nil {
		return nil, fmt.Errorf("smb auth: %w", err)
	}
	defer session.Logoff() //nolint:errcheck

	fs, err := session.Mount(srv.smbShare)
	if err != nil {
		return nil, fmt.Errorf("smb mount %s: %w", srv.smbShare, err)
	}
	defer fs.Umount() //nolint:errcheck

	rel, ok := smbRelPath(dirPath, srv.sysRoot)
	if !ok {
		return nil, fmt.Errorf("path %s not under sys_root %s", dirPath, srv.sysRoot)
	}

	infos, err := fs.ReadDir(rel)
	if err != nil {
		return nil, fmt.Errorf("smb readdir %s: %w", rel, err)
	}

	entries := make([]dirEntry, len(infos))
	for i, info := range infos {
		entries[i] = dirEntry{name: info.Name(), isDir: info.IsDir(), size: info.Size()}
	}
	return entries, nil
}

func (h *RefreshDirHandler) listViaLocalMount(dirPath string, mounts map[string]string) ([]dirEntry, error) {
	vol := winVolumeOf(dirPath)
	mountRoot, ok := mounts[strings.ToUpper(vol)]
	if !ok {
		return nil, fmt.Errorf("no local mount for volume %s", vol)
	}
	rel := strings.TrimPrefix(strings.TrimPrefix(dirPath, vol), `\`)
	rel = strings.ReplaceAll(rel, `\`, string(filepath.Separator))
	infos, err := os.ReadDir(filepath.Join(mountRoot, rel))
	if err != nil {
		return nil, err
	}
	entries := make([]dirEntry, len(infos))
	for i, e := range infos {
		sz := int64(0)
		if !e.IsDir() {
			if fi, err2 := e.Info(); err2 == nil {
				sz = fi.Size()
			}
		}
		entries[i] = dirEntry{name: e.Name(), isDir: e.IsDir(), size: sz}
	}
	return entries, nil
}

func (h *RefreshDirHandler) listViaAList(ctx context.Context, dirPath string, mounts AListMounts) ([]dirEntry, error) {
	alistPath, ok := mounts.resolveURLPath(dirPath, false)
	if !ok {
		return nil, fmt.Errorf("path %s not under any AList mount", dirPath)
	}
	children, err := alist.List(ctx, h.hc, mounts.Base, alistPath)
	if err != nil {
		return nil, err
	}
	entries := make([]dirEntry, len(children))
	for i, c := range children {
		entries[i] = dirEntry{name: c.Name, isDir: c.IsDir, size: c.Size}
	}
	return entries, nil
}

// ── upsert ────────────────────────────────────────────────────────────────────

func (h *RefreshDirHandler) upsert(ctx context.Context, entries []model.FileEntry) error {
	_, err := upsertCatalog(ctx, h.db, entries)
	return err
}

// upsertCatalog is the shared package-level upsert used by RefreshDirHandler and
// ReconcileHandler. It inserts or updates file_catalog rows in chunks of 500,
// preserving existing biz_key and size values when the new ones are NULL.
// Returns the number of rows that were genuinely new inserts (not updates).
func upsertCatalog(ctx context.Context, db *sql.DB, entries []model.FileEntry) (inserted int64, err error) {
	const chunkSize = 500
	for i := 0; i < len(entries); i += chunkSize {
		end := i + chunkSize
		if end > len(entries) {
			end = len(entries)
		}
		n, e := upsertCatalogChunk(ctx, db, entries[i:end])
		inserted += n
		if e != nil {
			return inserted, e
		}
	}
	return inserted, nil
}

// upsertCatalogChunk executes one INSERT ... ON DUPLICATE KEY UPDATE batch.
// Returns new_inserts estimated via MySQL affected-rows semantics:
// new row → +1, updated row → +2, unchanged row → +0 ⟹ new = 2*N − affected.
func upsertCatalogChunk(ctx context.Context, db *sql.DB, entries []model.FileEntry) (int64, error) {
	ph := strings.Repeat("(?,?,?,?,?,?,?,?),", len(entries))
	ph = ph[:len(ph)-1]

	query := `INSERT INTO file_catalog (server_id, volume, path, is_dir, size, ext, biz_key, updated_at)
VALUES ` + ph + `
ON DUPLICATE KEY UPDATE
    is_dir=VALUES(is_dir),
    size=COALESCE(VALUES(size), size),
    ext=VALUES(ext),
    biz_key=COALESCE(VALUES(biz_key), biz_key),
    synced_at=NOW()`

	args := make([]any, 0, len(entries)*8)
	for _, e := range entries {
		args = append(args,
			e.ServerID, e.Volume, e.Path, e.IsDir, e.Size,
			nullableStr(e.Ext), nullableStr(e.BizKey), e.UpdatedAt,
		)
	}
	res, err := db.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	affected, _ := res.RowsAffected()
	newInserts := int64(2*len(entries)) - affected
	if newInserts < 0 {
		newInserts = 0
	}
	return newInserts, nil
}

func nullableStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ── helpers ───────────────────────────────────────────────────────────────────

// winVolumeOf extracts the drive letter from a Windows path, e.g. "D:\foo" → "D:".
func winVolumeOf(p string) string {
	if len(p) >= 2 && p[1] == ':' {
		return strings.ToUpper(p[:2])
	}
	return ""
}

// smbRelPath maps an absolute Windows path to its path relative to sysRoot,
// using forward slashes as required by go-smb2. Returns ("", false) when path
// is not under sysRoot.
func smbRelPath(path, sysRoot string) (string, bool) {
	sysRoot = strings.TrimRight(strings.ReplaceAll(sysRoot, "/", `\`), `\`)
	path = strings.ReplaceAll(path, "/", `\`)
	if !strings.HasPrefix(strings.ToLower(path), strings.ToLower(sysRoot)) {
		return "", false
	}
	rel := strings.TrimLeft(path[len(sysRoot):], `\`)
	return strings.ReplaceAll(rel, `\`, "/"), true
}

// parseSMBUser splits "DOMAIN\user" into (domain, user). If no domain prefix is
// present, domain is empty.
func parseSMBUser(user string) (domain, username string) {
	if i := strings.IndexByte(user, '\\'); i >= 0 {
		return user[:i], user[i+1:]
	}
	return "", user
}
