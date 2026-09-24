//go:build windows

package brainsrvcfg

import "os"

// checkKeyFileOwner is a no-op on Windows: Unix owner and mode bits do not
// describe NTFS ACLs. Symlink and regular-file checks still apply.
func checkKeyFileOwner(string, os.FileInfo) error { return nil }
