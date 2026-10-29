//go:build windows

package updater

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// swapExecutable replaces exe with replacement. Windows will not let a
// running executable be overwritten or deleted, but it can be renamed, so the
// current file is first moved aside and then the replacement takes its name.
func swapExecutable(exe, replacement string) (bool, error) {
	aside := filepath.Join(filepath.Dir(exe),
		fmt.Sprintf("%s%d", movedAsidePrefix(exe), time.Now().UnixNano()))
	if err := renameWithRetry(exe, aside); err != nil {
		return false, fmt.Errorf("move current executable aside: %w", err)
	}
	if err := renameWithRetry(replacement, exe); err != nil {
		if rerr := renameWithRetry(aside, exe); rerr != nil {
			return false, fmt.Errorf("install new executable: %w (restoring original also failed: %v)", err, rerr)
		}
		return false, fmt.Errorf("install new executable: %w", err)
	}
	return true, nil
}

// renameWithRetry tolerates short-lived sharing violations, typically from
// antivirus or indexing services that briefly open new executables.
func renameWithRetry(from, to string) error {
	var err error
	for attempt := 1; attempt <= 5; attempt++ {
		if err = os.Rename(from, to); err == nil {
			return nil
		}
		time.Sleep(time.Duration(attempt) * 100 * time.Millisecond)
	}
	return err
}
