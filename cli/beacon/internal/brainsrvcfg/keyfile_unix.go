//go:build !windows

package brainsrvcfg

import (
	"fmt"
	"os"
	"syscall"
)

// checkKeyFileOwner enforces PLAN B-8 on Unix: owned by the current user and
// no group/other permission bits.
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
