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
// (mount + relative), choosing the longest matching directory. When escape is
// true each path segment is percent-encoded. Returns ("", false) when no
// configured mount contains the path.
func (m AListMounts) resolveURLPath(winPath string, escape bool) (string, bool) {
	var best string
	bestLen := -1
	for _, mt := range m.Mounts {
		if rel, ok := splitByPrefix(winPath, mt.Prefix); ok {
			if l := len(mt.Prefix); l > bestLen {
				bestLen = l
				best = joinMountRel(mt.Mount, rel, escape)
			}
		}
	}
	if bestLen < 0 {
		return "", false
	}
	return best, true
}

// fileURL returns the AList direct-download URL for a Windows path with a raw
// (unescaped) path. Suitable for http.Redirect, which percent-encodes non-ASCII
// itself; double-encoding would otherwise occur.
func (m AListMounts) fileURL(winPath string) (string, bool) {
	rel, ok := m.resolveURLPath(winPath, false)
	if !ok {
		return "", false
	}
	return strings.TrimRight(m.Base, "/") + "/d" + rel, true
}

// previewFileURL returns the AList direct-download URL with each path segment
// percent-encoded (Chinese → %E7…). This is the form embedded (Base64-encoded)
// in the kkFileView preview URL, where no later auto-encoding happens.
func (m AListMounts) previewFileURL(winPath string) (string, bool) {
	rel, ok := m.resolveURLPath(winPath, true)
	if !ok {
		return "", false
	}
	return strings.TrimRight(m.Base, "/") + "/d" + rel, true
}

// joinMountRel appends a Windows-relative path to an AList mount.
// mount is like "/doc1"; rel is like "subfolder1\111.txt". When escape is true
// each segment is percent-encoded via url.PathEscape.
func joinMountRel(mount, rel string, escape bool) string {
	rel = strings.TrimLeft(strings.ReplaceAll(rel, `\`, "/"), "/")
	if rel == "" {
		return mount
	}
	if escape {
		parts := strings.Split(rel, "/")
		for i, s := range parts {
			parts[i] = url.PathEscape(s)
		}
		rel = strings.Join(parts, "/")
	}
	return strings.TrimRight(mount, "/") + "/" + rel
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
