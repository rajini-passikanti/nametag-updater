package fsutil

import (
	"context"
	"errors"
	"time"
)

// LockContext waits for an inter-process lock without ignoring cancellation.
func LockContext(ctx context.Context, path string) (*Lock, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		lock, err := TryLock(path)
		if !errors.Is(err, ErrLocked) {
			return lock, err
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}
