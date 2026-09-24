//go:build windows

package forward

import (
	"fmt"
	"os"
	"syscall"
)

// fileKey identifies a file across renames. Windows has no inode in
// os.FileInfo; a rename keeps the creation time, which is precise to 100 ns
// and so tells Beacon's runtime log generations apart.
func fileKey(fi os.FileInfo) (string, error) {
	d, ok := fi.Sys().(*syscall.Win32FileAttributeData)
	if !ok {
		return "", fmt.Errorf("no file attributes for %s", fi.Name())
	}
	return fmt.Sprintf("ctime:%d", d.CreationTime.Nanoseconds()), nil
}
