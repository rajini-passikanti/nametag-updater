// Package fsutil provides crash-safe file operations.
package fsutil

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// TempPrefix is the prefix of temporary files created next to path. Callers
// may delete matching files left behind by a crash.
func TempPrefix(path string) string {
	return "." + filepath.Base(path) + ".tmp-"
}

// WriteAtomic creates or replaces path with the bytes produced by write.
// Readers observe either the old file or the complete new one, and the new
// contents are flushed to stable storage before WriteAtomic returns.
func WriteAtomic(path string, perm os.FileMode, write func(io.Writer) error) (err error) {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, TempPrefix(path)+"*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			f.Close()
			os.Remove(tmp)
		}
	}()

	if err := write(f); err != nil {
		return err
	}
	if err := f.Chmod(perm); err != nil {
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync temp file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename into place: %w", err)
	}
	return SyncDir(dir)
}

// WriteFileAtomic is WriteAtomic for an in-memory buffer.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	return WriteAtomic(path, perm, func(w io.Writer) error {
		_, err := w.Write(data)
		return err
	})
}

// CopyFileAtomic copies src to dst with WriteAtomic's guarantees.
func CopyFileAtomic(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	return WriteAtomic(dst, perm, func(w io.Writer) error {
		_, err := io.Copy(w, in)
		return err
	})
}

// ErrLocked is returned by TryLock when another process holds the lock.
var ErrLocked = errors.New("lock is held by another process")

// Lock is an exclusive inter-process lock. The operating system releases it
// if the holder exits or crashes, so it can never be left stale.
type Lock struct {
	f *os.File
}

// Unlock releases the lock.
func (l *Lock) Unlock() error {
	return l.f.Close()
}
