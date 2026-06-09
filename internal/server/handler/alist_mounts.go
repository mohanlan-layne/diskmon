package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// AListMounts is the structure persisted in servers.alist_urls. It maps the
// real Windows directories monitored on a server to the AList virtual mounts
// created for them, so file paths from the catalog can be turned into AList
// preview/download URLs while hiding the real system path.
type AListMounts struct {
	Base   string       `json:"base"`   // AList base URL, e.g. http://alist...:5244
	Mounts []AListMount `json:"mounts"` // one per exposed directory
}

// AListMount maps one monitored Windows directory to its AList virtual path.
type AListMount struct {
	Prefix string `json:"prefix"` // Windows abs dir, e.g. E:\smb\folder1\doc1
	Mount  string `json:"mount"`  // AList mount path, e.g. /doc1
}

// loadAListMounts reads and parses servers.alist_urls for the given server.
func loadAListMounts(ctx context.Context, db *sql.DB, serverID string) (AListMounts, error) {
	var raw []byte
	if err := db.QueryRowContext(ctx,
		"SELECT alist_urls FROM servers WHERE server_id=?", serverID,
	).Scan(&raw); err != nil {
		return AListMounts{}, fmt.Errorf("server %s not found", serverID)
	}
	if len(raw) == 0 {
		return AListMounts{}, fmt.Errorf("no AList mounts configured for server %s", serverID)
	}
	var m AListMounts
	if err := json.Unmarshal(raw, &m); err != nil {
		return AListMounts{}, fmt.Errorf("invalid alist_urls for server %s: %w", serverID, err)
	}
	if len(m.Mounts) == 0 {
		return AListMounts{}, fmt.Errorf("no AList mounts configured for server %s", serverID)
	}
	return m, nil
}

// resolveURLPath turns a full Windows path into the AList virtual path
// (mount + relative, URL-escaped), choosing the longest matching directory.
// Returns ("", false) when no configured mount contains the path.
func (m AListMounts) resolveURLPath(winPath string) (string, bool) {
	var bestMount AListMount
	bestLen := -1
	for _, mt := range m.Mounts {
		if rel, ok := splitByPrefix(winPath, mt.Prefix); ok {
			if l := len(mt.Prefix); l > bestLen {
				bestLen = l
				bestMount = AListMount{Prefix: mt.Prefix, Mount: mt.Mount}
				bestMount.Mount = joinMountRel(mt.Mount, rel)
			}
		}
	}
	if bestLen < 0 {
		return "", false
	}
	return bestMount.Mount, true
}

// fileURL returns the AList direct-download URL for a Windows path.
func (m AListMounts) fileURL(winPath string) (string, bool) {
	rel, ok := m.resolveURLPath(winPath)
	if !ok {
		return "", false
	}
	return strings.TrimRight(m.Base, "/") + "/d" + rel, true
}

// joinMountRel appends a Windows-relative path to an AList mount, escaping each
// segment. mount is like "/doc1"; rel is like "subfolder1\111.txt".
func joinMountRel(mount, rel string) string {
	rel = strings.TrimLeft(strings.ReplaceAll(rel, `\`, "/"), "/")
	if rel == "" {
		return mount
	}
	parts := strings.Split(rel, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.TrimRight(mount, "/") + "/" + strings.Join(parts, "/")
}

// splitByPrefix returns the path relative to prefix (original case) when winPath
// equals or sits under prefix (case-insensitive, segment-aware). Both may use
// "/" or "\" separators.
func splitByPrefix(winPath, prefix string) (string, bool) {
	p := strings.ReplaceAll(winPath, "/", `\`)
	pref := strings.TrimRight(strings.ReplaceAll(prefix, "/", `\`), `\`)
	lp, lpref := strings.ToLower(p), strings.ToLower(pref)
	if lp == lpref {
		return "", true
	}
	if strings.HasPrefix(lp, lpref+`\`) {
		return p[len(pref)+1:], true
	}
	return "", false
}

// marshalAListMounts serialises mounts for storage in servers.alist_urls.
func marshalAListMounts(m AListMounts) (string, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return "", fmt.Errorf("marshal alist mounts: %w", err)
	}
	return string(b), nil
}
