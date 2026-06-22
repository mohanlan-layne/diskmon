package handler

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"diskmon/internal/alist"
)

// backfillBatch caps how many NULL-size rows one run processes, so a single pass
// never floods AList nor holds the DB for too long. Runs every few minutes, so a
// large backlog drains over several passes.
const backfillBatch = 1000

// backfillMinAgeMinutes is the grace period before a NULL-size row is eligible.
// A freshly written file may still be uploading (temp not yet finalized); we only
// touch rows whose updated_at is older than this to avoid racing with uploads.
const backfillMinAgeMinutes = 10

// BackfillHandler repairs file_catalog rows that the USN watcher recorded with a
// NULL size (and possibly a wrong is_dir): it probes the real file via AList and
// either backfills size/is_dir or deletes the row when the file is gone/empty.
//
// It is driven by XXL-JOB (the registered "backfillCatalog" task calls Run), and
// also exposes a token-guarded HTTP endpoint for manual triggering and testing.
type BackfillHandler struct {
	db       *sql.DB
	jobToken string // X-Job-Token required to call the HTTP endpoint; "" disables it
	hc       *http.Client
}

// NewBackfillHandler creates a BackfillHandler. jobToken guards the HTTP trigger.
func NewBackfillHandler(db *sql.DB, jobToken string) *BackfillHandler {
	return &BackfillHandler{
		db:       db,
		jobToken: jobToken,
		hc:       &http.Client{Timeout: 20 * time.Second},
	}
}

// Register mounts the manual-trigger endpoint.
func (h *BackfillHandler) Register(r chi.Router) {
	r.Post("/api/jobs/backfill-catalog", h.httpRun)
}

// BackfillSummary reports what one run did. Returned as JSON (HTTP) and rendered
// into the XXL-JOB execution log.
type BackfillSummary struct {
	Scanned    int `json:"scanned"`    // rows examined
	FixedFile  int `json:"fixed_file"` // files whose size was backfilled
	FixedDir   int `json:"fixed_dir"`  // rows corrected to is_dir=1
	Deleted    int `json:"deleted"`    // rows removed (gone or 0-byte file)
	Unresolved int `json:"unresolved"` // path not under any AList mount → skipped, kept
	Errors     int `json:"errors"`     // AList/DB failures → skipped, kept, retried next run
}

func (s BackfillSummary) String() string {
	return fmt.Sprintf("scanned=%d fixed_file=%d fixed_dir=%d deleted=%d unresolved=%d errors=%d",
		s.Scanned, s.FixedFile, s.FixedDir, s.Deleted, s.Unresolved, s.Errors)
}

// httpRun is the token-guarded manual trigger. Optional query params scope the
// run for safe canaries / per-server draining: ?server_id=X&limit=N (limit is
// capped at backfillBatch). With no params it behaves exactly like the scheduled
// job (all servers, full batch).
func (h *BackfillHandler) httpRun(w http.ResponseWriter, r *http.Request) {
	if h.jobToken == "" || r.Header.Get("X-Job-Token") != h.jobToken {
		jsonError(w, "forbidden", http.StatusForbidden)
		return
	}
	serverID := r.URL.Query().Get("server_id")
	limit := backfillBatch
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n < backfillBatch {
			limit = n
		}
	}
	sum, err := h.run(r.Context(), serverID, limit)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonOK(w, sum)
}

// pendingRow is a NULL-size catalog row awaiting repair.
type pendingRow struct {
	id       int64
	serverID string
	path     string
}

// Run performs one full backfill pass (all servers, default batch). This is what
// the scheduled XXL-JOB task calls.
func (h *BackfillHandler) Run(ctx context.Context) (BackfillSummary, error) {
	return h.run(ctx, "", backfillBatch)
}

// run performs one backfill pass, optionally scoped to a single server and a
// custom row limit. It is safe to call concurrently with serving traffic; each
// row is handled independently so one failure never aborts the batch.
func (h *BackfillHandler) run(ctx context.Context, serverID string, limit int) (BackfillSummary, error) {
	q := `SELECT id, server_id, path FROM file_catalog
		WHERE size IS NULL AND is_dir=0 AND updated_at < (NOW() - INTERVAL ? MINUTE)`
	args := []any{backfillMinAgeMinutes}
	if serverID != "" {
		q += " AND server_id=?"
		args = append(args, serverID)
	}
	q += " ORDER BY server_id LIMIT ?"
	args = append(args, limit)

	rows, err := h.db.QueryContext(ctx, q, args...)
	if err != nil {
		return BackfillSummary{}, fmt.Errorf("query pending rows: %w", err)
	}
	var pending []pendingRow
	for rows.Next() {
		var p pendingRow
		if err := rows.Scan(&p.id, &p.serverID, &p.path); err != nil {
			rows.Close()
			return BackfillSummary{}, fmt.Errorf("scan pending row: %w", err)
		}
		pending = append(pending, p)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return BackfillSummary{}, err
	}
	rows.Close() // release the connection before slow AList calls / writes

	// Per-server AList mounts, loaded lazily and cached. A nil entry with a cached
	// error means the whole server is skipped for this run.
	type mountCache struct {
		mounts AListMounts
		err    error
	}
	cache := map[string]mountCache{}
	loadMounts := func(serverID string) (AListMounts, error) {
		if c, ok := cache[serverID]; ok {
			return c.mounts, c.err
		}
		m, err := loadAListMounts(ctx, h.db, serverID)
		cache[serverID] = mountCache{m, err}
		return m, err
	}

	var sum BackfillSummary
	for _, p := range pending {
		sum.Scanned++

		mounts, err := loadMounts(p.serverID)
		if err != nil {
			sum.Errors++
			continue
		}
		alistPath, ok := mounts.resolveURLPath(p.path, false)
		if !ok {
			// Path is under no configured mount: cannot verify or serve it. Keep the
			// row (a mount misconfig must not trigger mass deletion) and surface it.
			sum.Unresolved++
			continue
		}

		fi, err := alist.Stat(ctx, h.hc, mounts.Base, alistPath)
		if err != nil {
			sum.Errors++ // transient (storage offline/network): keep, retry next run
			continue
		}

		var ok2 bool
		switch decideBackfill(fi) {
		case actSetSize:
			if ok2 = h.setSize(ctx, p.id, fi.Size); ok2 {
				sum.FixedFile++
			}
		case actSetDir:
			if ok2 = h.setDir(ctx, p.id); ok2 {
				sum.FixedDir++
			}
		case actDelete:
			if ok2 = h.deleteRow(ctx, p.id); ok2 {
				sum.Deleted++
			}
		}
		if !ok2 {
			sum.Errors++ // the UPDATE/DELETE itself failed
		}
	}
	return sum, nil
}

// backfillAction is the repair decided for one row from its real AList state.
type backfillAction int

const (
	actDelete  backfillAction = iota // file gone or genuinely 0 bytes → remove row
	actSetSize                       // real file with size>0 → backfill size, is_dir=0
	actSetDir                        // real directory → correct is_dir=1, keep row
)

// decideBackfill maps a probed file state to the repair action. Directories are
// never deleted (only their is_dir is corrected); only a confirmed-missing or
// truly-empty file is removed.
func decideBackfill(fi alist.FileInfo) backfillAction {
	switch {
	case !fi.Exists:
		return actDelete
	case fi.IsDir:
		return actSetDir
	case fi.Size > 0:
		return actSetSize
	default:
		return actDelete
	}
}

func (h *BackfillHandler) setSize(ctx context.Context, id, size int64) bool {
	_, err := h.db.ExecContext(ctx,
		"UPDATE file_catalog SET size=?, is_dir=0 WHERE id=?", size, id)
	return err == nil
}

func (h *BackfillHandler) setDir(ctx context.Context, id int64) bool {
	_, err := h.db.ExecContext(ctx,
		"UPDATE file_catalog SET is_dir=1 WHERE id=?", id)
	return err == nil
}

func (h *BackfillHandler) deleteRow(ctx context.Context, id int64) bool {
	_, err := h.db.ExecContext(ctx,
		"DELETE FROM file_catalog WHERE id=?", id)
	return err == nil
}
