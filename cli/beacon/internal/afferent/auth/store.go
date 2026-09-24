package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/afferent/config"
)

// ErrNotFound means no credentials are stored.
var ErrNotFound = errors.New("no stored credentials")

// Store persists one set of credentials.
type Store interface {
	Load() (*Credentials, error) // ErrNotFound when there are none
	Save(*Credentials) error
	Delete() error // ErrNotFound when there was nothing to delete
	Name() string  // for messages, e.g. "macOS Keychain"
}

// FileStore keeps credentials in a 0600 JSON file. Load refuses a symlink,
// a file owned by someone else, or one with group/other permission bits.
type FileStore struct{ Path string }

// CredentialsFile returns the default file path inside the config dir.
func CredentialsFile(dir string) string { return filepath.Join(dir, "credentials.json") }

func (f *FileStore) Name() string { return f.Path }

func (f *FileStore) Load() (*Credentials, error) {
	fh, err := openNoFollow(f.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	info, err := fh.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", f.Path)
	}
	if err := checkOwnerAndMode(f.Path, info); err != nil {
		return nil, err
	}
	b, err := io.ReadAll(io.LimitReader(fh, maxBody))
	if err != nil {
		return nil, err
	}
	var c Credentials
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", f.Path, err)
	}
	return &c, nil
}

func (f *FileStore) Save(c *Credentials) error {
	if err := config.EnsureDir(filepath.Dir(f.Path)); err != nil {
		return err
	}
	if fi, err := os.Lstat(f.Path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink; refusing to write credentials through it", f.Path)
	}
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return config.WriteFileAtomic(f.Path, b, 0o600)
}

func (f *FileStore) Delete() error {
	err := os.Remove(f.Path)
	if errors.Is(err, os.ErrNotExist) {
		return ErrNotFound
	}
	return err
}

// FallbackStore uses Primary (the Keychain) and falls back to Secondary (the
// file) when Primary is unavailable or fails.
type FallbackStore struct {
	Primary   Store
	Secondary Store
	// Warn reports a Primary failure; nil discards it.
	Warn func(error)
}

func (s *FallbackStore) Name() string { return s.Primary.Name() }

func (s *FallbackStore) warn(err error) {
	if s.Warn != nil {
		s.Warn(err)
	}
}

func (s *FallbackStore) Load() (*Credentials, error) {
	c, err := s.Primary.Load()
	if err == nil {
		return c, nil
	}
	if !errors.Is(err, ErrNotFound) {
		s.warn(fmt.Errorf("%s: %w", s.Primary.Name(), err))
	}
	return s.Secondary.Load()
}

func (s *FallbackStore) Save(c *Credentials) error {
	err := s.Primary.Save(c)
	if err == nil {
		// Do not leave an older copy behind in the file.
		if derr := s.Secondary.Delete(); derr != nil && !errors.Is(derr, ErrNotFound) {
			s.warn(derr)
		}
		return nil
	}
	s.warn(fmt.Errorf("%s: %w; storing credentials in %s instead", s.Primary.Name(), err, s.Secondary.Name()))
	return s.Secondary.Save(c)
}

func (s *FallbackStore) Delete() error {
	e1 := s.Primary.Delete()
	e2 := s.Secondary.Delete()
	realErr := func(e error) bool { return e != nil && !errors.Is(e, ErrNotFound) }
	switch {
	case realErr(e2):
		return e2
	case realErr(e1) && e2 == nil:
		// The file copy (the one in use while the Keychain is unavailable)
		// is gone; report the Keychain problem without failing.
		s.warn(fmt.Errorf("%s: %w", s.Primary.Name(), e1))
		return nil
	case realErr(e1):
		return e1
	case e1 != nil && e2 != nil:
		return ErrNotFound
	}
	return nil
}
