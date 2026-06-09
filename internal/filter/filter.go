// Package filter decides which ChangeEvents should be written to the catalog.
package filter

import (
	"path/filepath"
	"strings"

	"diskmon/internal/model"
)

// Filter applies include/exclude rules to change events.
// An empty slice means "no restriction" for that dimension.
type Filter struct {
	IncludeDirs []string // only process paths under these roots (empty = all)
	ExcludeDirs []string // never process paths under these roots
	Extensions  []string // only process these extensions, e.g. ".pdf" (empty = all)
	Events      []string // only process these event types (empty = all)
}

// Match returns true if the event passes all filter criteria.
func (f *Filter) Match(evt model.ChangeEvent) bool {
	if !f.matchEvent(evt.EventType) {
		return false
	}
	if !f.matchPath(evt) {
		return false
	}
	if !evt.IsDir && !f.matchExt(evt.Ext) {
		return false
	}
	return true
}

func (f *Filter) matchEvent(eventType string) bool {
	if len(f.Events) == 0 {
		return true
	}
	lower := strings.ToLower(eventType)
	for _, e := range f.Events {
		if strings.ToLower(e) == lower {
			return true
		}
	}
	return false
}

func (f *Filter) matchPath(evt model.ChangeEvent) bool {
	path := evt.Path
	if path == "" {
		// No path: only allowed when there are no include restrictions.
		return len(f.IncludeDirs) == 0
	}

	// Recycle bin deletes always pass the include check.
	if isRecycleBinDelete(evt) {
		return !f.isExcluded(path)
	}

	if len(f.IncludeDirs) > 0 {
		if !f.isIncluded(path) {
			return false
		}
	}
	if f.isExcluded(path) {
		return false
	}
	return true
}

func (f *Filter) matchExt(ext string) bool {
	if len(f.Extensions) == 0 {
		return true
	}
	lower := strings.ToLower(ext)
	for _, e := range f.Extensions {
		if strings.ToLower(e) == lower {
			return true
		}
		if e == ".*" {
			return true
		}
	}
	return false
}

func (f *Filter) isIncluded(path string) bool {
	for _, dir := range f.IncludeDirs {
		if isUnder(path, dir) {
			return true
		}
	}
	return false
}

func (f *Filter) isExcluded(path string) bool {
	for _, dir := range f.ExcludeDirs {
		if isUnder(path, dir) {
			return true
		}
	}
	return false
}

// isUnder returns true when path equals root or is a descendant of root.
func isUnder(path, root string) bool {
	// Normalize separators.
	path = filepath.Clean(path)
	root = filepath.Clean(root)
	if strings.EqualFold(path, root) {
		return true
	}
	// Ensure we compare full path segments, not just a prefix.
	if strings.HasSuffix(root, string(filepath.Separator)) {
		return strings.HasPrefix(strings.ToLower(path), strings.ToLower(root))
	}
	return strings.HasPrefix(strings.ToLower(path),
		strings.ToLower(root)+string(filepath.Separator))
}

func isRecycleBinDelete(evt model.ChangeEvent) bool {
	if strings.ToLower(evt.EventType) != "remove" {
		return false
	}
	lower := strings.ToLower(evt.Path)
	return strings.Contains(lower, "$recycle.bin")
}
