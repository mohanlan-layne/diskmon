package handler

import "testing"

func TestAListMountsResolve(t *testing.T) {
	m := AListMounts{
		Base: "http://host:5244",
		Mounts: []AListMount{
			{Prefix: `E:\smb\folder1\doc1`, Mount: "/doc1"},
			{Prefix: `E:\smb\folder1\doc2`, Mount: "/doc2"},
		},
	}

	tests := []struct {
		name    string
		path    string
		wantURL string
		wantOK  bool
	}{
		{
			name:    "user example",
			path:    `E:\smb\folder1\doc1\subfolder1\111.txt`,
			wantURL: "http://host:5244/d/doc1/subfolder1/111.txt",
			wantOK:  true,
		},
		{
			name:    "second mount",
			path:    `E:\smb\folder1\doc2\a.pdf`,
			wantURL: "http://host:5244/d/doc2/a.pdf",
			wantOK:  true,
		},
		{
			name:    "forward slashes and case-insensitive",
			path:    `e:/SMB/folder1/doc1/x/y.txt`,
			wantURL: "http://host:5244/d/doc1/x/y.txt",
			wantOK:  true,
		},
		{
			name:    "file directly in monitored dir",
			path:    `E:\smb\folder1\doc1\top.txt`,
			wantURL: "http://host:5244/d/doc1/top.txt",
			wantOK:  true,
		},
		{
			name:   "path outside any mount",
			path:   `E:\smb\folder1\other\z.txt`,
			wantOK: false,
		},
		{
			name:    "spaces in path",
			path:    `E:\smb\folder1\doc1\my file.txt`,
			wantURL: "http://host:5244/d/doc1/my file.txt",
			wantOK:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := m.fileURL(tt.path)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && got != tt.wantURL {
				t.Errorf("url = %q, want %q", got, tt.wantURL)
			}
		})
	}
}

// TestPreviewFileURL verifies the path is percent-encoded for the Base64 form
// embedded in the kkFileView preview URL.
func TestPreviewFileURL(t *testing.T) {
	m := AListMounts{
		Base:   "http://192.168.1.182:30244",
		Mounts: []AListMount{{Prefix: `E:\filecenter`, Mount: "/filecenter"}},
	}

	tests := []struct {
		name    string
		path    string
		wantURL string
	}{
		{
			name:    "chinese filename is percent-encoded",
			path:    `E:\filecenter\电气线边仓库位码.pdf`,
			wantURL: "http://192.168.1.182:30244/d/filecenter/%E7%94%B5%E6%B0%94%E7%BA%BF%E8%BE%B9%E4%BB%93%E5%BA%93%E4%BD%8D%E7%A0%81.pdf",
		},
		{
			name:    "spaces escaped",
			path:    `E:\filecenter\my file.txt`,
			wantURL: "http://192.168.1.182:30244/d/filecenter/my%20file.txt",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := m.previewFileURL(tt.path)
			if !ok {
				t.Fatalf("previewFileURL ok = false, want true")
			}
			if got != tt.wantURL {
				t.Errorf("url = %q, want %q", got, tt.wantURL)
			}
		})
	}
}
