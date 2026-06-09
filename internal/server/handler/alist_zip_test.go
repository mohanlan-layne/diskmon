package handler

import (
	"archive/zip"
	"bytes"
	"context"
	"os"
	"testing"
)

// TestAListZipperLive is an integration test against a live AList server. It is
// skipped unless DISKMON_ALIST_BASE and DISKMON_ALIST_PATH are set, e.g.:
//
//	DISKMON_ALIST_BASE=http://192.168.1.182:30244 \
//	DISKMON_ALIST_PATH=/filecenter \
//	go test ./internal/server/handler -run TestAListZipperLive -v
func TestAListZipperLive(t *testing.T) {
	base := os.Getenv("DISKMON_ALIST_BASE")
	path := os.Getenv("DISKMON_ALIST_PATH")
	if base == "" || path == "" {
		t.Skip("set DISKMON_ALIST_BASE and DISKMON_ALIST_PATH to run")
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	z := newAListZipper(base)
	if err := z.addToZip(context.Background(), zw, path, lastSegment(path)); err != nil {
		t.Fatalf("addToZip: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}
	if len(zr.File) == 0 {
		t.Fatal("zip is empty")
	}
	var total uint64
	for _, f := range zr.File {
		total += f.UncompressedSize64
		t.Logf("entry: %s (%d bytes)", f.Name, f.UncompressedSize64)
	}
	t.Logf("total %d entries, %d bytes", len(zr.File), total)
}
