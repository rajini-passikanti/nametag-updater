//go:build !windows

package updater

import (
	"os"
	"path/filepath"

	"nametag-updater/internal/fsutil"
)

// swapExecutable atomically replaces exe with replacement. On Unix a running
// executable can be renamed over: the old process keeps its open inode, and
// new launches see the new file. A fresh inode also keeps macOS from killing
// the process over a modified code signature.
func swapExecutable(exe, replacement string) (bool, error) {
	if err := os.Rename(replacement, exe); err != nil {
		return false, err
	}
	return true, fsutil.SyncDir(filepath.Dir(exe))
}
