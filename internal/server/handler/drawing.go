package handler

// DrawingHandler implements the drawing-library API used by the Donguan drawing
// system (东莞图档库). These routes replace the old PrintServer.jar and are accessed
// via nginx path-rewriting that injects server_id, so the Saber3 frontend is unchanged.
//
// nginx rewrite pattern (东莞图档库 serverId hard-coded in nginx, e.g. "dongguan-srv"):
//   GET  /drawing/{料号}                 → GET  /api/drawing/{serverId}/{料号}
//   POST /pdf-printing/join              → POST /api/pdf-printing/{serverId}/join
//   GET  /pdf-printing/print/{name}      → GET  /api/pdf-printing/{serverId}/print/{name}
//   POST /pdf-printing/zip               → POST /api/pdf-printing/{serverId}/zip
//   GET  /pdf-printing/zip-download/{name} → GET /api/pdf-printing/{serverId}/zip-download/{name}

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/types"
)

//go:embed assets/NotoSansSC-Regular.ttf
var notoSansSCFont []byte

// cjkFontName is the PostScript name the embedded font registers as in pdfcpu.
const cjkFontName = "NotoSansSC-Regular"

// cjkFontOnce guards one-time installation of the embedded CJK font into
// pdfcpu's user-font registry so TextWatermark can embed it for Chinese text.
var (
	cjkFontOnce sync.Once
	cjkFontErr  error
)

// initCJKFont installs the embedded NotoSansSC-Regular into pdfcpu's user font
// dir (idempotent). It first ensures the default configuration is loaded so
// font.UserFontDir is set, then writes the TTF to a temp file and installs it.
func initCJKFont() error {
	cjkFontOnce.Do(func() {
		// Loading the default config sets font.UserFontDir, required by InstallFonts.
		_ = model.NewDefaultConfiguration()

		dir, err := os.MkdirTemp("", "diskmon-fonts-*")
		if err != nil {
			cjkFontErr = err
			return
		}
		defer os.RemoveAll(dir)

		fontPath := filepath.Join(dir, cjkFontName+".ttf")
		if err := os.WriteFile(fontPath, notoSansSCFont, 0o644); err != nil {
			cjkFontErr = err
			return
		}
		if err := api.InstallFonts([]string{fontPath}); err != nil {
			cjkFontErr = err
			return
		}
	})
	return cjkFontErr
}

// DrawingHandler serves the 5 drawing-library routes.
type DrawingHandler struct {
	db    *sql.DB
	dl    *DownloadHandler
	store *tempStore
}

// NewDrawingHandler creates a DrawingHandler.
func NewDrawingHandler(db *sql.DB, dl *DownloadHandler) *DrawingHandler {
	return &DrawingHandler{
		db:    db,
		dl:    dl,
		store: newTempStore(),
	}
}

// Register mounts the drawing-library routes.
func (h *DrawingHandler) Register(r chi.Router) {
	r.Get("/api/drawing/{server_id}/{biz_key}", h.drawingPreview)
	r.Post("/api/pdf-printing/{server_id}/join", h.joinPDF)
	r.Get("/api/pdf-printing/{server_id}/print/{name}", h.printPDF)
	r.Post("/api/pdf-printing/{server_id}/zip", h.zipBizKeys)
	r.Get("/api/pdf-printing/{server_id}/zip-download/{name}", h.zipDownload)
}

// v1Error replicates the old PrintServer.jar error contract: HTTP 200 with
// {"code":"500","msg":…} (code is a string). MRP 分单等老调用方按这个形状写的
// 错误处理，替换期必须保持 —— 这些路由上不要改回真实 HTTP 错误码。
func v1Error(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json;charset=UTF-8")
	_ = json.NewEncoder(w).Encode(map[string]string{"code": "500", "msg": msg})
}

// v1Result replicates the old PrintServer.jar join/zip response:
// {"status":bool,"mergeName":"<无扩展名>","lhs":"料号1,料号2,"}。
// lhs 是"没查到/没取到文件"的料号列表（逗号分隔、结尾带逗号，与 Java
// StringBuilder 输出一致）；MRP 分单前端靠它弹"未查询到XX等打印信息"。
func v1Result(w http.ResponseWriter, status bool, mergeName, lhs string) {
	jsonOK(w, map[string]any{"status": status, "mergeName": mergeName, "lhs": lhs})
}

// errNoPDF signals the catalog has no PDF rows at all for a biz_key.
var errNoPDF = errors.New("no pdf in catalog")

// drawingPDFCandidates is how many newest catalog rows fetchLatestPDF tries
// before giving up. The newest row can be stale (file since moved/deleted on
// AList while the catalog hasn't reconciled yet), so we fall back to older ones.
const drawingPDFCandidates = 5

// fetchLatestPDF downloads the newest available PDF for bizKey to a local temp
// file, skipping stale catalog rows whose file no longer exists on AList.
// Returns errNoPDF when the catalog has no PDF rows, otherwise the last
// download error if every candidate failed.
func (h *DrawingHandler) fetchLatestPDF(ctx context.Context, serverID, bizKey string) (string, func(), error) {
	rows, err := h.db.QueryContext(ctx,
		`SELECT path FROM file_catalog
		 WHERE server_id=? AND biz_key=? AND is_dir=0 AND size>0 AND LOWER(ext)='.pdf'
		 ORDER BY updated_at DESC LIMIT ?`,
		serverID, bizKey, drawingPDFCandidates)
	if err != nil {
		return "", nil, err
	}
	defer rows.Close()
	var paths []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return "", nil, err
		}
		paths = append(paths, p)
	}
	if err := rows.Err(); err != nil {
		return "", nil, err
	}
	if len(paths) == 0 {
		return "", nil, errNoPDF
	}

	var lastErr error
	for _, p := range paths {
		lp, cleanup, err := h.dl.fetchLocal(ctx, serverID, p, "diskmon-draw-*.pdf")
		if err == nil {
			return lp, cleanup, nil
		}
		lastErr = err
	}
	return "", nil, lastErr
}

// drawingPreview streams the latest available PDF for the given biz_key (料号)
// directly as application/pdf, matching the old PrintServer.jar behaviour: the
// browser renders it inline (Content-Disposition inline, no kkFileView redirect).
// GET /api/drawing/{server_id}/{biz_key}
func (h *DrawingHandler) drawingPreview(w http.ResponseWriter, r *http.Request) {
	serverID := chi.URLParam(r, "server_id")
	bizKey := chi.URLParam(r, "biz_key")

	lp, cleanup, err := h.fetchLatestPDF(r.Context(), serverID, bizKey)
	if err != nil {
		// v1 语义：找不到/取不到统一 200 + {"code":"500","msg":"要查找的文件不存在"}
		v1Error(w, "要查找的文件不存在")
		return
	}
	defer cleanup()

	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", `inline; filename="`+bizKey+`.pdf"`)
	http.ServeFile(w, r, lp)
}

// joinItem is one element in the merge-PDF request.
type joinItem struct {
	PDFName    string `json:"pdfName"`    // biz_key (料号) to look up
	PDFExplain string `json:"pdfExplain"` // per-page stamp text (tracking no / PO / qty)
}

// joinPDF fetches the latest PDF for each biz_key, stamps every page with the
// optional pdfExplain text, merges them in order, and returns the v1-shaped
// {status,mergeName,lhs} for a follow-up GET /print/{mergeName}.
// POST /api/pdf-printing/{server_id}/join
func (h *DrawingHandler) joinPDF(w http.ResponseWriter, r *http.Request) {
	serverID := chi.URLParam(r, "server_id")

	var items []joinItem
	if err := json.NewDecoder(r.Body).Decode(&items); err != nil || len(items) == 0 {
		jsonError(w, "请求体须为非空 JSON 数组 [{pdfName,pdfExplain}]", http.StatusBadRequest)
		return
	}

	conf := model.NewDefaultConfiguration()
	conf.ValidationMode = model.ValidationRelaxed

	var tomerge []string
	var cleanups []func()
	var lhs strings.Builder // v1 语义：没查到/没取到文件的料号，"料号1,料号2,"
	defer func() {
		for _, c := range cleanups {
			c()
		}
	}()

	for _, item := range items {
		if item.PDFName == "" {
			continue
		}
		// 无图或文件已不在 AList 的料号记入 lhs 并跳过，保持老系统"尽量合并"的语义。
		lp, cleanup, err := h.fetchLatestPDF(r.Context(), serverID, item.PDFName)
		if err != nil {
			lhs.WriteString(item.PDFName + ",")
			continue
		}
		cleanups = append(cleanups, cleanup)

		if item.PDFExplain != "" {
			annotated, aClean, aErr := annotatePages(lp, item.PDFExplain, conf)
			if aErr == nil {
				cleanups = append(cleanups, aClean)
				lp = annotated
			}
			// annotation failure is non-fatal — use the un-annotated PDF
		}
		tomerge = append(tomerge, lp)
	}

	var valid []string
	for _, lp := range tomerge {
		if api.ValidateFile(lp, conf) == nil {
			valid = append(valid, lp)
		}
	}
	if len(valid) == 0 {
		// v1 全部失败：200 + {"status":false,"mergeName":"","lhs":"…"}
		v1Result(w, false, "", lhs.String())
		return
	}

	tmp, err := os.CreateTemp("", "diskmon-join-*.pdf")
	if err != nil {
		v1Error(w, "create temp file failed")
		return
	}
	tmpName := tmp.Name()
	tmp.Close()

	if err := api.MergeCreateFile(valid, tmpName, false, conf); err != nil {
		os.Remove(tmpName)
		v1Error(w, "merge failed: "+err.Error())
		return
	}

	key := h.store.put(tmpName)
	// v1 的 mergeName 不带扩展名；print/{name} 侧对有无 .pdf 后缀都兼容。
	v1Result(w, true, key, lhs.String())
}

// printPDF serves the merged PDF produced by joinPDF. Repeatable within the
// 30-minute TTL (v1 semantics: MRP opens this URL with window.open and the
// browser PDF viewer issues multiple/range requests; a refresh must also work).
// The file is deleted by the tempStore sweep goroutine when the TTL expires.
// Like v1, no Content-Disposition — the browser renders the PDF inline for printing.
// GET /api/pdf-printing/{server_id}/print/{name}
func (h *DrawingHandler) printPDF(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	key := strings.TrimSuffix(name, ".pdf")
	path, ok := h.store.get(key)
	if !ok {
		v1Error(w, "当前回话已失效,请重新选择图纸") // 文案照抄 v1（含"回话"错别字）
		return
	}
	w.Header().Set("Content-Type", "application/pdf")
	http.ServeFile(w, r, path)
}

// zipBizKeys packs files for the given biz_keys using flat-directory lookup
// (东莞图档库 pattern: find a file with matching biz_key → take its parent
// directory → collect all files in that directory with the same biz_key).
// Returns the v1-shaped {status,mergeName,lhs} for a follow-up
// GET /zip-download/{mergeName}.
// POST /api/pdf-printing/{server_id}/zip
func (h *DrawingHandler) zipBizKeys(w http.ResponseWriter, r *http.Request) {
	serverID := chi.URLParam(r, "server_id")

	// 兼容两种请求体:
	//   ["料号A","料号B"]                         —— Saber3 供应商自助下图前端
	//   {"biz_keys":["料号A"],"ext":["pdf"]}       —— diskmon 原生格式
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		jsonError(w, "读取请求体失败", http.StatusBadRequest)
		return
	}
	var bizKeys, extRaw []string
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		if err := json.Unmarshal(trimmed, &bizKeys); err != nil {
			jsonError(w, "请求体 JSON 解析失败: "+err.Error(), http.StatusBadRequest)
			return
		}
	} else {
		var body struct {
			BizKeys []string `json:"biz_keys"`
			Ext     []string `json:"ext"`
		}
		if err := json.Unmarshal(trimmed, &body); err != nil {
			jsonError(w, "请求体 JSON 解析失败: "+err.Error(), http.StatusBadRequest)
			return
		}
		bizKeys, extRaw = body.BizKeys, body.Ext
	}
	if len(bizKeys) == 0 {
		jsonError(w, "请求体须包含非空料号数组", http.StatusBadRequest)
		return
	}
	exts := normExtSlice(extRaw)

	mounts, err := loadAListMounts(r.Context(), h.db, serverID)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	var allEntries []zipEntry
	var lhs strings.Builder // v1 语义：没查到文件的料号，"料号1,料号2,"
	for _, bk := range bizKeys {
		entries, ferr := flatBizFiles(r.Context(), h.db, serverID, bk, exts, mounts)
		if ferr != nil {
			v1Error(w, "查询失败: "+ferr.Error())
			return
		}
		if len(entries) == 0 {
			lhs.WriteString(bk + ",")
			continue
		}
		allEntries = append(allEntries, entries...)
	}
	if len(allEntries) == 0 {
		// v1 全部失败：200 + {"status":false,"mergeName":"","lhs":"…"}
		v1Result(w, false, "", lhs.String())
		return
	}

	tmp, err := os.CreateTemp("", "diskmon-drawing-zip-*.zip")
	if err != nil {
		v1Error(w, "create temp file failed")
		return
	}
	tmpName := tmp.Name()

	z := newAListZipper(mounts.Base)
	zw := zip.NewWriter(tmp)
	for _, e := range allEntries {
		_ = z.addFile(r.Context(), zw, e.alistPath, e.zipName)
	}
	zw.Close()
	tmp.Close()

	key := h.store.put(tmpName)
	// v1 的 mergeName 不带扩展名；zip-download/{name} 侧对有无 .zip 后缀都兼容。
	v1Result(w, true, key, lhs.String())
}

// zipDownload serves the ZIP archive produced by zipBizKeys. Repeatable within
// the 30-minute TTL (v1 semantics); the sweep goroutine deletes expired files.
// The Content-Disposition filename mimics v1's datetime name (Java
// SimpleDateFormat "yyyy年MM月dd日hh时mm分ss秒" — hh is 12-hour, copied as-is).
// GET /api/pdf-printing/{server_id}/zip-download/{name}
func (h *DrawingHandler) zipDownload(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	key := strings.TrimSuffix(name, ".zip")
	path, ok := h.store.get(key)
	if !ok {
		v1Error(w, "当前会话已失效,请重新选择图纸打印")
		return
	}
	date := time.Now().Format("2006年01月02日03时04分05秒")
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", "attachment;fileName="+date+".zip")
	http.ServeFile(w, r, path)
}

// --- flat-directory helpers (东莞图档库 pattern) ---

// flatBizFiles finds all files for bizKey using the flat-directory pattern used
// by the Donguan drawing library: each large directory contains files from many
// part numbers. Algorithm: find the most-recently-updated file for bizKey → take
// its parent directory → return all files in that exact directory (no deeper)
// that share the same biz_key.
//
// We deliberately do NOT filter on size>0: the client sometimes records a real
// file with NULL/0 size (catalog incomplete), and those files do exist on AList.
// Whether a candidate truly exists is decided at download time — addFile probes
// AList and skips (no empty entry) anything that 404s.
func flatBizFiles(ctx context.Context, db *sql.DB, serverID, bizKey string, exts []string, mounts AListMounts) ([]zipEntry, error) {
	var anyPath string
	err := db.QueryRowContext(ctx,
		`SELECT path FROM file_catalog
		 WHERE server_id=? AND biz_key=? AND is_dir=0
		 ORDER BY updated_at DESC LIMIT 1`,
		serverID, bizKey).Scan(&anyPath)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	parent := winParentDir(anyPath)
	if parent == "" {
		return nil, nil
	}

	// Direct children of parent only: path starts with parent+'\' and has no
	// further '\' after that prefix (LOCATE returns 0 when not found).
	q := `SELECT path FROM file_catalog
	      WHERE server_id=? AND is_dir=0 AND biz_key=?
	        AND LEFT(path, CHAR_LENGTH(?)+1) = CONCAT(?, '\\')
	        AND LOCATE('\\', SUBSTRING(path, CHAR_LENGTH(?)+2)) = 0`
	args := []any{serverID, bizKey, parent, parent, parent}
	if len(exts) > 0 {
		q += " AND LOWER(ext) IN (" + placeholders(len(exts)) + ")"
		for _, e := range exts {
			args = append(args, e)
		}
	}
	q += " ORDER BY path LIMIT 500"

	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []zipEntry
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		alistPath, ok := mounts.resolveURLPath(p, false)
		if !ok {
			continue
		}
		// v1 的 ZIP 按料号分文件夹，且用 Windows 反斜杠作分隔（Java File.separator
		// 直接进了 ZipEntry），照抄以保证解包结构一致。
		entries = append(entries, zipEntry{alistPath: alistPath, zipName: bizKey + `\` + lastSegment(p)})
	}
	return entries, rows.Err()
}

// winParentDir returns the Windows parent directory of path (everything before
// the last backslash). Forward slashes are normalised to backslashes first.
func winParentDir(path string) string {
	path = strings.ReplaceAll(path, "/", `\`)
	i := strings.LastIndex(path, `\`)
	if i < 0 {
		return ""
	}
	return path[:i]
}

// --- per-page PDF annotation ---

// watermarkDesc is the pdfcpu TextWatermark descriptor, verified to reproduce
// the original PrintServer.jar stamp:
//   - native vector text (crisp at any zoom, never rasterised/blurry)
//   - embedded NotoSansSC-Regular for Chinese glyphs
//   - 10pt, red (#FF0000), fully opaque
//   - pos:bl off:60 530 → baseline-left anchored at x=60, y=530 from the
//     bottom-left of every page (robust to non-A4 page sizes since it measures
//     from the bottom).
const drawingWatermarkDesc = "scale:1.0 abs, pos:bl, off:60 530, font:" + cjkFontName +
	", points:10, fillcolor:#FF0000, opacity:1.0, rotation:0"

// annotatePages stamps text onto every page of srcPath using pdfcpu's native
// text watermark, writes the result to a new temp file, and returns its path
// with a cleanup function.
func annotatePages(srcPath, text string, conf *model.Configuration) (string, func(), error) {
	if err := initCJKFont(); err != nil {
		return "", nil, fmt.Errorf("CJK font init: %w", err)
	}

	tmp, err := os.CreateTemp("", "diskmon-ann-*.pdf")
	if err != nil {
		return "", nil, err
	}
	tmpName := tmp.Name()
	tmp.Close()
	cleanup := func() { os.Remove(tmpName) }

	wm, err := api.TextWatermark(text, drawingWatermarkDesc, true, true, types.POINTS)
	if err != nil {
		cleanup()
		return "", nil, fmt.Errorf("TextWatermark: %w", err)
	}
	if err := api.AddWatermarksFile(srcPath, tmpName, nil, wm, conf); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("AddWatermarks: %w", err)
	}
	return tmpName, cleanup, nil
}

// --- tempStore: short-lived result cache for two-step downloads ---

const tempTTL = 30 * time.Minute

type tempEntry struct {
	path    string
	expires time.Time
}

// tempStore maps random hex keys to temporary file paths with a 30-minute TTL.
// Entries stay downloadable until they expire (v1 semantics — the browser PDF
// viewer re-requests and users refresh print tabs). A background goroutine
// purges expired entries and their files every 5 minutes.
type tempStore struct {
	mu      sync.Mutex
	entries map[string]tempEntry
}

func newTempStore() *tempStore {
	s := &tempStore{entries: make(map[string]tempEntry)}
	go s.sweepLoop()
	return s
}

// put stores path and returns its key.
func (s *tempStore) put(path string) string {
	key := randomHex()
	s.mu.Lock()
	s.entries[key] = tempEntry{path: path, expires: time.Now().Add(tempTTL)}
	s.mu.Unlock()
	return key
}

// get returns the path for key without removing it from the store.
func (s *tempStore) get(key string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[key]
	if !ok || time.Now().After(e.expires) {
		return "", false
	}
	return e.path, true
}

func (s *tempStore) sweepLoop() {
	for {
		time.Sleep(5 * time.Minute)
		now := time.Now()
		s.mu.Lock()
		for k, e := range s.entries {
			if now.After(e.expires) {
				os.Remove(e.path)
				delete(s.entries, k)
			}
		}
		s.mu.Unlock()
	}
}

func randomHex() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
