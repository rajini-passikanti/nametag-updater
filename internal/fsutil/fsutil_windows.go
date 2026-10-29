//go:build windows

package fsutil

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// errSharingViolation is ERROR_SHARING_VIOLATION, which the syscall package
// does not export.
const errSharingViolation syscall.Errno = 32

// SyncDir is a no-op on Windows: NTFS journals metadata changes such as
// renames, and directories cannot be opened for flushing through os.Open.
func SyncDir(string) error { return nil }

// TryLock opens path with no sharing allowed. While the handle is open, any
// other attempt to open the file fails with a sharing violation.
func TryLock(path string) (*Lock, error) {
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := syscall.CreateFile(name,
		syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		0, // no sharing
		nil,
		syscall.OPEN_ALWAYS,
		syscall.FILE_ATTRIBUTE_NORMAL,
		0)
	if err != nil {
		if errors.Is(err, errSharingViolation) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	return &Lock{f: os.NewFile(uintptr(h), path)}, nil
}
