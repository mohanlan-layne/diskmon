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
