package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"

	"diskmon/internal/alist"
	"diskmon/internal/bizrule"
	"diskmon/internal/model"
)

// ReconcileHandler walks every AList mount point for every configured server
// and upserts all discovered entries into file_catalog. Unlike the full scanner
// it never truncates data, so the catalog stays queryable throughout the run.
type ReconcileHandler struct {
	db      *sql.DB
	jobToken string
	hc      *http.Client
	logger  *slog.Logger
	running atomic.Bool // guards against concurrent HTTP-triggered runs
}

// NewReconcileHandler creates a ReconcileHandler. jobToken guards the HTTP trigger.
func NewReconcileHandler(db *sql.DB, jobToken string) *ReconcileHandler {
	return &ReconcileHandler{
		db:       db,
		jobToken: jobToken,
		hc:       &http.Client{Timeout: 30 * time.Second},
		logger:   slog.Default(),
	}
}

// Register mounts the manual-trigger endpoint (caller applies auth middleware).
func (h *ReconcileHandler) Register(r chi.Router) {
	r.Post("/api/jobs/reconcile-catalog", h.httpRun)
}

// ReconcileSummary reports what one run did.
type ReconcileSummary struct {
	Servers  int `json:"servers"`
	Dirs     int `json:"dirs"`
	Files    int `json:"files"`
	Upserted int `json:"upserted"`
	NewRows  int `json:"new_rows"`  // rows genuinely inserted (not existing-row updates)
	Errors   int `json:"errors"`
}

func (s ReconcileSummary) String() string {
	return fmt.Sprintf("servers=%d dirs=%d files=%d upserted=%d new_rows=%d errors=%d",
		s.Servers, s.Dirs, s.Files, s.Upserted, s.NewRows, s.Errors)
}

func (h *ReconcileHandler) httpRun(w http.ResponseWriter, r *http.Request) {
	if h.jobToken == "" || r.Header.Get("X-Job-Token") != h.jobToken {
		jsonError(w, "forbidden", http.StatusForbidden)
		return
	}
	if !h.running.CompareAndSwap(false, true) {
		jsonError(w, "reconcile already running", http.StatusConflict)
		return
	}
	serverID := r.URL.Query().Get("server_id")
	go func() {
		defer h.running.Store(false)
		sum, err := h.Run(context.Background(), serverID)
		if err != nil {
			h.logger.Error("reconcile async failed", "err", err)
			return
		}
		h.logger.Info("reconcile async done", "summary", sum.String())
	}()
	w.WriteHeader(http.StatusAccepted)
	jsonOK(w, map[string]string{"status": "started", "server_id": serverID})
}

// Run walks all servers (or just serverID when non-empty). Called by XXL-JOB.
func (h *ReconcileHandler) Run(ctx context.Context, serverID string) (ReconcileSummary, error) {
	srvs, err := h.listServers(ctx, serverID)
	if err != nil {
		return ReconcileSummary{}, err
	}

	var total ReconcileSummary
	for _, srv := range srvs {
		mounts, err := loadAListMounts(ctx, h.db, srv.id)
		if err != nil {
			h.logger.Warn("reconcile: skip server (no AList mounts)", "server", srv.id, "err", err)
			total.Errors++
			continue
		}
		total.Servers++
		s := h.walkServer(ctx, srv, mounts)
		total.Dirs += s.Dirs
		total.Files += s.Files
		total.Upserted += s.Upserted
		total.NewRows += s.NewRows
		total.Errors += s.Errors
		h.logger.Info("reconcile: server done",
			"server", srv.id,
			"dirs", s.Dirs,
			"files", s.Files,
			"upserted", s.Upserted,
			"new_rows", s.NewRows,
			"errors", s.Errors,
		)
	}
	return total, nil
}

// reconcileSrv holds the per-server data needed during a walk.
type reconcileSrv struct {
	id       string
	apiAddr  string
	apiToken string
	volumes  []struct {
		Name    string `json:"name"`
		BizRule string `json:"biz_rule"`
	}
}

func (h *ReconcileHandler) listServers(ctx context.Context, serverID string) ([]reconcileSrv, error) {
	q := `SELECT server_id, COALESCE(api_addr,''), COALESCE(api_token,''), COALESCE(volumes,'[]')
	      FROM servers`
	args := []any{}
	if serverID != "" {
		q += " WHERE server_id=?"
		args = append(args, serverID)
	}
	rows, err := h.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list servers: %w", err)
	}
	defer rows.Close()

	var srvs []reconcileSrv
	for rows.Next() {
		var s reconcileSrv
		var volJSON []byte
		if err := rows.Scan(&s.id, &s.apiAddr, &s.apiToken, &volJSON); err != nil {
			return nil, fmt.Errorf("scan server: %w", err)
		}
		_ = json.Unmarshal(volJSON, &s.volumes)
		srvs = append(srvs, s)
	}
	return srvs, rows.Err()
}

// walkItem is one directory queued for BFS listing.
type walkItem struct {
	alistPath string // e.g. /新系统成品代码专案/262405
	winPath   string // e.g. E:\...\新系统成品代码专案\262405
}

func (h *ReconcileHandler) walkServer(ctx context.Context, srv reconcileSrv, mounts AListMounts) ReconcileSummary {
	var sum ReconcileSummary

	h.logger.Info("reconcile: server start", "server", srv.id, "mounts", len(mounts.Mounts))

	// Build per-volume rules, cached.
	ruleCache := map[string]bizrule.VolumeRule{}
	rule := func(volume string) bizrule.VolumeRule {
		if r, ok := ruleCache[volume]; ok {
			return r
		}
		r := h.buildRule(ctx, srv, volume)
		ruleCache[volume] = r
		return r
	}

	// Seed BFS queue with each mount root.
	var queue []walkItem
	for _, mt := range mounts.Mounts {
		queue = append(queue, walkItem{alistPath: mt.Mount, winPath: mt.Prefix})
	}

	const (
		batchSize    = 500
		progressEvery = 500 // log progress every N dirs
	)
	var pending []model.FileEntry

	flush := func() {
		if len(pending) == 0 {
			return
		}
		n := len(pending)
		inserted, err := upsertCatalog(ctx, h.db, pending)
		if err != nil {
			h.logger.Error("reconcile: upsert failed", "server", srv.id, "err", err)
			sum.Errors++
		} else {
			sum.Upserted += n
			sum.NewRows += int(inserted)
			if inserted > 0 {
				h.logger.Info("reconcile: new rows found",
					"server", srv.id,
					"batch_size", n,
					"new_rows", inserted,
					"total_new_so_far", sum.NewRows,
				)
			}
		}
		pending = pending[:0]
	}

	for len(queue) > 0 {
		select {
		case <-ctx.Done():
			flush()
			return sum
		default:
		}

		item := queue[0]
		queue = queue[1:]

		vol := winVolumeOf(item.winPath)
		r := rule(vol)
		now := time.Now().UTC()

		// Upsert the directory itself.
		pending = append(pending, model.FileEntry{
			ServerID:  srv.id,
			Volume:    vol,
			Path:      item.winPath,
			IsDir:     true,
			BizKey:    r.ExtractBizKey(item.winPath),
			UpdatedAt: now,
		})
		sum.Dirs++

		if sum.Dirs%progressEvery == 0 {
			h.logger.Info("reconcile: progress",
				"server", srv.id,
				"dirs_done", sum.Dirs,
				"files_found", sum.Files,
				"new_rows_so_far", sum.NewRows,
				"queue_remaining", len(queue),
			)
		}

		// List immediate children via AList (paginated).
		children, err := alist.List(ctx, h.hc, mounts.Base, item.alistPath)
		if err != nil {
			h.logger.Warn("reconcile: list failed", "path", item.alistPath, "err", err)
			sum.Errors++
			if len(pending) >= batchSize {
				flush()
			}
			continue
		}

		for _, c := range children {
			childAList := strings.TrimRight(item.alistPath, "/") + "/" + c.Name
			childWin, ok := mounts.alistPathToWin(childAList)
			if !ok {
				sum.Errors++
				continue
			}
			childVol := winVolumeOf(childWin)

			var size *int64
			if !c.IsDir && c.Size > 0 {
				s := c.Size
				size = &s
			}
			ext := ""
			if !c.IsDir {
				if e := strings.ToLower(filepath.Ext(c.Name)); len(e) <= 20 {
					ext = e
				}
			}

			pending = append(pending, model.FileEntry{
				ServerID:  srv.id,
				Volume:    childVol,
				Path:      childWin,
				IsDir:     c.IsDir,
				Size:      size,
				Ext:       ext,
				BizKey:    r.ExtractBizKey(childWin),
				UpdatedAt: now,
			})
			if c.IsDir {
				sum.Dirs++
				queue = append(queue, walkItem{alistPath: childAList, winPath: childWin})
			} else {
				sum.Files++
			}
		}

		if len(pending) >= batchSize {
			flush()
		}
	}

	flush()
	return sum
}

// buildRule returns the biz_key rule for the given volume on this server.
func (h *ReconcileHandler) buildRule(ctx context.Context, srv reconcileSrv, volume string) bizrule.VolumeRule {
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
		if root := h.fetchDrawingsRoot(ctx, srv); root != "" {
			return &bizrule.DrawingsRule{Root: root}
		}
	}
	return bizrule.NoopRule{}
}

func (h *ReconcileHandler) fetchDrawingsRoot(ctx context.Context, srv reconcileSrv) string {
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
	if len(cfg.Filters.IncludeDirs) > 0 {
		return cfg.Filters.IncludeDirs[0]
	}
	return ""
}
