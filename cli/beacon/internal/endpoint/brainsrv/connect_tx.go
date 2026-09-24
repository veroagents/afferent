package brainsrv

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// connectTx records what Connect changed so a failure can put it back. Without it a failed
// reconnect would leave the new key next to the old URL (status and the old forwarder would
// send a credential to the wrong server), or leave the previous forwarder stopped with its
// checkpoints gone.
type connectTx struct {
	userMode  bool
	manager   Forwarder
	reconnect bool

	// forwarderTouched: a unit was written; stopped: the running forwarder was stopped;
	// loaded: Load was attempted (it stops the previous instance itself).
	forwarderTouched, stopped, loaded bool

	files []savedFile
	moved []movedPath
}

type savedFile struct {
	path    string
	data    []byte
	mode    os.FileMode
	existed bool
}

type movedPath struct{ path, backup string }

func newConnectTx(userMode bool, manager Forwarder, reconnect bool) *connectTx {
	return &connectTx{userMode: userMode, manager: manager, reconnect: reconnect}
}

// writeFile remembers path's previous content (once) and writes data atomically.
func (tx *connectTx) writeFile(path string, data []byte, mode os.FileMode) error {
	if !tx.saved(path) {
		prev := savedFile{path: path}
		if info, err := os.Lstat(path); err == nil && info.Mode().IsRegular() {
			content, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			prev.data, prev.mode, prev.existed = content, info.Mode().Perm(), true
		} else if err != nil && !os.IsNotExist(err) {
			return err
		}
		tx.files = append(tx.files, prev)
	}
	return writeFileAtomic(path, data, mode)
}

func (tx *connectTx) saved(path string) bool {
	for _, f := range tx.files {
		if f.path == path {
			return true
		}
	}
	return false
}

// moveAside renames path to a backup in the state dir; commit deletes it, rollback restores
// it. A missing path is not an error.
func (tx *connectTx) moveAside(path string) error {
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		return nil
	}
	backup := filepath.Join(Dir(tx.userMode), "."+filepath.Base(path)+".rollback")
	if err := os.RemoveAll(backup); err != nil {
		return err
	}
	if err := os.Rename(path, backup); err != nil {
		return err
	}
	tx.moved = append(tx.moved, movedPath{path: path, backup: backup})
	return nil
}

func (tx *connectTx) saveConnection(c Connection) error { return SaveConnection(tx.userMode, c) }

// commit drops the backups; the new connection stands.
func (tx *connectTx) commit() {
	for _, m := range tx.moved {
		_ = os.RemoveAll(m.backup)
	}
	tx.moved, tx.files = nil, nil
}

// rollback restores files and moved directories and the service: a first connect leaves no
// forwarder, a reconnect restarts the previous one on its restored config.
func (tx *connectTx) rollback(cause error) error {
	problems := []error{cause}
	for i := len(tx.moved) - 1; i >= 0; i-- {
		m := tx.moved[i]
		if err := os.RemoveAll(m.path); err != nil {
			problems = append(problems, fmt.Errorf("restore %s: %w", m.path, err))
			continue
		}
		if err := os.Rename(m.backup, m.path); err != nil {
			problems = append(problems, fmt.Errorf("restore %s: %w", m.path, err))
		}
	}
	for i := len(tx.files) - 1; i >= 0; i-- {
		f := tx.files[i]
		var err error
		if f.existed {
			err = writeFileAtomic(f.path, f.data, f.mode)
		} else if rmErr := os.Remove(f.path); rmErr != nil && !os.IsNotExist(rmErr) {
			err = rmErr
		}
		if err != nil {
			problems = append(problems, fmt.Errorf("restore %s: %w", f.path, err))
		}
	}
	switch {
	case !tx.reconnect && tx.forwarderTouched:
		if err := tx.manager.Unload(); err != nil {
			problems = append(problems, fmt.Errorf("could not stop the incomplete forwarder: %w", err))
		}
		tx.manager.RemoveUnits()
	case tx.reconnect && (tx.stopped || tx.loaded):
		if err := tx.manager.Load(); err != nil {
			problems = append(problems, fmt.Errorf("the previous forwarder could not be restarted either: %w", err))
		} else {
			problems = append(problems, errors.New("the previous connection was restored and its forwarder restarted"))
		}
	}
	return errors.Join(problems...)
}
