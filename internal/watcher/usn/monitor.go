//go:build windows

package usn

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows"

	"diskmon/internal/bizrule"
	"diskmon/internal/catalog"
	"diskmon/internal/checkpoint"
	"diskmon/internal/config"
	"diskmon/internal/filter"
	"diskmon/internal/model"
	"diskmon/internal/pathresolver"
)

// pendingRename holds one side of a rename pair waiting to be matched.
type pendingRename struct {
	path      string
	timestamp time.Time
	isDir     bool
}

// Monitor watches a single NTFS volume via USN Journal and writes changes to the catalog.
type Monitor struct {
	serverID   string
	volume     config.VolumeConfig
	filter     *filter.Filter
	rule       bizrule.VolumeRule
	catalog    *catalog.Catalog
	checkDir   string
	pollDelay  time.Duration
	renameWin  time.Duration
	cacheSize  int
	dryRun     bool
	logger     *slog.Logger
}

// MonitorConfig groups the parameters for NewMonitor.
type MonitorConfig struct {
	ServerID      string
	Volume        config.VolumeConfig
	Filter        *filter.Filter
	Rule          bizrule.VolumeRule
	Catalog       *catalog.Catalog
	CheckpointDir string
	PollIntervalMs int
	RenameWindowMs int
	CacheSize      int
	DryRun         bool
	Logger         *slog.Logger
}

// NewMonitor creates a Monitor for a single volume.
func NewMonitor(cfg MonitorConfig) *Monitor {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Monitor{
		serverID:  cfg.ServerID,
		volume:    cfg.Volume,
		filter:    cfg.Filter,
		rule:      cfg.Rule,
		catalog:   cfg.Catalog,
		checkDir:  cfg.CheckpointDir,
		pollDelay: time.Duration(cfg.PollIntervalMs) * time.Millisecond,
		renameWin: time.Duration(cfg.RenameWindowMs) * time.Millisecond,
		cacheSize: cfg.CacheSize,
		dryRun:    cfg.DryRun,
		logger:    logger.With("volume", cfg.Volume.Name),
	}
}

// Run starts the monitoring loop. Blocks until ctx is cancelled.
func (m *Monitor) Run(ctx context.Context) error {
	volHandle, err := OpenVolume(m.volume.Name)
	if err != nil {
		return fmt.Errorf("monitor %s: %w", m.volume.Name, err)
	}
	defer windows.CloseHandle(volHandle)

	if err := EnsureJournal(volHandle, m.volume.JournalSize); err != nil {
		return fmt.Errorf("ensure journal %s: %w", m.volume.Name, err)
	}

	resolver, err := pathresolver.New(m.volume.Name, m.cacheSize)
	if err != nil {
		return fmt.Errorf("path resolver %s: %w", m.volume.Name, err)
	}
	defer resolver.Close()

	cp, err := checkpoint.Load(m.checkDir, m.volume.Name)
	if err != nil {
		m.logger.Warn("could not load checkpoint, starting from beginning", "err", err)
	}

	pendingOld := make(map[uint64]pendingRename)
	pendingNew := make(map[uint64]pendingRename)

	m.logger.Info("monitor started", "nextUSN", cp.NextUSN, "journalID", cp.JournalID,
		"include_dirs", m.filter.IncludeDirs, "extensions", m.filter.Extensions)

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		journal, err := QueryJournal(volHandle)
		if err != nil {
			m.logger.Error("query journal failed", "err", err)
			m.sleep(ctx)
			continue
		}

		// Detect journal reset (journal ID changed).
		if cp.JournalID != 0 && cp.JournalID != journal.JournalID {
			m.logger.Warn("USN journal ID changed — journal was reset, restarting from beginning",
				"old", cp.JournalID, "new", journal.JournalID)
			cp = checkpoint.Data{}
		}
		if cp.JournalID == 0 {
			cp.JournalID = journal.JournalID
		}
		if cp.NextUSN == 0 {
			// Fresh start: begin at the journal's current end so we only watch
			// changes from now on. Existing files are captured by the full scan.
			// Starting from FirstUSN would replay the entire historical journal.
			cp.NextUSN = journal.NextUSN
			m.logger.Info("no checkpoint, starting from current journal position", "nextUSN", cp.NextUSN)
		}

		// Read up to 6 consecutive batches per poll cycle (~250 ms of records).
		events, nextUSN, err := m.readCycle(ctx, volHandle, resolver, journal, cp.NextUSN,
			pendingOld, pendingNew)
		if err != nil {
			if IsJournalEntryDeleted(err) {
				// Journal wrapped; the records since our checkpoint are gone.
				// Skip the gap and stay live rather than replaying all history.
				// Changes in the lost window are missed — a full rescan is needed
				// to reconcile if this happens.
				m.logger.Warn("journal entries deleted (wrapped), skipping to current position — run a full rescan to reconcile",
					"lost_from", cp.NextUSN, "resume_at", journal.NextUSN)
				cp.NextUSN = journal.NextUSN
				m.sleep(ctx)
				continue
			}
			m.logger.Error("read cycle failed", "err", err)
			m.sleep(ctx)
			continue
		}

		// Flush expired rename pairs before writing.
		events = m.flushExpiredRenames(pendingOld, pendingNew, events)

		// Collapse the burst of records a single file operation produces.
		events = dedupEvents(events)

		if len(events) > 0 {
			if err := m.applyEvents(ctx, events); err != nil {
				m.logger.Error("apply events failed", "err", err)
				// Do not advance checkpoint so events are retried next cycle.
				m.sleep(ctx)
				continue
			}
		}

		cp.NextUSN = nextUSN
		if saveErr := checkpoint.Save(m.checkDir, m.volume.Name, cp); saveErr != nil {
			m.logger.Warn("checkpoint save failed", "err", saveErr)
		}

		m.sleep(ctx)
	}
}

// readCycle reads up to 6 consecutive batches from the journal.
func (m *Monitor) readCycle(
	ctx context.Context,
	handle windows.Handle,
	resolver *pathresolver.Resolver,
	journal JournalInfo,
	startUSN int64,
	pendingOld, pendingNew map[uint64]pendingRename,
) ([]model.ChangeEvent, int64, error) {
	var events []model.ChangeEvent
	currentUSN := startUSN
	deadline := time.Now().Add(250 * time.Millisecond)

	for i := 0; i < 6 && time.Now().Before(deadline); i++ {
		select {
		case <-ctx.Done():
			return events, currentUSN, nil
		default:
		}

		records, nextUSN, err := ReadBatch(handle, currentUSN, journal.JournalID)
		if err != nil {
			return events, currentUSN, err
		}
		if nextUSN == currentUSN {
			break // no new records
		}

		for _, rec := range records {
			evts := m.processRecord(rec, resolver, pendingOld, pendingNew)
			events = append(events, evts...)
		}
		currentUSN = nextUSN
	}
	return events, currentUSN, nil
}

// processRecord resolves the path and converts a raw record to ChangeEvent(s).
// Rename events may yield 0 events (pending) or 1 complete rename event.
func (m *Monitor) processRecord(
	rec RawRecord,
	resolver *pathresolver.Resolver,
	pendingOld, pendingNew map[uint64]pendingRename,
) []model.ChangeEvent {
	isDir := IsDirectory(rec.FileAttrs)
	occuredAt := WindowsFileTimeToTime(rec.Timestamp)

	// Resolve path: try parent FRN + filename first, fall back to direct FRN.
	path := m.resolvePath(rec, resolver)

	if IsRenameOld(rec.Reason) {
		if path != "" {
			pendingOld[rec.FRN] = pendingRename{path: path, timestamp: occuredAt, isDir: isDir}
		}
		return nil
	}

	if IsRenameNew(rec.Reason) {
		if path == "" {
			if old, ok := pendingOld[rec.FRN]; ok {
				delete(pendingOld, rec.FRN)
				return []model.ChangeEvent{{
					EventType:  "remove",
					Path:       old.path,
					IsDir:      isDir,
					Volume:     m.volume.Name,
					FRN:        rec.FRN,
					USN:        rec.USN,
					OccurredAt: occuredAt,
				}}
			}
			return nil
		}
		pendingNew[rec.FRN] = pendingRename{path: path, timestamp: occuredAt, isDir: isDir}
		if old, ok := pendingOld[rec.FRN]; ok {
			delete(pendingOld, rec.FRN)
			delete(pendingNew, rec.FRN)
			return m.buildRenameEvents(rec, old.path, path, isDir, occuredAt)
		}
		return nil
	}

	eventType := MapEventType(rec.Reason)
	if eventType == "" {
		return nil
	}
	if path == "" {
		m.logger.Info("path resolution failed, event dropped", "file", rec.FileName, "parentFRN", rec.ParentFRN, "frn", rec.FRN)
		return nil
	}

	evt := model.ChangeEvent{
		EventType:  eventType,
		Path:       path,
		IsDir:      isDir,
		Volume:     m.volume.Name,
		FRN:        rec.FRN,
		ParentFRN:  rec.ParentFRN,
		USN:        rec.USN,
		OccurredAt: occuredAt,
	}
	if !isDir {
		evt.Ext = strings.ToLower(filepath.Ext(path))
	}
	enrichSize(&evt)

	if !m.filter.Match(evt) {
		// 只在路径属于监听目录时记录被过滤的原因（如后缀不匹配）
		if m.filter.IsUnderIncludeDirs(path) {
			m.logger.Info("file detected but filtered", "type", eventType, "path", path, "ext", evt.Ext)
		}
		return nil
	}
	return []model.ChangeEvent{evt}
}

func (m *Monitor) buildRenameEvents(rec RawRecord, oldPath, newPath string, isDir bool, at time.Time) []model.ChangeEvent {
	lower := strings.ToLower(newPath)
	// New path into recycle bin → treat as remove with old path.
	if strings.Contains(lower, "$recycle.bin") {
		return []model.ChangeEvent{{
			EventType:  "remove",
			Path:       oldPath,
			IsDir:      isDir,
			Volume:     m.volume.Name,
			FRN:        rec.FRN,
			USN:        rec.USN,
			OccurredAt: at,
		}}
	}

	evt := model.ChangeEvent{
		EventType:  "rename",
		Path:       newPath,
		OldPath:    oldPath,
		IsDir:      isDir,
		Volume:     m.volume.Name,
		FRN:        rec.FRN,
		USN:        rec.USN,
		OccurredAt: at,
	}
	if !isDir {
		evt.Ext = strings.ToLower(filepath.Ext(newPath))
	}
	enrichSize(&evt)

	if !m.filter.Match(evt) {
		return nil
	}
	return []model.ChangeEvent{evt}
}

// flushExpiredRenames converts unpaired rename pairs that exceeded the rename window
// into create/remove events and returns the extended events slice.
func (m *Monitor) flushExpiredRenames(
	pendingOld, pendingNew map[uint64]pendingRename,
	events []model.ChangeEvent,
) []model.ChangeEvent {
	now := time.Now()
	for frn, r := range pendingOld {
		if now.Sub(r.timestamp) > m.renameWin {
			delete(pendingOld, frn)
			evt := model.ChangeEvent{
				EventType:  "remove",
				Path:       r.path,
				IsDir:      r.isDir,
				Volume:     m.volume.Name,
				OccurredAt: r.timestamp,
			}
			if m.filter.Match(evt) {
				events = append(events, evt)
			}
		}
	}
	for frn, r := range pendingNew {
		if now.Sub(r.timestamp) > m.renameWin {
			delete(pendingNew, frn)
			evt := model.ChangeEvent{
				EventType:  "create",
				Path:       r.path,
				IsDir:      r.isDir,
				Volume:     m.volume.Name,
				OccurredAt: r.timestamp,
			}
			if !r.isDir {
				evt.Ext = strings.ToLower(filepath.Ext(r.path))
			}
			enrichSize(&evt)
			if m.filter.Match(evt) {
				events = append(events, evt)
			}
		}
	}
	return events
}

// dedupEvents collapses the burst of records a single file operation produces
// within one read cycle. Creating or saving a file emits several USN records
// (FileCreate, DataExtend, DataOverwrite, DataTruncation, BasicInfoChange,
// Close…), several of which carry the same reason bit and would otherwise
// become identical upserts and log lines. Rules:
//   - at most one event per (type, path) is kept, in first-seen order;
//   - a write to a path that was also created in this cycle is dropped (the
//     create already upserts the row).
//
// Order is preserved so a later remove/rename still applies after the
// create/write that precedes it (e.g. a temp file created and deleted in the
// same cycle yields create then remove).
func dedupEvents(events []model.ChangeEvent) []model.ChangeEvent {
	if len(events) <= 1 {
		return events
	}
	hasCreate := make(map[string]bool)
	for _, e := range events {
		if e.EventType == "create" {
			hasCreate[e.Path] = true
		}
	}
	seen := make(map[string]bool)
	out := make([]model.ChangeEvent, 0, len(events))
	for _, e := range events {
		if e.EventType == "write" && hasCreate[e.Path] {
			continue // create already covers this path
		}
		key := e.EventType + "\x00" + e.OldPath + "\x00" + e.Path
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, e)
	}
	return out
}

// applyEvents writes a batch of events to the catalog.
func (m *Monitor) applyEvents(ctx context.Context, events []model.ChangeEvent) error {
	for _, evt := range events {
		entry := model.FileEntry{
			ServerID:  m.serverID,
			Volume:    evt.Volume,
			Path:      evt.Path,
			IsDir:     evt.IsDir,
			Size:      evt.Size,
			Ext:       evt.Ext,
			BizKey:    m.rule.ExtractBizKey(evt.Path),
			UpdatedAt: evt.OccurredAt,
		}

		switch evt.EventType {
		case "rename":
			m.logger.Info("file event", "type", "rename", "old", evt.OldPath, "new", evt.Path)
		default:
			m.logger.Info("file event", "type", evt.EventType, "path", evt.Path)
		}

		if m.dryRun {
			continue // log only, no database writes
		}

		var err error
		switch evt.EventType {
		case "create", "write":
			err = m.catalog.Upsert(ctx, entry)
		case "remove":
			err = m.catalog.Delete(ctx, m.serverID, evt.Path)
		case "rename":
			err = m.catalog.Rename(ctx, m.serverID, evt.OldPath, entry)
		}
		if err != nil {
			return fmt.Errorf("apply %s %s: %w", evt.EventType, evt.Path, err)
		}
	}
	return nil
}

func (m *Monitor) resolvePath(rec RawRecord, resolver *pathresolver.Resolver) string {
	// Try: resolve parent, then append filename.
	if parent, ok := resolver.Resolve(rec.ParentFRN); ok {
		path := filepath.Join(parent, rec.FileName)
		resolver.Cache(rec.FRN, path)
		return path
	}
	// Fallback: resolve FRN directly.
	if path, ok := resolver.Resolve(rec.FRN); ok {
		return path
	}
	return ""
}

func (m *Monitor) sleep(ctx context.Context) {
	select {
	case <-ctx.Done():
	case <-time.After(m.pollDelay):
	}
}

// enrichSize fetches the current file size from the filesystem for non-remove events.
func enrichSize(evt *model.ChangeEvent) {
	if evt.EventType == "remove" || evt.IsDir {
		return
	}
	if info, err := os.Stat(evt.Path); err == nil {
		size := info.Size()
		evt.Size = &size
	}
}
