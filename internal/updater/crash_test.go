package updater

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"nametag-updater/internal/manifest"
	"nametag-updater/internal/release"
)

var newBinary = []byte("new binary")

// TestCrashAtEveryInstallStepIsRecoverable simulates a power loss at each
// step of an install. At every checkpoint it freezes a copy of the install
// directory, as the disk would be after the crash, then boots a fresh
// updater on that copy and checks that:
//
//   - the executable is intact: entirely the old binary or entirely the new;
//   - startup cleans up any half-finished files;
//   - the install converges on the new version.
func TestCrashAtEveryInstallStepIsRecoverable(t *testing.T) {
	f := newFixture(t)
	f.publish("1.1.0", newBinary, nil)
	u := f.updater("1.0.0", nil)

	var steps []string
	u.checkpoint = func(step string) {
		steps = append(steps, step)
		t.Run("crash after "+step, func(t *testing.T) {
			recoverFrom(t, f, snapshotInstall(t, filepath.Dir(f.exe)))
		})
	}
	if _, err := u.CheckOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	want := []string{"downloaded", "self-tested", "pending-recorded", "backed-up", "swapped"}
	if len(steps) != len(want) {
		t.Fatalf("visited checkpoints %v, want %v", steps, want)
	}
}

// recoverFrom boots the binary found in a crashed install and checks that
// the install ends up healthy on the new version.
func recoverFrom(t *testing.T, f *fixture, installDir string) {
	exe := filepath.Join(installDir, filepath.Base(f.exe))
	content, err := os.ReadFile(exe)
	if err != nil {
		t.Fatalf("executable missing after crash: %v", err)
	}
	var running string
	switch {
	case bytes.Equal(content, oldBinary):
		running = "1.0.0"
	case bytes.Equal(content, newBinary):
		running = "1.1.0"
	default:
		t.Fatalf("executable is neither old nor new after crash: %q", content)
	}

	edit := func(c *Config) {
		c.Executable = exe
		c.StateDir = filepath.Join(installDir, ".updater")
		c.HealthyAfter = time.Hour
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	booted := f.updater(running, edit)
	if rolledBack, err := booted.Startup(ctx); err != nil || rolledBack {
		t.Fatalf("Startup on recovered install = %v, %v", rolledBack, err)
	}
	assertNoTempFiles(t, exe)

	if running == "1.1.0" {
		// Rollback must still be armed: the new version is unconfirmed.
		if p := f.state(booted).Pending; p == nil || p.Version != mustVersion(t, "1.1.0") {
			t.Fatalf("new version is running without a pending record (no rollback possible): %+v", p)
		}
	}
	if running == "1.0.0" {
		res, err := booted.CheckOnce(ctx)
		if err != nil || !res.Installed {
			t.Fatalf("retrying the update after the crash = %+v, %v", res, err)
		}
	}
	if got, _ := os.ReadFile(exe); !bytes.Equal(got, newBinary) {
		t.Fatalf("install did not converge on the new version: %q", got)
	}
}

// snapshotInstall copies dir as it is right now. The lock file is skipped:
// it is held by the running check and carries no state.
func snapshotInstall(t *testing.T, dir string) string {
	t.Helper()
	dst := t.TempDir()
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		target := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return os.MkdirAll(target, info.Mode().Perm())
		case d.Name() == "update.lock":
			return nil
		default:
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			return os.WriteFile(target, data, info.Mode().Perm())
		}
	})
	if err != nil {
		t.Fatalf("snapshot install: %v", err)
	}
	return dst
}

// TestKeyRotation walks through a planned key rotation:
//  1. clients that trust both keys accept releases signed with either;
//  2. once a client trusts only the new key, the old key is rejected.
func TestKeyRotation(t *testing.T) {
	f := newFixture(t)
	oldKeys := f.keys
	newPub, newPriv, _ := ed25519.GenerateKey(nil)
	both := manifest.KeyRing{"k1": oldKeys["k1"], "k2": newPub}
	onlyNew := manifest.KeyRing{"k2": newPub}
	signWithNew := func(o *release.Options) { o.KeyID, o.PrivateKey = "k2", newPriv }

	f.publish("1.1.0", newBinary, signWithNew)
	if res, err := f.updater("1.0.0", func(c *Config) { c.Keys = both }).CheckOnce(context.Background()); err != nil || !res.Installed {
		t.Fatalf("client trusting both keys: %+v, %v", res, err)
	}

	f.publish("1.2.0", []byte("signed with retired key"), nil) // still k1
	next := f.updater("1.1.0", func(c *Config) { c.Keys = onlyNew })
	next.confirmAfter(context.Background(), 0) // confirm before accepting another release
	_, err := next.CheckOnce(context.Background())
	if !errors.Is(err, manifest.ErrUnknownKey) {
		t.Fatalf("client trusting only the new key: error = %v, want ErrUnknownKey", err)
	}
}
