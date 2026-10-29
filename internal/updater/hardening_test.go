package updater

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"nametag-updater/internal/fsutil"
)

func installCandidate(t *testing.T, f *fixture) *Updater {
	t.Helper()
	f.publish("1.1.0", newBinary, nil)
	u := f.updater("1.0.0", nil)
	if res, err := u.CheckOnce(context.Background()); err != nil || !res.Installed {
		t.Fatalf("install = %+v, %v", res, err)
	}
	return u
}

func TestPendingUpdatePreservesConfirmedFallback(t *testing.T) {
	f := newFixture(t)
	old := installCandidate(t, f)
	f.publish("1.2.0", []byte("third binary"), nil)
	next := f.updater("1.1.0", func(c *Config) { c.HealthyAfter = time.Hour })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if rolled, err := next.Startup(ctx); err != nil || rolled {
		t.Fatalf("startup = %v, %v", rolled, err)
	}
	res, err := next.CheckOnce(ctx)
	if err != nil || !res.Deferred || res.Installed {
		t.Fatalf("pending check = %+v, %v", res, err)
	}
	assertBackup(t, f, oldBinary)
	// The old process must not erase recovery state or confirm its replacement.
	if _, err := old.Startup(ctx); !errors.Is(err, ErrStaleProcess) {
		t.Fatalf("old startup = %v", err)
	}
	old.confirmAfter(ctx, 0)
	if p := f.state(next).Pending; p == nil || p.Version != mustVersion(t, "1.1.0") {
		t.Fatalf("pending lost: %+v", p)
	}
	// Even a freshly constructed old-version updater cannot clear the marker.
	stale := f.updater("1.0.0", nil)
	if _, err := stale.Startup(ctx); !errors.Is(err, ErrStaleProcess) {
		t.Fatalf("stale startup = %v", err)
	}
	assertBackup(t, f, oldBinary)
	next.confirmAfter(ctx, 0)
	if _, err := old.CheckOnce(ctx); !errors.Is(err, ErrStaleProcess) {
		t.Fatalf("stale check = %v", err)
	}
	res, err = next.CheckOnce(ctx)
	if err != nil || !res.Installed {
		t.Fatalf("confirmed check = %+v, %v", res, err)
	}
	assertBackup(t, f, newBinary)
}

func TestFailedRollbackDoesNotRequestRelaunch(t *testing.T) {
	for _, failure := range []string{"missing", "changed", "swap error"} {
		t.Run(failure, func(t *testing.T) {
			f := newFixture(t)
			installCandidate(t, f)
			u := f.updater("1.1.0", nil)
			st := f.state(u)
			st.Pending.Boots = maxFailedBoots
			if err := saveState(u.statePath(), st); err != nil {
				t.Fatal(err)
			}
			switch failure {
			case "missing":
				if err := os.Remove(prevPath(f.exe)); err != nil {
					t.Fatal(err)
				}
			case "changed":
				if err := os.WriteFile(prevPath(f.exe), []byte("wrong backup"), 0o755); err != nil {
					t.Fatal(err)
				}
			case "swap error":
				u.swap = func(string, string) (bool, error) { return false, errors.New("access denied") }
			}
			rolled, err := u.Startup(context.Background())
			if err == nil || rolled {
				t.Fatalf("failed rollback requested relaunch: %v, %v", rolled, err)
			}
			if p := f.state(u).Pending; p == nil || p.Boots != maxFailedBoots {
				t.Fatalf("recovery record lost: %+v", p)
			}
			if !bytes.Equal(f.exeContent(), newBinary) {
				t.Fatal("failed restore changed executable")
			}
		})
	}
}

func TestCommittedSwapErrorPreservesRecovery(t *testing.T) {
	f := newFixture(t)
	f.publish("1.1.0", newBinary, nil)
	u := f.updater("1.0.0", nil)
	syncErr := errors.New("directory sync failed")
	u.swap = func(exe, replacement string) (bool, error) {
		committed, err := swapExecutable(exe, replacement)
		if err != nil {
			return committed, err
		}
		return true, syncErr
	}
	res, err := u.CheckOnce(context.Background())
	if !res.Installed || !errors.Is(err, syncErr) {
		t.Fatalf("committed result = %+v, %v", res, err)
	}
	if p := f.state(u).Pending; p == nil {
		t.Fatal("committed replacement lost pending state")
	}
	assertBackup(t, f, oldBinary)
	if _, err := u.CheckOnce(context.Background()); !errors.Is(err, ErrStaleProcess) {
		t.Fatalf("retry = %v", err)
	}
	assertBackup(t, f, oldBinary)
	next := f.updater("1.1.0", func(c *Config) { c.HealthyAfter = time.Hour })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if rolled, err := next.Startup(ctx); err != nil || rolled {
		t.Fatalf("recovery startup = %v, %v", rolled, err)
	}
	if p := f.state(next).Pending; p == nil || p.Boots != 1 {
		t.Fatalf("rollback not armed: %+v", p)
	}
}

func TestHandoverFailureRestoresAndBlocksCandidate(t *testing.T) {
	f := newFixture(t)
	u := installCandidate(t, f)
	launchErr := errors.New("cannot create process")
	u.launch = func(string, []string) error {
		if l, err := fsutil.TryLock(u.lockPath()); !errors.Is(err, fsutil.ErrLocked) {
			if l != nil {
				l.Unlock()
			}
			t.Fatalf("handover did not hold installation lock: %v", err)
		}
		return launchErr
	}
	if err := u.Handover(context.Background(), []string{"--interval", "1s"}); !errors.Is(err, launchErr) {
		t.Fatalf("handover = %v", err)
	}
	if !bytes.Equal(f.exeContent(), oldBinary) {
		t.Fatal("failed handover did not restore old executable")
	}
	st := f.state(u)
	if st.Pending != nil || !st.isBlocked(mustVersion(t, "1.1.0")) {
		t.Fatalf("unsafe recovery state: %+v", st)
	}
	if res, err := u.CheckOnce(context.Background()); err != nil || res.Installed {
		t.Fatalf("resumed updater = %+v, %v", res, err)
	}
}

func TestFailedHandoverRecoveryRequiresOperator(t *testing.T) {
	f := newFixture(t)
	u := installCandidate(t, f)
	if err := os.Remove(prevPath(f.exe)); err != nil {
		t.Fatal(err)
	}
	u.launch = func(string, []string) error { return errors.New("cannot launch") }
	err := u.Handover(context.Background(), nil)
	if !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("unrecoverable handover = %v", err)
	}
	if f.state(u).Pending == nil {
		t.Fatal("lost recovery record")
	}
}

func TestInstanceLockSerializesApplicationLifetimes(t *testing.T) {
	f := newFixture(t)
	u := f.updater("1.0.0", nil)
	instance, err := u.LockInstance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	other := f.updater("1.0.0", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if lock, err := other.LockInstance(ctx); !errors.Is(err, context.DeadlineExceeded) {
		if lock != nil {
			lock.Unlock()
		}
		t.Fatalf("concurrent lifetime allowed: %v", err)
	}
	if err := instance.Unlock(); err != nil {
		t.Fatal(err)
	}
	lock, err := other.LockInstance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	lock.Unlock()
}

func TestUnreadableStateIsPreserved(t *testing.T) {
	for _, content := range []string{"{broken", `{"high_water":"invalid"}`, "null", `{"pending":{"from":"1.0.0","version":"1.1.0","boots":-1}}`} {
		t.Run(content, func(t *testing.T) {
			f := newFixture(t)
			f.publish("1.1.0", newBinary, nil)
			u := f.updater("1.0.0", nil)
			if err := os.WriteFile(u.statePath(), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := u.Startup(context.Background()); err == nil {
				t.Fatal("startup accepted corrupt state")
			}
			if _, err := u.CheckOnce(context.Background()); err == nil {
				t.Fatal("update accepted corrupt state")
			}
			u.confirmAfter(context.Background(), 0)
			got, err := os.ReadFile(u.statePath())
			if err != nil || string(got) != content {
				t.Fatalf("state overwritten: %q, %v", got, err)
			}
			if !bytes.Equal(f.exeContent(), oldBinary) {
				t.Fatal("executable changed")
			}
		})
	}
}

func TestStartupWaitsForInstallationLock(t *testing.T) {
	f := newFixture(t)
	u := f.updater("1.0.0", nil)
	lock, err := fsutil.TryLock(u.lockPath())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err = u.Startup(ctx)
	lock.Unlock()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("startup bypassed lock: %v", err)
	}
	if _, err := os.Stat(u.statePath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state changed without lock: %v", err)
	}
}

func assertBackup(t *testing.T, f *fixture, want []byte) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(filepath.Dir(f.exe), filepath.Base(f.exe)+".prev"))
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("backup = %q, %v; want %q", got, err, want)
	}
}
