//go:build windows

package brainsrvcfg

import "os"

// openKeyFile opens path read-only. Windows has no O_NOFOLLOW; ReadKeyFile's
// Lstat still rejects a symlink at the path.
func openKeyFile(path string) (*os.File, error) { return os.Open(path) }

// checkKeyFileOwner is a no-op on Windows: Unix owner and mode bits do not
// describe NTFS ACLs. Symlink and regular-file checks still apply.
func checkKeyFileOwner(string, os.FileInfo) error { return nil }
