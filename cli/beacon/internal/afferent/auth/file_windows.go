//go:build windows

package auth

import (
	"context"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

// openNoFollow refuses a symlink (checked with Lstat) and opens the file.
func openNoFollow(path string) (*os.File, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s is a symlink; refusing to read credentials through it", path)
	}
	return os.Open(path)
}

// checkOwnerAndMode is a no-op on Windows: the file lives in the user's
// profile, whose ACL already limits it to that user.
func checkOwnerAndMode(string, os.FileInfo) error { return nil }

func lockFile(ctx context.Context, path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	h := windows.Handle(f.Fd())
	for {
		ol := new(windows.Overlapped)
		err := windows.LockFileEx(h, windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol)
		if err == nil {
			return func() {
				_ = windows.UnlockFileEx(h, 0, 1, 0, new(windows.Overlapped))
				_ = f.Close()
			}, nil
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, fmt.Errorf("lock %s: %w", path, ctx.Err())
		case <-time.After(25 * time.Millisecond):
		}
	}
}
