package alist

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newStatServer(t *testing.T, code int, isDir bool, size int64, msg string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/fs/get" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(jsonBody(code, isDir, size, msg))) //nolint:errcheck
	}))
}

func jsonBody(code int, isDir bool, size int64, msg string) string {
	d := "false"
	if isDir {
		d = "true"
	}
	return `{"code":` + itoa(code) + `,"message":"` + msg +
		`","data":{"is_dir":` + d + `,"size":` + itoa64(size) + `}}`
}

func itoa(n int) string { return itoa64(int64(n)) }
func itoa64(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func TestStat_File(t *testing.T) {
	srv := newStatServer(t, 200, false, 284291, "success")
	defer srv.Close()

	fi, err := Stat(context.Background(), srv.Client(), srv.URL, "/doc1/a.step")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !fi.Exists || fi.IsDir || fi.Size != 284291 {
		t.Fatalf("got %+v, want exists file size 284291", fi)
	}
}

func TestStat_Directory(t *testing.T) {
	srv := newStatServer(t, 200, true, 0, "success")
	defer srv.Close()

	fi, err := Stat(context.Background(), srv.Client(), srv.URL, "/doc1/sub")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !fi.Exists || !fi.IsDir {
		t.Fatalf("got %+v, want exists dir", fi)
	}
}

func TestStat_ZeroSizeFile(t *testing.T) {
	srv := newStatServer(t, 200, false, 0, "success")
	defer srv.Close()

	fi, err := Stat(context.Background(), srv.Client(), srv.URL, "/doc1/empty.ldtmp")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !fi.Exists || fi.IsDir || fi.Size != 0 {
		t.Fatalf("got %+v, want exists file size 0", fi)
	}
}

func TestStat_ObjectNotFound(t *testing.T) {
	srv := newStatServer(t, 500, false, 0, "object not found")
	defer srv.Close()

	fi, err := Stat(context.Background(), srv.Client(), srv.URL, "/doc1/gone.step")
	if err != nil {
		t.Fatalf("not-found must be a clean (nil-error) result, got err: %v", err)
	}
	if fi.Exists {
		t.Fatalf("got %+v, want Exists=false", fi)
	}
}

func TestStat_OtherErrorIsError(t *testing.T) {
	// e.g. storage offline → must surface as error so the caller skips (never deletes).
	srv := newStatServer(t, 500, false, 0, "storage not found")
	defer srv.Close()

	if _, err := Stat(context.Background(), srv.Client(), srv.URL, "/doc1/x.step"); err == nil {
		t.Fatal("expected error for non-not-found failure, got nil")
	}
}
