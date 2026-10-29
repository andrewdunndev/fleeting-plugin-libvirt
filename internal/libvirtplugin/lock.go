package libvirtplugin

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
	"time"
)

const lockPoll = 50 * time.Millisecond

var (
	lockTimeout     = 5 * time.Minute
	errLockTimedOut = errors.New("lock_file wait timed out")
)

var openFile = os.OpenFile

// openLock opens an existing file without O_CREAT, which protected_regular
// refuses on another user's file in a sticky directory, and creates it only
// when absent.
func openLock(path string) (*os.File, error) {
	f, err := openFile(path, os.O_RDONLY, 0)
	if errors.Is(err, fs.ErrNotExist) {
		f, err = openFile(path, os.O_CREATE|os.O_RDONLY, 0o644)
	}
	return f, err
}

// acquireLock takes an exclusive flock on path, waiting until ctx ends or
// lockTimeout passes. The returned func releases it.
func acquireLock(ctx context.Context, path string) (func(), error) {
	ctx, cancel := context.WithTimeoutCause(ctx, lockTimeout, errLockTimedOut)
	defer cancel()
	f, err := openLock(path)
	if err != nil {
		return nil, fmt.Errorf("opening lock_file: %w", err)
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			_ = f.Close()
			return nil, fmt.Errorf("locking %s: %w", path, err)
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, fmt.Errorf("waiting for %s: %w", path, context.Cause(ctx))
		case <-time.After(lockPoll):
		}
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
