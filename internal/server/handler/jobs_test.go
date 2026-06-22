package handler

import (
	"testing"

	"diskmon/internal/alist"
)

func TestDecideBackfill(t *testing.T) {
	cases := []struct {
		name string
		fi   alist.FileInfo
		want backfillAction
	}{
		{"missing file → delete", alist.FileInfo{Exists: false}, actDelete},
		{"real file size>0 → set size", alist.FileInfo{Exists: true, Size: 284291}, actSetSize},
		{"real directory → set dir", alist.FileInfo{Exists: true, IsDir: true}, actSetDir},
		{"empty file (0 bytes) → delete", alist.FileInfo{Exists: true, Size: 0}, actDelete},
		// a directory reported with size 0 must be corrected, never deleted
		{"directory with 0 size → set dir not delete", alist.FileInfo{Exists: true, IsDir: true, Size: 0}, actSetDir},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := decideBackfill(c.fi); got != c.want {
				t.Fatalf("decideBackfill(%+v) = %d, want %d", c.fi, got, c.want)
			}
		})
	}
}
