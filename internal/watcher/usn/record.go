//go:build windows

package usn

import (
	"path/filepath"
	"strings"
	"time"

	"diskmon/internal/model"
)

// USN reason flag bitmasks.
const (
	ReasonDataOverwrite   uint32 = 0x00000001
	ReasonDataExtend      uint32 = 0x00000002
	ReasonDataTruncation  uint32 = 0x00000004
	ReasonBasicInfoChange uint32 = 0x00008000
	ReasonRenameOldName   uint32 = 0x00001000
	ReasonRenameNewName   uint32 = 0x00002000
	ReasonFileCreate      uint32 = 0x00000100
	ReasonFileDelete      uint32 = 0x00000200
	ReasonClose           uint32 = 0x80000000

	AttrDirectory uint32 = 0x00000010
)

// MapEventType converts a USN reason bitmask to a diskmon event type string.
// Returns an empty string for reasons that should be ignored.
func MapEventType(reason uint32) string {
	switch {
	case reason&ReasonFileDelete != 0:
		return "remove"
	case reason&ReasonFileCreate != 0:
		return "create"
	case reason&ReasonRenameOldName != 0:
		return "rename" // signals the old-name side; paired in monitor
	case reason&ReasonRenameNewName != 0:
		return "rename" // signals the new-name side; paired in monitor
	case reason&(ReasonDataOverwrite|ReasonDataExtend|ReasonDataTruncation|ReasonBasicInfoChange) != 0:
		return "write"
	default:
		return ""
	}
}

// IsRenameOld reports whether the record is the old-name side of a rename.
func IsRenameOld(reason uint32) bool { return reason&ReasonRenameOldName != 0 }

// IsRenameNew reports whether the record is the new-name side of a rename.
func IsRenameNew(reason uint32) bool { return reason&ReasonRenameNewName != 0 }

// IsDirectory reports whether the record refers to a directory.
func IsDirectory(attrs uint32) bool { return attrs&AttrDirectory != 0 }

// WindowsFileTimeToTime converts a Windows FILETIME value (100-ns intervals
// since 1601-01-01 UTC) to a Go time.Time in UTC.
func WindowsFileTimeToTime(ft int64) time.Time {
	const epochDiff = 116444736000000000 // 100-ns intervals between 1601 and 1970
	ns := (ft - epochDiff) * 100
	return time.Unix(0, ns).UTC()
}

// ToFileEntry converts a resolved path and metadata into a model.FileEntry
// suitable for writing to the catalog.
func ToFileEntry(serverID, volume, path string, isDir bool, size *int64, updatedAt time.Time) model.FileEntry {
	ext := ""
	if !isDir {
		ext = strings.ToLower(filepath.Ext(path))
	}
	return model.FileEntry{
		ServerID:  serverID,
		Volume:    volume,
		Path:      path,
		IsDir:     isDir,
		Size:      size,
		Ext:       ext,
		UpdatedAt: updatedAt,
	}
}
