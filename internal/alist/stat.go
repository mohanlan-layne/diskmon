package alist

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// FileInfo is the subset of AList's /api/fs/get response that the catalog
// backfill job needs.
type FileInfo struct {
	IsDir  bool  // true when the path is a directory
	Size   int64 // file size in bytes (0 for directories)
	Exists bool  // false when AList reports the object does not exist
}

// Stat fetches metadata for a single AList virtual path via POST /api/fs/get,
// using guest (anonymous) access — the same access level the ZIP/download paths
// rely on. base is the AList public base URL (e.g. http://host:5244); alistPath
// is the raw, unescaped virtual path (e.g. "/doc1/电气/图.step").
//
// Existence semantics are deliberately conservative so the caller can safely
// delete catalog rows for missing files without risking deletion on a transient
// outage:
//   - code 200            → Exists=true, with IsDir/Size filled.
//   - "object not found"  → Exists=false, nil error (the file is truly gone).
//   - any other failure   → non-nil error (storage offline, network, etc.); the
//     caller should skip the row and retry next run, never delete on this.
func Stat(ctx context.Context, hc *http.Client, base, alistPath string) (FileInfo, error) {
	body, _ := json.Marshal(map[string]string{"path": alistPath, "password": ""})
	url := strings.TrimRight(base, "/") + "/api/fs/get"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return FileInfo{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := hc.Do(req)
	if err != nil {
		return FileInfo{}, err
	}
	defer resp.Body.Close()

	var out struct {
		Code int    `json:"code"`
		Msg  string `json:"message"`
		Data struct {
			IsDir bool  `json:"is_dir"`
			Size  int64 `json:"size"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return FileInfo{}, fmt.Errorf("fs/get %s: decode: %w", alistPath, err)
	}

	if out.Code == 200 {
		return FileInfo{IsDir: out.Data.IsDir, Size: out.Data.Size, Exists: true}, nil
	}
	// AList returns "object not found" for a path that no longer exists. Match it
	// narrowly so "storage not found" and other infrastructure errors stay errors
	// (which the caller skips rather than treating as a missing file).
	if strings.Contains(strings.ToLower(out.Msg), "object not found") {
		return FileInfo{Exists: false}, nil
	}
	return FileInfo{}, fmt.Errorf("fs/get %s (code %d): %s", alistPath, out.Code, out.Msg)
}
