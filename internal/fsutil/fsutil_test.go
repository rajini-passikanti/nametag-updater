package fsutil

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLockAcrossProcesses(t *testing.T) {
	if os.Getenv("NAMETAG_LOCK_HELPER") == "1" {
		lock, err := TryLock(os.Getenv("NAMETAG_LOCK_PATH"))
		if err != nil {
			t.Fatal(err)
		}
		defer lock.Unlock()
		if _, err := os.Stdout.WriteString("locked\n"); err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, os.Stdin)
		return
	}
	path := filepath.Join(t.TempDir(), "update.lock")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLockAcrossProcesses$")
	cmd.Env = append(os.Environ(), "NAMETAG_LOCK_HELPER=1", "NAMETAG_LOCK_PATH="+path)
	cmd.Stderr = os.Stderr
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	scanner := bufio.NewScanner(output)
	if !scanner.Scan() || scanner.Text() != "locked" {
		t.Fatal("helper did not acquire lock")
	}
	if l, err := TryLock(path); !errors.Is(err, ErrLocked) {
		if l != nil {
			l.Unlock()
		}
		t.Fatalf("bypassed child process lock: %v", err)
	}
	waitCtx, stopWait := context.WithTimeout(ctx, 50*time.Millisecond)
	defer stopWait()
	if l, err := LockContext(waitCtx, path); !errors.Is(err, context.DeadlineExceeded) {
		if l != nil {
			l.Unlock()
		}
		t.Fatalf("waiting for child lock = %v", err)
	}
	if err := input.Close(); err != nil {
		t.Fatal(err)
	}
	// Drain the test runner's final output before waiting for process exit.
	for scanner.Scan() {
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	lock, err := LockContext(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	lock.Unlock()
}

func TestWriteFileAtomicReplacesContents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	for _, content := range []string{"first", "second"} {
		if err := WriteFileAtomic(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		got, _ := os.ReadFile(path)
		if string(got) != content {
			t.Fatalf("contents = %q, want %q", got, content)
		}
	}
}

func TestWriteAtomicLeavesOriginalOnFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	os.WriteFile(path, []byte("original"), 0o600)

	err := WriteAtomic(path, 0o600, func(w io.Writer) error {
		w.Write([]byte("partial"))
		return errors.New("disk full")
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if got, _ := os.ReadFile(path); string(got) != "original" {
		t.Fatalf("contents = %q, want original", got)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), TempPrefix(path)) {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
}

func TestTryLockIsExclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "update.lock")
	l, err := TryLock(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := TryLock(path); !errors.Is(err, ErrLocked) {
		t.Fatalf("second TryLock = %v, want ErrLocked", err)
	}
	if err := l.Unlock(); err != nil {
		t.Fatal(err)
	}
	l2, err := TryLock(path)
	if err != nil {
		t.Fatalf("TryLock after Unlock: %v", err)
	}
	l2.Unlock()
}
