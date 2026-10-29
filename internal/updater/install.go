package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"nametag-updater/internal/fsutil"
	"nametag-updater/internal/manifest"
)

const selfTestTimeout = 10 * time.Second

// SelfTestOutput is what a healthy binary prints for --self-test.
func SelfTestOutput(v manifest.Version) string {
	return "self-test ok " + v.String()
}

// runSelfTest executes the candidate binary before it replaces the current
// one. This catches corrupt, wrong-architecture, or unlaunchable builds.
func runSelfTest(ctx context.Context, path string, want manifest.Version) error {
	ctx, cancel := context.WithTimeout(ctx, selfTestTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--self-test")
	cmd.Dir = filepath.Dir(path)
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if err != nil {
		return err
	}
	if got := strings.TrimSpace(string(out)); got != SelfTestOutput(want) {
		return fmt.Errorf("unexpected self-test output %q", got)
	}
	return nil
}

func prevPath(exe string) string { return exe + ".prev" }

// movedAsidePrefix names executables that a Windows swap moved out of the
// way. They are deleted on a later start, once the old process has exited.
func movedAsidePrefix(exe string) string {
	return "." + filepath.Base(exe) + ".old-"
}

// replaceExecutable keeps a copy of exe as the rollback target, then moves
// replacement into exe's place.
func (u *Updater) replaceExecutable(exe, replacement string, perm os.FileMode) (bool, error) {
	if err := fsutil.CopyFileAtomic(exe, prevPath(exe), perm); err != nil {
		return false, fmt.Errorf("back up current executable: %w", err)
	}
	u.checkpoint("backed-up")
	committed, err := u.swap(exe, replacement)
	if committed {
		u.checkpoint("swapped")
	}
	return committed, err
}

// restorePrevious puts the backup taken by replaceExecutable back in place.
func (u *Updater) restorePrevious(exe string) (bool, error) {
	info, err := os.Stat(prevPath(exe))
	if err != nil {
		return false, fmt.Errorf("no previous version to restore: %w", err)
	}
	// Copy rather than rename so the backup survives if this fails midway.
	tmp := filepath.Join(filepath.Dir(exe), fsutil.TempPrefix(exe)+"restore"+filepath.Ext(exe))
	defer os.Remove(tmp)
	if err := fsutil.CopyFileAtomic(prevPath(exe), tmp, info.Mode().Perm()); err != nil {
		return false, err
	}
	return u.swap(exe, tmp)
}

// removeLeftovers deletes temp files from interrupted updates and, on
// Windows, executables that were moved aside by earlier swaps.
func removeLeftovers(exe string) {
	dir := filepath.Dir(exe)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	prefixes := []string{fsutil.TempPrefix(exe), fsutil.TempPrefix(prevPath(exe)), movedAsidePrefix(exe)}
	for _, e := range entries {
		for _, p := range prefixes {
			if strings.HasPrefix(e.Name(), p) {
				// A file still in use (Windows) is retried on the next start.
				_ = os.Remove(filepath.Join(dir, e.Name()))
			}
		}
	}
}

func executableDigest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
