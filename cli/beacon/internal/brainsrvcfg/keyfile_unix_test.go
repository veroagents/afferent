//go:build !windows

package brainsrvcfg

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The enforcing open never follows a symlink, so a path swapped for a
// symlink after ReadKeyFile's Lstat is refused rather than read.
func TestOpenKeyFileRefusesSymlinkSwappedAfterCheck(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "attacker.key")
	if err := os.WriteFile(target, []byte("spk_attacker\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "beacon.key")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	f, err := openKeyFile(link)
	if err == nil {
		f.Close()
		t.Fatal("openKeyFile followed a symlink")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("error = %v", err)
	}
}

// A FIFO at the key path must be rejected, not block the CLI on open.
func TestReadKeyFileRejectsFIFOWithoutBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "beacon.key")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	done := make(chan error, 1)
	go func() { _, err := ReadKeyFile(path); done <- err }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ReadKeyFile blocked on a FIFO")
	}
}
