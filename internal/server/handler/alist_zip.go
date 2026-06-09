package handler

import (
	"archive/zip"
	"context"
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

// addFile streams one AList file into the archive under zipName.
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
