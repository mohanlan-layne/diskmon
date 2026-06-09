//go:build windows

// Package usn provides NTFS USN Journal reading via Windows DeviceIoControl.
package usn

import (
	"encoding/binary"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// IOCTL control codes for USN Journal operations.
const (
	fsctlQueryUsnJournal  = 0x000900f4
	fsctlCreateUsnJournal = 0x000900e7
	fsctlReadUsnJournal   = 0x000900bb
)

// JournalInfo holds metadata about a volume's USN Journal.
type JournalInfo struct {
	JournalID    uint64
	FirstUSN     int64
	NextUSN      int64
	LowestValidUSN int64
	MaxUSN       int64
}

// RawRecord is a parsed USN_RECORD_V2 from the journal.
type RawRecord struct {
	FRN        uint64
	ParentFRN  uint64
	USN        int64
	Timestamp  int64 // Windows FILETIME (100-ns intervals since 1601-01-01)
	Reason     uint32
	FileAttrs  uint32
	FileName   string
}

// OpenVolume opens a handle to the root of a volume suitable for IOCTL calls.
// volume is e.g. "D:" (no trailing backslash).
func OpenVolume(volume string) (windows.Handle, error) {
	ptr, err := windows.UTF16PtrFromString(`\\.\` + volume)
	if err != nil {
		return windows.InvalidHandle, err
	}
	h, err := windows.CreateFile(
		ptr,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return windows.InvalidHandle, fmt.Errorf("open volume %s: %w", volume, err)
	}
	return h, nil
}

// QueryJournal returns the current USN Journal metadata for the volume.
func QueryJournal(handle windows.Handle) (JournalInfo, error) {
	// USN_JOURNAL_DATA_V0 is 56 bytes.
	var buf [56]byte
	var bytesReturned uint32
	err := windows.DeviceIoControl(
		handle,
		fsctlQueryUsnJournal,
		nil, 0,
		&buf[0], uint32(len(buf)),
		&bytesReturned, nil,
	)
	if err != nil {
		return JournalInfo{}, fmt.Errorf("query USN journal: %w", err)
	}
	return JournalInfo{
		JournalID:      binary.LittleEndian.Uint64(buf[0:]),
		FirstUSN:       int64(binary.LittleEndian.Uint64(buf[8:])),
		NextUSN:        int64(binary.LittleEndian.Uint64(buf[16:])),
		LowestValidUSN: int64(binary.LittleEndian.Uint64(buf[24:])),
		MaxUSN:         int64(binary.LittleEndian.Uint64(buf[32:])),
	}, nil
}

// EnsureJournal creates or resizes the USN Journal to at least maxSize bytes.
func EnsureJournal(handle windows.Handle, maxSize int64) error {
	// CREATE_USN_JOURNAL_DATA: MaximumSize + AllocationDelta (both uint64).
	var input [16]byte
	binary.LittleEndian.PutUint64(input[0:], uint64(maxSize))
	binary.LittleEndian.PutUint64(input[8:], uint64(maxSize/4))
	var bytesReturned uint32
	return windows.DeviceIoControl(
		handle,
		fsctlCreateUsnJournal,
		&input[0], uint32(len(input)),
		nil, 0,
		&bytesReturned, nil,
	)
}

// ReadBatch reads up to ~1 MB of USN records starting from startUSN.
// Returns the parsed records and the next USN to read from.
func ReadBatch(handle windows.Handle, startUSN int64, journalID uint64) ([]RawRecord, int64, error) {
	const bufSize = 1 << 20 // 1 MB

	// READ_USN_JOURNAL_DATA_V0 (40 bytes):
	// StartUsn(8) + ReasonMask(4) + ReturnOnlyOnClose(4) +
	// Timeout(8) + BytesToWaitFor(8) + UsnJournalID(8)
	var input [40]byte
	binary.LittleEndian.PutUint64(input[0:], uint64(startUSN))
	binary.LittleEndian.PutUint32(input[8:], 0xFFFFFFFF) // all reasons
	binary.LittleEndian.PutUint64(input[32:], journalID)

	output := make([]byte, bufSize)
	var bytesReturned uint32
	err := windows.DeviceIoControl(
		handle,
		fsctlReadUsnJournal,
		&input[0], uint32(len(input)),
		&output[0], uint32(len(output)),
		&bytesReturned, nil,
	)
	if err != nil {
		return nil, 0, err
	}
	if bytesReturned < 8 {
		return nil, startUSN, nil
	}

	// First 8 bytes of output are the NextUSN.
	nextUSN := int64(binary.LittleEndian.Uint64(output[0:8]))
	records, err := parseRecords(output[8:bytesReturned])
	return records, nextUSN, err
}

// parseRecords parses the binary USN_RECORD_V2 records from the output buffer.
func parseRecords(buf []byte) ([]RawRecord, error) {
	var records []RawRecord
	for len(buf) >= 60 { // minimum USN_RECORD_V2 size
		recLen := binary.LittleEndian.Uint32(buf[0:4])
		if recLen == 0 || uint32(len(buf)) < recLen {
			break
		}
		major := binary.LittleEndian.Uint16(buf[4:6])
		if major != 2 {
			buf = buf[recLen:]
			continue
		}

		fnLen := binary.LittleEndian.Uint16(buf[56:58])
		fnOff := binary.LittleEndian.Uint16(buf[58:60])
		var fileName string
		if int(fnOff)+int(fnLen) <= int(recLen) {
			raw := buf[fnOff : fnOff+fnLen]
			fileName = windows.UTF16ToString(
				unsafe.Slice((*uint16)(unsafe.Pointer(&raw[0])), fnLen/2),
			)
		}

		records = append(records, RawRecord{
			FRN:       binary.LittleEndian.Uint64(buf[8:16]),
			ParentFRN: binary.LittleEndian.Uint64(buf[16:24]),
			USN:       int64(binary.LittleEndian.Uint64(buf[24:32])),
			Timestamp: int64(binary.LittleEndian.Uint64(buf[32:40])),
			Reason:    binary.LittleEndian.Uint32(buf[40:44]),
			FileAttrs: binary.LittleEndian.Uint32(buf[52:56]),
			FileName:  fileName,
		})
		buf = buf[recLen:]
	}
	return records, nil
}

// IsJournalEntryDeleted returns true for the Windows error indicating that
// the requested USN has been overwritten (journal wrapped around).
func IsJournalEntryDeleted(err error) bool {
	const errorJournalEntryDeleted = windows.Errno(0xC0000900 | 0x13D)
	return err == errorJournalEntryDeleted ||
		err == windows.Errno(1181) // ERROR_JOURNAL_ENTRY_DELETED
}
