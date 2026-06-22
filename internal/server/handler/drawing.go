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
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
	kkURL string
	store *tempStore
}

// NewDrawingHandler creates a DrawingHandler.
func NewDrawingHandler(db *sql.DB, dl *DownloadHandler, kkURL string) *DrawingHandler {
	return &DrawingHandler{
		db:    db,
		dl:    dl,
		kkURL: strings.TrimRight(kkURL, "/"),
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

// drawingPreview redirects to the kkFileView preview of the latest PDF (size>0)
// for the given biz_key (料号).
// GET /api/drawing/{server_id}/{biz_key}
func (h *DrawingHandler) drawingPreview(w http.ResponseWriter, r *http.Request) {
	serverID := chi.URLParam(r, "server_id")
	bizKey := chi.URLParam(r, "biz_key")

	var path string
	err := h.db.QueryRowContext(r.Context(),
		`SELECT path FROM file_catalog
		 WHERE server_id=? AND biz_key=? AND is_dir=0 AND size>0 AND LOWER(ext)='.pdf'
		 ORDER BY updated_at DESC LIMIT 1`,
		serverID, bizKey).Scan(&path)
	if err == sql.ErrNoRows {
		jsonError(w, "该料号没有 PDF 图纸", http.StatusNotFound)
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
	fileURL, ok := mounts.previewFileURL(path)
	if !ok {
		jsonError(w, "该文件不在任何 AList 挂载目录下", http.StatusBadRequest)
		return
	}
	if h.kkURL == "" {
		jsonOK(w, map[string]string{"url": fileURL})
		return
	}
	encoded := base64.StdEncoding.EncodeToString([]byte(fileURL))
	http.Redirect(w, r, h.kkURL+"/onlinePreview?url="+url.QueryEscape(encoded), http.StatusFound)
}

// joinItem is one element in the merge-PDF request.
type joinItem struct {
	PDFName    string `json:"pdfName"`    // biz_key (料号) to look up
	PDFExplain string `json:"pdfExplain"` // per-page stamp text (tracking no / PO / qty)
}

// joinPDF fetches the latest PDF for each biz_key, stamps every page with the
// optional pdfExplain text, merges them in order, and returns {mergeName} for a
// follow-up GET /print/{mergeName}.
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
	defer func() {
		for _, c := range cleanups {
			c()
		}
	}()

	for _, item := range items {
		if item.PDFName == "" {
			continue
		}
		var pdfPath string
		err := h.db.QueryRowContext(r.Context(),
			`SELECT path FROM file_catalog
			 WHERE server_id=? AND biz_key=? AND is_dir=0 AND size>0 AND LOWER(ext)='.pdf'
			 ORDER BY updated_at DESC LIMIT 1`,
			serverID, item.PDFName).Scan(&pdfPath)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			jsonError(w, "query failed: "+err.Error(), http.StatusInternalServerError)
			return
		}

		lp, cleanup, err := h.dl.fetchLocal(r.Context(), serverID, pdfPath, "diskmon-draw-*.pdf")
		if err != nil {
			jsonError(w, "下载 PDF 失败: "+err.Error(), http.StatusBadGateway)
			return
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

	if len(tomerge) == 0 {
		jsonError(w, "这些料号下没有可用的 PDF 文件", http.StatusNotFound)
		return
	}

	var valid []string
	for _, lp := range tomerge {
		if api.ValidateFile(lp, conf) == nil {
			valid = append(valid, lp)
		}
	}
	if len(valid) == 0 {
		jsonError(w, "所有 PDF 均无法解析（可能损坏）", http.StatusUnprocessableEntity)
		return
	}

	tmp, err := os.CreateTemp("", "diskmon-join-*.pdf")
	if err != nil {
		jsonError(w, "create temp file failed", http.StatusInternalServerError)
		return
	}
	tmpName := tmp.Name()
	tmp.Close()

	if err := api.MergeCreateFile(valid, tmpName, false, conf); err != nil {
		os.Remove(tmpName)
		jsonError(w, "merge failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	key := h.store.put(tmpName)
	jsonOK(w, map[string]string{"mergeName": key + ".pdf"})
}

// printPDF serves the merged PDF produced by joinPDF (one-time download).
// GET /api/pdf-printing/{server_id}/print/{name}
func (h *DrawingHandler) printPDF(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	key := strings.TrimSuffix(name, ".pdf")
	path, ok := h.store.pop(key)
	if !ok {
		jsonError(w, "文件不存在或已过期（30分钟有效）", http.StatusNotFound)
		return
	}
	defer os.Remove(path)
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	http.ServeFile(w, r, path)
}

// zipBizKeys packs files for the given biz_keys using flat-directory lookup
// (东莞图档库 pattern: find a file with matching biz_key → take its parent
// directory → collect all files in that directory with the same biz_key).
// Returns {mergeName} for a follow-up GET /zip-download/{mergeName}.
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
	for _, bk := range bizKeys {
		entries, ferr := flatBizFiles(r.Context(), h.db, serverID, bk, exts, mounts)
		if ferr != nil {
			jsonError(w, "查询失败: "+ferr.Error(), http.StatusInternalServerError)
			return
		}
		allEntries = append(allEntries, entries...)
	}
	if len(allEntries) == 0 {
		jsonError(w, "没有可打包的文件", http.StatusNotFound)
		return
	}

	tmp, err := os.CreateTemp("", "diskmon-drawing-zip-*.zip")
	if err != nil {
		jsonError(w, "create temp file failed", http.StatusInternalServerError)
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
	jsonOK(w, map[string]string{"mergeName": key + ".zip"})
}

// zipDownload serves the ZIP archive produced by zipBizKeys (one-time download).
// GET /api/pdf-printing/{server_id}/zip-download/{name}
func (h *DrawingHandler) zipDownload(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	key := strings.TrimSuffix(name, ".zip")
	path, ok := h.store.pop(key)
	if !ok {
		jsonError(w, "文件不存在或已过期（30分钟有效）", http.StatusNotFound)
		return
	}
	defer os.Remove(path)
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
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
		entries = append(entries, zipEntry{alistPath: alistPath, zipName: lastSegment(p)})
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
// Keys are consumed on first pop (one-time download). A background goroutine
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

// pop returns the path for key and removes it from the store.
func (s *tempStore) pop(key string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[key]
	if !ok || time.Now().After(e.expires) {
		delete(s.entries, key)
		return "", false
	}
	delete(s.entries, key)
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
