//go:build windows

// Package pathresolver resolves NTFS File Reference Numbers (FRN) to file paths.
// It maintains an LRU cache to avoid repeated expensive syscalls.
package pathresolver

import (
	"fmt"
	"strings"
	"unsafe"

	lru "github.com/hashicorp/golang-lru/v2"
	"golang.org/x/sys/windows"
)

// Resolver maps FRN → absolute path using OpenFileById + GetFinalPathNameByHandle.
type Resolver struct {
	volumeHandle windows.Handle
	cache        *lru.Cache[uint64, string]
}

// New opens a handle to the volume and creates an LRU cache of the given capacity.
// volume must be of the form "D:" (no trailing backslash).
func New(volume string, cacheSize int) (*Resolver, error) {
	// Volume path for CreateFile must end with backslash.
	volPath := strings.TrimRight(volume, `\/`) + `\`
	ptr, err := windows.UTF16PtrFromString(`\\.\` + strings.TrimRight(volume, `\/`))
	if err != nil {
		return nil, fmt.Errorf("pathresolver: UTF16 volume %s: %w", volume, err)
	}

	handle, err := windows.CreateFile(
		ptr,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return nil, fmt.Errorf("pathresolver: open volume %s: %w", volPath, err)
	}

	cache, err := lru.New[uint64, string](cacheSize)
	if err != nil {
		windows.CloseHandle(handle)
		return nil, fmt.Errorf("pathresolver: create cache: %w", err)
	}

	return &Resolver{volumeHandle: handle, cache: cache}, nil
}

// Resolve returns the absolute path for the given FRN.
// Returns ("", false) if the FRN cannot be resolved (file deleted, access denied, etc.).
func (r *Resolver) Resolve(frn uint64) (string, bool) {
	if path, ok := r.cache.Get(frn); ok {
		return path, true
	}

	descriptor := fileIDDescriptor{
		dwSize:  uint32(unsafe.Sizeof(fileIDDescriptor{})),
		idType:  0, // FileIdType
		fileID:  frn,
	}

	fileHandle, err := openFileByID(r.volumeHandle, &descriptor,
		windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return "", false
	}
	defer windows.CloseHandle(fileHandle)

	path, err := getFinalPath(fileHandle)
	if err != nil {
		return "", false
	}

	r.cache.Add(frn, path)
	return path, true
}

// Cache manually stores a known FRN → path mapping (e.g. during path resolution
// when parent FRN is already known from the USN record).
func (r *Resolver) Cache(frn uint64, path string) {
	r.cache.Add(frn, path)
}

// Close releases the volume handle.
func (r *Resolver) Close() error {
	return windows.CloseHandle(r.volumeHandle)
}

// getFinalPath calls GetFinalPathNameByHandle and strips the \\?\ prefix.
func getFinalPath(handle windows.Handle) (string, error) {
	buf := make([]uint16, 1024)
	for {
		n, err := windows.GetFinalPathNameByHandle(handle, &buf[0], uint32(len(buf)), 0)
		if err != nil {
			return "", err
		}
		if n < uint32(len(buf)) {
			path := windows.UTF16ToString(buf[:n])
			// Strip \\?\ prefix added by the API.
			path = strings.TrimPrefix(path, `\\?\`)
			// Normalise drive letter to uppercase.
			if len(path) >= 2 && path[1] == ':' {
				path = strings.ToUpper(path[:1]) + path[1:]
			}
			return path, nil
		}
		buf = make([]uint16, n+1)
	}
}

// fileIDDescriptor mirrors the Windows FILE_ID_DESCRIPTOR structure.
type fileIDDescriptor struct {
	dwSize uint32
	idType uint32
	fileID uint64
}

// openFileByID wraps the OpenFileById Windows API.
func openFileByID(
	volumeHandle windows.Handle,
	fileID *fileIDDescriptor,
	desiredAccess uint32,
	shareMode uint32,
	securityAttrs *windows.SecurityAttributes,
	creationDisposition uint32,
	flagsAndAttrs uint32,
	templateFile windows.Handle,
) (windows.Handle, error) {
	r, _, err := procOpenFileById.Call(
		uintptr(volumeHandle),
		uintptr(unsafe.Pointer(fileID)),
		uintptr(desiredAccess),
		uintptr(shareMode),
		uintptr(unsafe.Pointer(securityAttrs)),
		uintptr(flagsAndAttrs),
	)
	if windows.Handle(r) == windows.InvalidHandle {
		return windows.InvalidHandle, err
	}
	return windows.Handle(r), nil
}

var procOpenFileById = windows.NewLazySystemDLL("kernel32.dll").NewProc("OpenFileById")
