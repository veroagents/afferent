//go:build !windows

package auth

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

// openNoFollow opens path read-only without following a final symlink and
// without blocking on a FIFO.
func openNoFollow(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		var pe *os.PathError
		if errors.As(err, &pe) && pe.Err == syscall.ELOOP {
			return nil, fmt.Errorf("%s is a symlink; refusing to read credentials through it", path)
		}
		return nil, err
	}
	return f, nil
}

func checkOwnerAndMode(path string, info os.FileInfo) error {
	if st, ok := info.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return fmt.Errorf("%s is owned by uid %d, not the current user (uid %d)", path, st.Uid, os.Getuid())
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("%s has permissions %#o; run chmod 600 %s", path, perm, path)
	}
	return nil
}

// lockFile takes an exclusive flock on path, polling until ctx is done.
// flock locks belong to the open file description, so two opens in one
// process exclude each other just as two processes do.
func lockFile(ctx context.Context, path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				_ = f.Close()
			}, nil
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EINTR {
			f.Close()
			return nil, fmt.Errorf("lock %s: %w", path, err)
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, fmt.Errorf("lock %s: %w", path, ctx.Err())
		case <-time.After(25 * time.Millisecond):
		}
	}
}
