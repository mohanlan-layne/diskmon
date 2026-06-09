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
// anonymous (guest) list/get/download endpoints. It is used when a server is
// backed by AList SMB storages instead of a local SMB mount on the server pod,
// in which case the pod has no filesystem access to the files.
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

type alistObj struct {
	Name  string `json:"name"`
	Size  int64  `json:"size"`
	IsDir bool   `json:"is_dir"`
}

// addToZip recursively adds the AList object at alistPath into zw, naming
// entries under zipName (which becomes the top-level name in the archive).
// alistPath is the raw (unescaped) virtual path, e.g. /filecenter/电气/图.pdf.
func (z *alistZipper) addToZip(ctx context.Context, zw *zip.Writer, alistPath, zipName string) error {
	obj, err := z.fsGet(ctx, alistPath)
	if err != nil {
		return err
	}
	if !obj.IsDir {
		return z.addFile(ctx, zw, alistPath, zipName)
	}
	children, err := z.fsList(ctx, alistPath)
	if err != nil {
		return err
	}
	for _, c := range children {
		if err := z.addToZip(ctx, zw,
			alistPath+"/"+c.Name, zipName+"/"+c.Name); err != nil {
			return err
		}
	}
	return nil
}

func (z *alistZipper) addFile(ctx context.Context, zw *zip.Writer, alistPath, zipName string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, z.downloadURL(alistPath), nil)
	if err != nil {
		return err
	}
	resp, err := z.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: status %d", alistPath, resp.StatusCode)
	}
	w, err := zw.Create(zipName)
	if err != nil {
		return err
	}
	_, err = io.Copy(w, resp.Body)
	return err
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

func (z *alistZipper) fsGet(ctx context.Context, path string) (*alistObj, error) {
	var resp struct {
		Code int      `json:"code"`
		Msg  string   `json:"message"`
		Data alistObj `json:"data"`
	}
	if err := z.post(ctx, "/api/fs/get",
		map[string]any{"path": path, "password": ""}, &resp); err != nil {
		return nil, err
	}
	if resp.Code != 200 {
		return nil, fmt.Errorf("alist get %s: %s", path, resp.Msg)
	}
	return &resp.Data, nil
}

func (z *alistZipper) fsList(ctx context.Context, path string) ([]alistObj, error) {
	var resp struct {
		Code int    `json:"code"`
		Msg  string `json:"message"`
		Data struct {
			Content []alistObj `json:"content"`
		} `json:"data"`
	}
	if err := z.post(ctx, "/api/fs/list", map[string]any{
		"path": path, "password": "", "page": 1, "per_page": 0, "refresh": false,
	}, &resp); err != nil {
		return nil, err
	}
	if resp.Code != 200 {
		return nil, fmt.Errorf("alist list %s: %s", path, resp.Msg)
	}
	return resp.Data.Content, nil
}

func (z *alistZipper) post(ctx context.Context, path string, body, out any) error {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, z.base+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := z.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(out)
}
