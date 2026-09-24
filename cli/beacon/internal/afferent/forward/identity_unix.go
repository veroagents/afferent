//go:build !windows

package forward

import (
	"fmt"
	"os"
	"syscall"
)

// fileKey identifies a file across renames: device and inode. Beacon rotates
// runtime.jsonl by renaming, so an archive keeps the key it had while live.
func fileKey(fi os.FileInfo) (string, error) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return "", fmt.Errorf("no device/inode for %s", fi.Name())
	}
	return fmt.Sprintf("%d:%d", uint64(st.Dev), uint64(st.Ino)), nil // Dev is int32 on darwin, uint64 on linux
}
