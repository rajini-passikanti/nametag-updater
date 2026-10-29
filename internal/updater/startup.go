package updater

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"nametag-updater/internal/fsutil"
)

// Startup must be called once, before Run, each time the program starts.
// It cleans up after earlier updates and tracks a freshly installed version:
// once the version has stayed up for HealthyAfter it is confirmed, and if it
// has already failed to come up maxFailedBoots times the previous version is
// restored. When Startup reports rolledBack, the caller should Relaunch.
func (u *Updater) Startup(ctx context.Context) (rolledBack bool, err error) {
	lock, err := fsutil.LockContext(ctx, u.lockPath())
	if err != nil {
		return false, err
	}
	defer lock.Unlock()
	if err := u.checkIdentity(); err != nil {
		return false, err
	}
	st, err := loadState(u.statePath())
	if err != nil {
		return false, err
	}
	if u.cfg.Current.Less(st.HighWater) {
		return false, ErrStaleProcess
	}
	removeLeftovers(u.cfg.Executable)
	if p := st.Pending; p != nil {
		digest, err := executableDigest(u.cfg.Executable)
		if err != nil {
			return false, err
		}
		switch {
		case p.Version == u.cfg.Current:
			if p.SHA256 != "" && digest != p.SHA256 {
				return false, errors.New("pending executable digest does not match")
			}
			if p.Boots >= maxFailedBoots {
				return u.rollBack(ctx, st)
			}
			st.Pending.Boots++
		case p.From == u.cfg.Current && p.FromSHA256 != "" && digest == p.FromSHA256:
			// An interrupted pre-commit install or a restored backup whose state
			// write failed. Never let an old process clear a committed replacement.
			if p.Boots >= maxFailedBoots && !st.isBlocked(p.Version) {
				st.Blocked = append(st.Blocked, p.Version)
			}
			st.Pending = nil
		default:
			return false, ErrStaleProcess
		}
	}
	if err := saveState(u.statePath(), st); err != nil {
		return false, err
	}
	go u.confirmAfter(ctx, u.cfg.HealthyAfter)
	return false, nil
}

// rollBack requires the installation lock. A failed restore must never be
// reported as a completed rollback, otherwise callers can relaunch forever.
func (u *Updater) rollBack(ctx context.Context, st State) (bool, error) {
	bad := st.Pending.Version
	if err := u.verifyBackup(st.Pending); err != nil {
		return false, err
	}
	committed, err := u.restorePrevious(u.cfg.Executable)
	if err != nil {
		u.report(ctx, Event{Kind: EventFailed, From: bad, Err: err})
		return false, fmt.Errorf("roll back %s (replacement committed=%t): %w", bad, committed, err)
	}
	from := st.Pending.From
	if !st.isBlocked(bad) {
		st.Blocked = append(st.Blocked, bad)
	}
	st.Pending = nil
	if err := saveState(u.statePath(), st); err != nil {
		return false, err
	}
	u.log.Warn("new version kept failing; rolled back", "bad_version", bad, "restored", from)
	u.report(ctx, Event{Kind: EventRolledBack, From: bad, To: from})
	return true, nil
}

func (u *Updater) confirmAfter(ctx context.Context, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return
	case <-timer.C:
	}
	lock, err := fsutil.LockContext(ctx, u.lockPath())
	if err != nil {
		return
	}
	defer lock.Unlock()
	if err := u.checkIdentity(); err != nil {
		return
	}
	st, err := loadState(u.statePath())
	if err != nil {
		u.log.Error("could not read updater state", "err", err)
		return
	}
	// A previous process must not confirm a replacement it is not running.
	if st.Pending != nil && st.Pending.Version != u.cfg.Current {
		return
	}
	confirmed := st.Pending != nil
	if st.HighWater.Less(u.cfg.Current) {
		st.HighWater = u.cfg.Current
	}
	st.Pending = nil
	if err := saveState(u.statePath(), st); err != nil {
		u.log.Error("could not record healthy version", "err", err)
		return
	}
	if confirmed {
		u.log.Info("update confirmed healthy")
		u.report(ctx, Event{Kind: EventConfirmed, To: u.cfg.Current})
	}
}

func (u *Updater) verifyBackup(p *Pending) error {
	digest, err := executableDigest(prevPath(u.cfg.Executable))
	if err != nil {
		return fmt.Errorf("read recovery backup: %w", err)
	}
	if p.FromSHA256 != "" && digest != p.FromSHA256 {
		return errors.New("recovery backup digest does not match")
	}
	return nil
}

// ErrRecoveryRequired tells the caller to preserve its workload but disable
// automatic updates until the installation is repaired and restarted.
var ErrRecoveryRequired = errors.New("updater recovery requires operator repair")

// Handover launches the installed candidate while holding the installation
// lock. The child waits in Startup until the parent releases it. If launch
// fails, restore the verified backup so the caller can resume its workload.
// Only use this after Run or CheckOnce reports an installed replacement.
func (u *Updater) Handover(ctx context.Context, args []string) error {
	lock, err := fsutil.LockContext(ctx, u.lockPath())
	if err != nil {
		return errors.Join(ErrRecoveryRequired, err)
	}
	defer lock.Unlock()
	st, err := loadState(u.statePath())
	if err != nil {
		return errors.Join(ErrRecoveryRequired, err)
	}
	p := st.Pending
	if p == nil || p.From != u.cfg.Current {
		return errors.Join(ErrRecoveryRequired, errors.New("no matching pending handover"))
	}
	digest, err := executableDigest(u.cfg.Executable)
	if err == nil && p.SHA256 != "" && digest != p.SHA256 {
		err = errors.New("handover executable digest does not match")
	}
	if err == nil {
		err = u.launch(u.cfg.Executable, args)
	}
	if err == nil {
		return nil
	}
	launchErr := fmt.Errorf("handover failed: %w", err)
	if err := u.verifyBackup(p); err != nil {
		return errors.Join(ErrRecoveryRequired, launchErr, err)
	}
	committed, restoreErr := u.restorePrevious(u.cfg.Executable)
	if restoreErr != nil {
		return errors.Join(ErrRecoveryRequired, launchErr, fmt.Errorf("restore after handover failure (committed=%t): %w", committed, restoreErr))
	}
	u.identity, err = os.Stat(u.cfg.Executable)
	if err != nil {
		return errors.Join(ErrRecoveryRequired, launchErr, err)
	}
	if !st.isBlocked(p.Version) {
		st.Blocked = append(st.Blocked, p.Version)
	}
	st.Pending = nil
	if err := saveState(u.statePath(), st); err != nil {
		return errors.Join(ErrRecoveryRequired, launchErr, err)
	}
	u.log.Warn("handover failed; restored previous executable", "err", launchErr)
	return launchErr
}

// Executable returns the absolute path of the running binary with symlinks
// resolved, so updates replace the real file rather than a link to it.
func Executable() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	if p, err = filepath.EvalSymlinks(p); err != nil {
		return "", err
	}
	return filepath.Abs(p)
}

// Relaunch starts exe with args, sharing this process's standard streams and
// environment. The caller should exit promptly so the new process takes over.
func Relaunch(exe string, args []string) error {
	if exe == "" {
		return errors.New("relaunch: empty executable path")
	}
	cmd := exec.Command(exe, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("relaunch %s: %w", exe, err)
	}
	// A release error concerns our process handle, not whether Start launched
	// the child. Never restore the executable underneath an already-live child.
	_ = cmd.Process.Release()
	return nil
}

// LockInstance serializes application lifetimes, including health accounting.
// Hold this through Startup and the workload, and release it on process exit.
// A relaunched child waits here until its parent has stopped and released it.
// Libraries embedding this updater should use this or equivalent supervision.
func (u *Updater) LockInstance(ctx context.Context) (*fsutil.Lock, error) {
	return fsutil.LockContext(ctx, filepath.Join(u.cfg.StateDir, "app.lock"))
}
