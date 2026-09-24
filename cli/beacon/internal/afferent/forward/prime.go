package forward

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/afferent/config"
)

// place reconciles the checkpoints with the files on disk (on a first run,
// at the end of each file) and saves them, without reading any data.
func (fw *Forwarder) place() error {
	views, err := openViews(fw.opts.LogPath)
	if err != nil {
		return err
	}
	defer closeViews(views)
	fw.status.LogFound = len(views) > 0
	changed, err := fw.reconcile(views)
	if err != nil {
		return err
	}
	fw.refreshLag()
	if !changed {
		return nil
	}
	if err := saveState(fw.opts.StateDir, fw.state, fw.opts.Now()); err != nil {
		return fmt.Errorf("save checkpoints: %w", err)
	}
	return nil
}

// Prime makes sure the forwarder's checkpoints for logPath exist before
// something else appends history to the log (afferent sync). A first run
// starts at the end of the log, so history written before it would never be
// sent; after Prime it is new data and is. It reports whether it placed
// them. When a forwarder is running it returns ErrAlreadyRunning: that
// forwarder placed its checkpoints when it started.
func Prime(stateDir, logPath string) (bool, error) {
	if err := config.EnsureDir(stateDir); err != nil {
		return false, err
	}
	release, ok, err := tryLock(filepath.Join(stateDir, LockFileName))
	if err != nil {
		return false, fmt.Errorf("lock state directory: %w", err)
	}
	if !ok {
		return false, fmt.Errorf("%w (%s)", ErrAlreadyRunning, stateDir)
	}
	defer release()
	st, err := loadState(stateDir, logPath)
	if err != nil {
		return false, err
	}
	if st.Initialized {
		return false, nil
	}
	fw := &Forwarder{
		opts:  Options{LogPath: logPath, StateDir: stateDir, Now: time.Now, Logf: func(string, ...any) {}},
		state: st,
	}
	if err := fw.place(); err != nil {
		return false, err
	}
	return true, nil
}

// Primed reports whether checkpoints for logPath exist in stateDir.
func Primed(stateDir, logPath string) bool {
	st, err := loadState(stateDir, logPath)
	return err == nil && st.Initialized
}
