package handler

import "testing"

func TestZipNameFor(t *testing.T) {
	tests := []struct {
		name string
		p    string
		fp   string
		want string
	}{
		{"file selected directly", `E:\filecenter\a.pdf`, `E:\filecenter\a.pdf`, "a.pdf"},
		{"file under dir", `E:\filecenter`, `E:\filecenter\电气.pdf`, "filecenter/电气.pdf"},
		{"nested file under dir", `E:\filecenter`, `E:\filecenter\sub\b.txt`, "filecenter/sub/b.txt"},
		{"case-insensitive exact", `E:\filecenter\a.pdf`, `e:\FILECENTER\a.pdf`, "a.pdf"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := zipNameFor(tt.p, tt.fp); got != tt.want {
				t.Errorf("zipNameFor(%q,%q) = %q, want %q", tt.p, tt.fp, got, tt.want)
			}
		})
	}
}

func TestEscapeLike(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{`E:\filecenter\`, `E:\\filecenter\\`},
		{`a%b_c`, `a\%b\_c`},
		{`E:\dir\50%_done\`, `E:\\dir\\50\%\_done\\`},
	}
	for _, tt := range tests {
		if got := escapeLike(tt.in); got != tt.want {
			t.Errorf("escapeLike(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestDownloadURL(t *testing.T) {
	z := newAListZipper("http://192.168.1.182:30244/")
	got := z.downloadURL("/filecenter/电气线边仓库位码.pdf")
	want := "http://192.168.1.182:30244/d/filecenter/%E7%94%B5%E6%B0%94%E7%BA%BF%E8%BE%B9%E4%BB%93%E5%BA%93%E4%BD%8D%E7%A0%81.pdf"
	if got != want {
		t.Errorf("downloadURL = %q, want %q", got, want)
	}
}
