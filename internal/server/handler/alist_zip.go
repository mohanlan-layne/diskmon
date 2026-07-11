package handler

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// alistZipper streams files from an AList server into a ZIP archive using the
// anonymous (guest) download endpoint. It is used when a server is backed by
// AList SMB storages instead of a local SMB mount on the server pod, in which
// case the pod has no filesystem access to the files. The file list comes from
// file_catalog (see zipPack); this type only fetches bytes.
type alistZipper struct {
	base string // AList public base URL, e.g. http://192.168.1.182:30244
	hc   *http.Client
}

func newAListZipper(base string) *alistZipper {
	return &alistZipper{
		base: strings.TrimRight(base, "/"),
		hc:   &http.Client{Timeout: 5 * time.Minute},
	}
}

// zipEntry is a single file to place into the archive.
type zipEntry struct {
	alistPath string // raw AList virtual path, e.g. /filecenter/电气/图.pdf
	zipName   string // entry name inside the archive, e.g. filecenter/电气/图.pdf
}

// downloadURL builds the AList direct-download URL, percent-encoding each path
// segment so non-ASCII names survive.
func (z *alistZipper) downloadURL(alistPath string) string {
	parts := strings.Split(strings.TrimLeft(alistPath, "/"), "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return z.base + "/d/" + strings.Join(parts, "/")
}

// fetch issues the AList download GET and returns the response only on HTTP 200.
// The caller is responsible for closing resp.Body.
func (z *alistZipper) fetch(ctx context.Context, alistPath string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, z.downloadURL(alistPath), nil)
	if err != nil {
		return nil, err
	}
	resp, err := z.hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("download %s: status %d", alistPath, resp.StatusCode)
	}
	// AList reports errors like "object not found" (stale catalog row, file
	// moved/deleted) as HTTP 200 with a JSON envelope {"code":500,"message":…}.
	// Detect that envelope so callers see a failure instead of streaming the
	// JSON through as file content. Legit JSON file downloads are preserved by
	// re-attaching the sniffed bytes to the body.
	if ct := resp.Header.Get("Content-Type"); strings.Contains(ct, "application/json") {
		head, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		var e struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		}
		if json.Unmarshal(head, &e) == nil && e.Code != 0 && e.Code != http.StatusOK {
			resp.Body.Close()
			return nil, fmt.Errorf("download %s: alist error %d: %s", alistPath, e.Code, e.Message)
		}
		resp.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(head), resp.Body), resp.Body}
	}
	return resp, nil
}

// download streams the AList file at alistPath to w.
func (z *alistZipper) download(ctx context.Context, alistPath string, w io.Writer) error {
	resp, err := z.fetch(ctx, alistPath)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, err = io.Copy(w, resp.Body)
	return err
}

// addFile streams one AList file into the archive under zipName.
//
// It probes AList first and only creates the ZIP entry once the file is
// confirmed to exist (HTTP 200). A file that 404s — e.g. a catalog row for a
// file that was since removed, or a stray temp file (*.ldtmp) — is skipped
// entirely rather than left as an empty entry. This is what lets us include
// NULL/0-size catalog rows safely: real files download, phantom ones drop out.
func (z *alistZipper) addFile(ctx context.Context, zw *zip.Writer, alistPath, zipName string) error {
	resp, err := z.fetch(ctx, alistPath)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	w, err := zw.Create(zipName)
	if err != nil {
		return err
	}
	_, err = io.Copy(w, resp.Body)
	return err
}
