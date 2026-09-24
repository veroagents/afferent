//go:build !windows

package brainsrvcfg

import (
	"fmt"
	"os"
	"syscall"
)

// openKeyFile opens path read-only without following a final symlink
// (O_NOFOLLOW fails with ELOOP on one) and without blocking on a FIFO
// (O_NONBLOCK; the regular-file check then rejects it).
func openKeyFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if pe, ok := err.(*os.PathError); ok && pe.Err == syscall.ELOOP {
			return nil, fmt.Errorf("%s is a symlink; point at the file itself", path)
		}
		return nil, err
	}
	return f, nil
}

// checkKeyFileOwner enforces PLAN B-8 on Unix: owned by the current user and
// no group/other permission bits. info comes from the open descriptor.
func checkKeyFileOwner(path string, info os.FileInfo) error {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		if int(st.Uid) != os.Getuid() {
			return fmt.Errorf("key file %s is owned by uid %d, not the current user (uid %d)", path, st.Uid, os.Getuid())
		}
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("key file %s has permissions %#o; run chmod 600 %s", path, perm, path)
	}
	return nil
}
