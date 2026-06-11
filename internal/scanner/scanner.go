// Package scanner performs a full filesystem scan and populates file_catalog.
// It is intended to be run once at startup (or on --rescan) before incremental
// USN Journal monitoring begins.
package scanner

import (
	"context"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"diskmon/internal/bizrule"
	"diskmon/internal/catalog"
	"diskmon/internal/config"
	"diskmon/internal/filter"
	"diskmon/internal/model"
)

const batchSize = 1000

// Scanner performs a full scan of the configured volumes and writes file
// entries to the catalog.
type Scanner struct {
	serverID string
	volumes  []config.VolumeConfig
	rules    bizrule.Registry
	catalog  *catalog.Catalog
	filter   *filter.Filter
	logger   *slog.Logger
}

// New creates a Scanner.
func New(
	serverID string,
	volumes []config.VolumeConfig,
	rules bizrule.Registry,
	cat *catalog.Catalog,
	f *filter.Filter,
	logger *slog.Logger,
) *Scanner {
	if logger == nil {
		logger = slog.Default()
	}
	return &Scanner{
		serverID: serverID,
		volumes:  volumes,
		rules:    rules,
		catalog:  cat,
		filter:   f,
		logger:   logger,
	}
}

// Run clears existing data for this server and performs a fresh full scan.
func (s *Scanner) Run(ctx context.Context) error {
	s.logger.Info("full scan: clearing existing catalog data", "server", s.serverID)
	if err := s.catalog.TruncateServer(ctx, s.serverID); err != nil {
		return err
	}

	for _, vol := range s.volumes {
		if err := s.scanVolume(ctx, vol); err != nil {
			return err
		}
	}
	s.logger.Info("full scan complete", "server", s.serverID)
	return nil
}

func (s *Scanner) scanVolume(ctx context.Context, vol config.VolumeConfig) error {
	rule := s.rules.Get(vol.BizRule)
	roots := s.filter.IncludeDirs
	if len(roots) == 0 {
		roots = []string{vol.Name + `\`}
	}
	// Only scan roots on this volume.
	var volRoots []string
	for _, r := range roots {
		if strings.HasPrefix(strings.ToLower(r), strings.ToLower(vol.Name)) {
			volRoots = append(volRoots, r)
		}
	}
	if len(volRoots) == 0 {
		volRoots = []string{vol.Name + `\`}
	}

	s.logger.Info("scanning volume", "volume", vol.Name, "roots", volRoots)

	var batch []model.FileEntry
	var dirsDone, filesDone int64

	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		err := s.catalog.UpsertBatch(ctx, batch)
		batch = batch[:0]
		return err
	}

	// Stack-based DFS to avoid deep recursion.
	type frame struct {
		path string
		dirs []string
		dirIdx int
		files []string
		fileIdx int
		recorded bool
	}

	sort.Strings(volRoots)
	stack := make([]*frame, 0, 64)
	for _, root := range volRoots {
		stack = append(stack, &frame{path: root})
	}

	for len(stack) > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		f := stack[len(stack)-1]

		if !f.recorded {
			if info, err := os.Stat(f.path); err == nil && info.IsDir() {
				entry := s.buildEntry(f.path, true, nil, vol.Name, time.Now(), rule)
				if s.shouldIncludeDir(f.path) {
					batch = append(batch, entry)
					dirsDone++
					if len(batch) >= batchSize {
						if err := flush(); err != nil {
							return err
						}
						s.logger.Info("scan progress", "dirs", dirsDone, "files", filesDone)
					}
				}
			} else if err != nil {
				stack = stack[:len(stack)-1]
				continue
			}
			f.recorded = true
		}

		// Lazily load directory entries.
		if f.dirs == nil || f.files == nil {
			dirs, files, err := listDir(f.path)
			if err != nil {
				s.logger.Warn("cannot read directory", "path", f.path, "err", err)
				stack = stack[:len(stack)-1]
				continue
			}
			f.dirs = dirs
			f.files = files
		}

		// Push next subdirectory.
		if f.dirIdx < len(f.dirs) {
			sub := f.dirs[f.dirIdx]
			f.dirIdx++
			if !s.isExcluded(sub) {
				stack = append(stack, &frame{path: sub})
			}
			continue
		}

		// Process remaining files in this directory.
		for f.fileIdx < len(f.files) {
			filePath := f.files[f.fileIdx]
			f.fileIdx++

			info, err := os.Stat(filePath)
			if err != nil {
				continue
			}
			size := info.Size()
			entry := s.buildEntry(filePath, false, &size, vol.Name, info.ModTime(), rule)
			if s.filter.Match(model.ChangeEvent{
				EventType: "create",
				Path:      filePath,
				IsDir:     false,
				Ext:       entry.Ext,
				Volume:    vol.Name,
			}) {
				batch = append(batch, entry)
				filesDone++
				if len(batch) >= batchSize {
					if err := flush(); err != nil {
						return err
					}
					s.logger.Info("scan progress", "dirs", dirsDone, "files", filesDone)
				}
			}
		}

		// Done with this directory.
		stack = stack[:len(stack)-1]
	}

	if err := flush(); err != nil {
		return err
	}
	s.logger.Info("volume scan done", "volume", vol.Name, "dirs", dirsDone, "files", filesDone)
	return nil
}

func (s *Scanner) buildEntry(path string, isDir bool, size *int64, volume string, modTime time.Time, rule bizrule.VolumeRule) model.FileEntry {
	ext := ""
	if !isDir {
		if e := strings.ToLower(filepath.Ext(path)); len(e) <= 20 {
			ext = e
		}
	}
	return model.FileEntry{
		ServerID:  s.serverID,
		Volume:    volume,
		Path:      path,
		IsDir:     isDir,
		Size:      size,
		Ext:       ext,
		BizKey:    rule.ExtractBizKey(path),
		UpdatedAt: modTime.UTC(),
	}
}

func (s *Scanner) shouldIncludeDir(path string) bool {
	if len(s.filter.IncludeDirs) == 0 {
		return true
	}
	for _, inc := range s.filter.IncludeDirs {
		if isUnder(path, inc) || isUnder(inc, path) {
			return true
		}
	}
	return false
}

func (s *Scanner) isExcluded(path string) bool {
	for _, exc := range s.filter.ExcludeDirs {
		if isUnder(path, exc) {
			return true
		}
	}
	return false
}

// listDir returns sorted subdirectories and files for a directory.
func listDir(path string) (dirs, files []string, err error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, nil, err
	}
	for _, e := range entries {
		full := filepath.Join(path, e.Name())
		if e.IsDir() {
			dirs = append(dirs, full)
		} else if e.Type()&fs.ModeSymlink == 0 {
			files = append(files, full)
		}
	}
	return dirs, files, nil
}

func isUnder(path, root string) bool {
	path = filepath.Clean(path)
	root = filepath.Clean(root)
	if strings.EqualFold(path, root) {
		return true
	}
	sep := string(filepath.Separator)
	return strings.HasPrefix(strings.ToLower(path), strings.ToLower(root)+sep)
}
