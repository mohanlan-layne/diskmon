// Package model defines shared data structures used across diskmon.
package model

import "time"

// ChangeEvent represents a single file system change detected by the USN Journal.
type ChangeEvent struct {
	EventType  string    // create / write / rename / remove
	Path       string    // full normalized path
	OldPath    string    // previous path, rename events only
	IsDir      bool
	Size       *int64
	Ext        string    // lowercase extension including dot, e.g. ".pdf"
	Volume     string    // e.g. "D:"
	FRN        uint64    // file reference number
	ParentFRN  uint64
	USN        int64
	OccurredAt time.Time
}

// FileEntry is the record written to the file_catalog table.
type FileEntry struct {
	ServerID  string
	Volume    string
	Path      string
	IsDir     bool
	Size      *int64
	Ext       string // null for directories
	BizKey    string // material+version, empty when rule cannot extract
	UpdatedAt time.Time
}
