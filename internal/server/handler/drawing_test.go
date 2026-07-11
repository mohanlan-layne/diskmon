package handler

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
)

func TestTempStore_PutGet(t *testing.T) {
	s := &tempStore{entries: make(map[string]tempEntry)}

	f, err := os.CreateTemp("", "ts-test-*")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	defer os.Remove(f.Name())

	key := s.put(f.Name())
	if key == "" {
		t.Fatal("put returned empty key")
	}

	path, ok := s.get(key)
	if !ok || path != f.Name() {
		t.Fatalf("get: want (%s, true), got (%s, %v)", f.Name(), path, ok)
	}

	// get 可重复取（v1 语义：TTL 内可反复下载）
	path2, ok2 := s.get(key)
	if !ok2 || path2 != f.Name() {
		t.Fatalf("repeat get: want (%s, true), got (%s, %v)", f.Name(), path2, ok2)
	}
}

func TestTempStore_Expiry(t *testing.T) {
	s := &tempStore{entries: make(map[string]tempEntry)}

	key := randomHex()
	s.mu.Lock()
	s.entries[key] = tempEntry{path: "/tmp/fake-drawing-test", expires: time.Now().Add(-time.Second)}
	s.mu.Unlock()

	_, ok := s.get(key)
	if ok {
		t.Fatal("expired entry should not be returned by get")
	}
}

// makeA4PDF writes a minimal one-page A4 PDF (from a white PNG) and returns its path.
func makeA4PDF(t *testing.T, dir string) string {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 595, 842))
	for y := 0; y < 842; y++ {
		for x := 0; x < 595; x++ {
			img.SetNRGBA(x, y, color.NRGBA{255, 255, 255, 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	pngPath := filepath.Join(dir, "src.png")
	if err := os.WriteFile(pngPath, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write png: %v", err)
	}
	pdfPath := filepath.Join(dir, "src.pdf")
	if err := api.ImportImagesFile([]string{pngPath}, pdfPath, nil, nil); err != nil {
		t.Fatalf("import images: %v", err)
	}
	return pdfPath
}

// TestAnnotatePages verifies the native CJK text watermark end-to-end: the
// embedded font installs, a Chinese stamp is applied, and the output is a valid
// parseable PDF. Locks in the watermark fix (crisp red vector text at 200,550).
func TestAnnotatePages(t *testing.T) {
	dir := t.TempDir()
	src := makeA4PDF(t, dir)

	conf := model.NewDefaultConfiguration()
	conf.ValidationMode = model.ValidationRelaxed

	out, cleanup, err := annotatePages(src, "计划单号:PO-2024001 数量:5", conf)
	if err != nil {
		t.Fatalf("annotatePages: %v", err)
	}
	defer cleanup()

	fi, err := os.Stat(out)
	if err != nil {
		t.Fatalf("stat output: %v", err)
	}
	if fi.Size() == 0 {
		t.Fatal("annotated PDF is empty")
	}
	if err := api.ValidateFile(out, conf); err != nil {
		t.Fatalf("annotated PDF failed validation: %v", err)
	}
}

// TestInitCJKFont ensures the embedded font registers under the expected name.
func TestInitCJKFont(t *testing.T) {
	if err := initCJKFont(); err != nil {
		t.Fatalf("initCJKFont: %v", err)
	}
	fonts, err := api.ListFonts()
	if err != nil {
		t.Fatalf("list fonts: %v", err)
	}
	for _, f := range fonts {
		if bytes.Contains([]byte(f), []byte(cjkFontName)) {
			return
		}
	}
	t.Fatalf("font %s not registered after initCJKFont; got %v", cjkFontName, fonts)
}

func TestWinParentDir(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{`D:\foo\bar\file.pdf`, `D:\foo\bar`},
		{`D:\foo\file.pdf`, `D:\foo`},
		{`D:/foo/bar/file.pdf`, `D:\foo\bar`}, // forward slashes normalised
		{`file.pdf`, ``},
	}
	for _, tt := range tests {
		got := winParentDir(tt.input)
		if got != tt.want {
			t.Errorf("winParentDir(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
