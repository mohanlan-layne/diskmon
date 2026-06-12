package handler

import (
	"archive/zip"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
)

// maxBizFiles caps how many files a single (multi-)part-number ZIP may contain.
const maxBizFiles = 500

// BizHandler exposes part-number (biz_key) oriented endpoints for external
// systems (ERP/MES/AI). All file lookups go through biz_key + server_id; no
// physical path is exposed.
type BizHandler struct {
	db *sql.DB
	dl *DownloadHandler
}

// NewBizHandler creates a BizHandler.
func NewBizHandler(db *sql.DB, dl *DownloadHandler) *BizHandler {
	return &BizHandler{db: db, dl: dl}
}

// Register mounts the part-number routes.
func (h *BizHandler) Register(r chi.Router) {
	r.Get("/api/biz/{server_id}/{biz_key}/files", h.files)
	r.Get("/api/biz/{server_id}/{biz_key}/zip", h.zipOne)
	r.Post("/api/biz/zip", h.zipMany)
	r.Post("/api/biz/merge-pdf", h.mergePDF)
}

// bizFile is one file entry returned by the files endpoint.
type bizFile struct {
	ID       int64     `json:"id"`
	Name     string    `json:"name"`
	Ext      string    `json:"ext"`
	Size     *int64    `json:"size"`
	Modified time.Time `json:"modified"`
	Rel      string    `json:"rel"` // path relative to the part-number folder
}

// files lists every file under a part number, optionally filtered by extension.
// GET /api/biz/{server_id}/{biz_key}/files?ext=.pdf,.nc
func (h *BizHandler) files(w http.ResponseWriter, r *http.Request) {
	serverID := chi.URLParam(r, "server_id")
	bizKey := chi.URLParam(r, "biz_key")
	exts := normExts(r.URL.Query().Get("ext"))

	folder, err := latestBizFolder(r.Context(), h.db, serverID, bizKey)
	if err == sql.ErrNoRows || folder == "" {
		jsonOK(w, []bizFile{})
		return
	}
	if err != nil {
		jsonError(w, "query failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	rows, err := h.filesUnder(r.Context(), serverID, folder, exts)
	if err != nil {
		jsonError(w, "query failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var out []bizFile
	for rows.Next() {
		var f bizFile
		var p, ext string
		var size sql.NullInt64
		if err := rows.Scan(&f.ID, &p, &ext, &size, &f.Modified); err != nil {
			jsonError(w, "scan failed", http.StatusInternalServerError)
			return
		}
		f.Name = lastSegment(p)
		f.Ext = ext
		if size.Valid {
			f.Size = &size.Int64
		}
		f.Rel = relTo(folder, p)
		out = append(out, f)
	}
	jsonOK(w, out)
}

// zipOne packs a single part number.
// GET /api/biz/{server_id}/{biz_key}/zip?ext=.pdf
func (h *BizHandler) zipOne(w http.ResponseWriter, r *http.Request) {
	serverID := chi.URLParam(r, "server_id")
	bizKey := chi.URLParam(r, "biz_key")
	h.streamZip(w, r, serverID, []string{bizKey}, normExts(r.URL.Query().Get("ext")), bizKey)
}

// zipMany packs multiple part numbers.
// POST /api/biz/zip  {"server_id":"","biz_keys":["..."],"ext":[".pdf"]}
func (h *BizHandler) zipMany(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ServerID string   `json:"server_id"`
		BizKeys  []string `json:"biz_keys"`
		Ext      []string `json:"ext"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	if body.ServerID == "" || len(body.BizKeys) == 0 {
		jsonError(w, "server_id 和 biz_keys 必填", http.StatusBadRequest)
		return
	}
	h.streamZip(w, r, body.ServerID, body.BizKeys, normExtSlice(body.Ext), "diskmon-pack")
}

// streamZip enumerates files for the given part numbers (≤ maxBizFiles), then
// streams a ZIP named <name>.zip with each part number under its own folder.
func (h *BizHandler) streamZip(w http.ResponseWriter, r *http.Request, serverID string, bizKeys, exts []string, name string) {
	mounts, err := loadAListMounts(r.Context(), h.db, serverID)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	entries, over, err := h.collectBizEntries(r.Context(), serverID, bizKeys, exts, mounts)
	if err != nil {
		jsonError(w, "枚举待打包文件失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if over {
		jsonError(w, fmt.Sprintf("文件数超过上限 %d，请缩小范围", maxBizFiles), http.StatusBadRequest)
		return
	}
	if len(entries) == 0 {
		jsonError(w, "没有可打包的文件", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.zip"`, name))
	z := newAListZipper(mounts.Base)
	zw := zip.NewWriter(w)
	defer zw.Close()
	for _, e := range entries {
		_ = z.addFile(r.Context(), zw, e.alistPath, e.zipName)
	}
}

// collectBizEntries lists the files to pack for the given part numbers, naming
// each entry under its part-number folder. Stops with over=true past maxBizFiles.
func (h *BizHandler) collectBizEntries(ctx context.Context, serverID string, bizKeys, exts []string, mounts AListMounts) (entries []zipEntry, over bool, err error) {
	for _, bk := range bizKeys {
		folder, ferr := latestBizFolder(ctx, h.db, serverID, bk)
		if ferr == sql.ErrNoRows || folder == "" {
			continue
		}
		if ferr != nil {
			return nil, false, ferr
		}

		rows, qerr := h.filesUnder(ctx, serverID, folder, exts)
		if qerr != nil {
			return nil, false, qerr
		}
		for rows.Next() {
			var id int64
			var p, ext string
			var size sql.NullInt64
			var mod time.Time
			if scanErr := rows.Scan(&id, &p, &ext, &size, &mod); scanErr != nil {
				rows.Close()
				return nil, false, scanErr
			}
			alistPath, ok := mounts.resolveURLPath(p, false)
			if !ok {
				continue
			}
			entries = append(entries, zipEntry{alistPath: alistPath, zipName: zipNameFor(folder, p)})
			if len(entries) > maxBizFiles {
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

// mergePDF merges the latest PDF of each part number into one PDF, in order.
// POST /api/biz/merge-pdf  {"server_id":"","biz_keys":["...","..."]}
func (h *BizHandler) mergePDF(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ServerID string   `json:"server_id"`
		BizKeys  []string `json:"biz_keys"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	if body.ServerID == "" || len(body.BizKeys) == 0 {
		jsonError(w, "server_id 和 biz_keys 必填", http.StatusBadRequest)
		return
	}

	var localPaths []string
	var cleanups []func()
	defer func() {
		for _, c := range cleanups {
			c()
		}
	}()

	for _, bk := range body.BizKeys {
		var p string
		err := h.db.QueryRowContext(r.Context(),
			`SELECT path FROM file_catalog
			 WHERE server_id=? AND biz_key=? AND is_dir=0 AND LOWER(ext)='.pdf'
			 ORDER BY updated_at DESC LIMIT 1`, body.ServerID, bk).Scan(&p)
		if err == sql.ErrNoRows {
			continue // no PDF for this part number → skip
		}
		if err != nil {
			jsonError(w, "query failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		lp, cleanup, err := h.dl.fetchLocal(r.Context(), body.ServerID, p, "diskmon-merge-src-*.pdf")
		if err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		cleanups = append(cleanups, cleanup)
		localPaths = append(localPaths, lp)
	}
	if len(localPaths) == 0 {
		jsonError(w, "这些料号下没有 PDF 文件", http.StatusNotFound)
		return
	}

	tmp, err := os.CreateTemp("", "diskmon-merged-*.pdf")
	if err != nil {
		jsonError(w, "create temp file failed", http.StatusInternalServerError)
		return
	}
	defer os.Remove(tmp.Name())
	tmp.Close()

	// Skip PDFs that can't be parsed (some source files are corrupt) so one bad
	// file doesn't fail the whole merge.
	conf := model.NewDefaultConfiguration()
	conf.ValidationMode = model.ValidationRelaxed
	var valid []string
	for _, lp := range localPaths {
		if api.ValidateFile(lp, conf) == nil {
			valid = append(valid, lp)
		}
	}
	if len(valid) == 0 {
		jsonError(w, "这些料号的 PDF 均无法解析（可能损坏）", http.StatusUnprocessableEntity)
		return
	}
	if err := api.MergeCreateFile(valid, tmp.Name(), false, conf); err != nil {
		jsonError(w, "merge failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", `attachment; filename="merged.pdf"`)
	http.ServeFile(w, r, tmp.Name())
}

// latestBizFolder returns the most recent part-number folder for a biz_key.
//
// A part number may recur in many directories (different month/operator/batch).
// We take the file with the newest updated_at, then its part-number folder — the
// shortest same-biz_key ancestor directory of that file (i.e. the "料号" level,
// not its FM/ZM sub-folders). Callers then operate on just this one folder, so
// listing/zipping reflects the latest processing and paths stay consistent.
func latestBizFolder(ctx context.Context, db *sql.DB, serverID, bizKey string) (string, error) {
	var latest string
	err := db.QueryRowContext(ctx,
		`SELECT path FROM file_catalog
		 WHERE server_id=? AND biz_key=? AND is_dir=0 AND size>0
		 ORDER BY updated_at DESC LIMIT 1`, serverID, bizKey).Scan(&latest)
	if err != nil {
		return "", err
	}
	// Shortest same-biz_key path that is an ancestor directory of the newest file.
	// We deliberately do NOT filter on is_dir: a client bug records some
	// directories created via rename as is_dir=0 (the directory flag is lost when
	// an unpaired rename is flushed as a create), so relying on is_dir would miss
	// those part-number folders. LOCATE(CONCAT(path,'\'), latest)=1 means path+'\'
	// is a prefix of the file path, which can only hold for a real ancestor
	// directory — so this stays correct regardless of the (possibly wrong) is_dir.
	var folder string
	err = db.QueryRowContext(ctx,
		`SELECT path FROM file_catalog
		 WHERE server_id=? AND biz_key=?
		   AND LOCATE(CONCAT(path,'\\'), ?)=1
		 ORDER BY CHAR_LENGTH(path) LIMIT 1`, serverID, bizKey, latest).Scan(&folder)
	return folder, err
}

// filesUnder lists the non-dir files directly within folder (recursively),
// optionally filtered by extension. It matches by path prefix so only this one
// part-number folder's files are returned.
func (h *BizHandler) filesUnder(ctx context.Context, serverID, folder string, exts []string) (*sql.Rows, error) {
	// size>0 excludes directories that the client mis-recorded as is_dir=0 (they
	// carry a NULL/0 size), so a mislabeled sub-folder is never packed as a file.
	q := `SELECT id, path, COALESCE(ext,''), size, updated_at FROM file_catalog
	      WHERE server_id=? AND is_dir=0 AND size>0 AND LOCATE(CONCAT(?,'\\'), path)=1`
	args := []any{serverID, folder}
	if len(exts) > 0 {
		q += " AND LOWER(ext) IN (" + placeholders(len(exts)) + ")"
		for _, e := range exts {
			args = append(args, e)
		}
	}
	q += " ORDER BY path LIMIT 5000"
	return h.db.QueryContext(ctx, q, args...)
}

// relTo returns path relative to folder, using forward slashes.
func relTo(folder, path string) string {
	if folder != "" && len(path) > len(folder) && strings.EqualFold(path[:len(folder)], folder) {
		return strings.ReplaceAll(strings.TrimLeft(path[len(folder):], `\`), `\`, "/")
	}
	return lastSegment(path)
}

// normExts parses a comma-separated ext list ("pdf,.NC") into normalised,
// lower-cased, dot-prefixed extensions ([".pdf",".nc"]).
func normExts(s string) []string {
	if s == "" {
		return nil
	}
	return normExtSlice(strings.Split(s, ","))
}

func normExtSlice(in []string) []string {
	var out []string
	for _, e := range in {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" {
			continue
		}
		if !strings.HasPrefix(e, ".") {
			e = "." + e
		}
		out = append(out, e)
	}
	return out
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}
