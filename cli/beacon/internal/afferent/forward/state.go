package forward

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/afferent/config"
)

// File names inside the state directory.
const (
	CheckpointsFile = "checkpoints.json"
	StatusFile      = "status.json"
	LockFileName    = "forward.lock"
	ScopeCacheFile  = "scope.json"
	ServiceLogFile  = "forwarder.log"
)

const stateVersion = 1

// headMax is how many leading bytes of a file the checkpoint fingerprints.
// A reused inode, or a file truncated and rewritten past the old offset,
// has a different head and is read again from the start.
const headMax = 1024

// Checkpoint is how far into one file (by identity) brainsrv has
// acknowledged. Offset is always just past a newline.
type Checkpoint struct {
	// Path is where the file was last seen (informational; the key is the
	// identity, which survives rotation).
	Path   string `json:"path"`
	Offset int64  `json:"offset"`
	// HeadLen and HeadSHA fingerprint the first HeadLen bytes (≤ Offset).
	HeadLen int64  `json:"head_len,omitempty"`
	HeadSHA string `json:"head_sha,omitempty"`
	// Size is the file size at the last scan, for lag reporting.
	Size int64 `json:"size"`
	// Missing counts consecutive scans that did not find the file. It is
	// dropped after two, which tolerates a scan racing a rotation.
	Missing int `json:"missing,omitempty"`
}

// State is the persisted checkpoint set.
type State struct {
	Version int `json:"version"`
	// LogPath is the runtime log these checkpoints belong to. A different
	// log path starts over as a first run.
	LogPath string `json:"log_path"`
	// Initialized is set once the first scan has placed every file that
	// existed then (at its end, or at 0 with --backfill). After that an
	// unknown file is new data and is read from 0.
	Initialized bool                   `json:"initialized"`
	Files       map[string]*Checkpoint `json:"files"`
	UpdatedAt   time.Time              `json:"updated_at"`
}

func loadState(dir, logPath string) (*State, error) {
	fresh := &State{Version: stateVersion, LogPath: logPath, Files: map[string]*Checkpoint{}}
	b, err := os.ReadFile(filepath.Join(dir, CheckpointsFile))
	if errors.Is(err, os.ErrNotExist) {
		return fresh, nil
	}
	if err != nil {
		return nil, err
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("parse %s: %w (delete it to start over from the end of the log)", filepath.Join(dir, CheckpointsFile), err)
	}
	if s.Version != stateVersion || s.LogPath != logPath {
		return fresh, nil
	}
	if s.Files == nil {
		s.Files = map[string]*Checkpoint{}
	}
	return &s, nil
}

func saveState(dir string, s *State, now time.Time) error {
	s.UpdatedAt = now.UTC()
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return config.WriteFileAtomic(filepath.Join(dir, CheckpointsFile), append(b, '\n'), 0o600)
}

// FileLag is how far one retained file is behind brainsrv.
type FileLag struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	Offset int64  `json:"offset"`
	Behind int64  `json:"behind"`
}

// Status is the forwarder's status file, read by `afferent status`.
type Status struct {
	PID          int       `json:"pid"`
	StartedAt    time.Time `json:"started_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	State        string    `json:"state"` // running, paused, backoff, stopped
	PausedReason string    `json:"paused_reason,omitempty"`
	LogPath      string    `json:"log_path"`
	BrainsrvURL  string    `json:"brainsrv_url"`
	Context      string    `json:"context"`
	Scope        string    `json:"scope"`

	LastSuccessAt time.Time `json:"last_success_at,omitempty"`
	LastError     string    `json:"last_error,omitempty"`
	LastErrorAt   time.Time `json:"last_error_at,omitempty"`
	NextRetryAt   time.Time `json:"next_retry_at,omitempty"`

	// Totals since the state directory was created.
	Batches      int64 `json:"batches_sent"`
	LinesSent    int64 `json:"lines_sent"`
	BytesSent    int64 `json:"bytes_sent"` // uncompressed NDJSON
	Accepted     int64 `json:"accepted"`
	Duplicate    int64 `json:"duplicate"`
	Rejected     int64 `json:"rejected"`
	LinesSkipped int64 `json:"lines_skipped"`

	Lag      []FileLag `json:"lag,omitempty"`
	LagBytes int64     `json:"lag_bytes"`
	LogFound bool      `json:"log_found"`
}

// Forwarder states.
const (
	StateRunning = "running"
	StatePaused  = "paused"
	StateBackoff = "backoff"
	StateStopped = "stopped"
)

// Paused reasons.
const (
	ReasonLoginRequired = "login required"
	ReasonScopeDenied   = "scope denied"
	ReasonUnauthorized  = "unauthorized"
)

// ReadStatus reads the status file in dir. A missing file returns
// os.ErrNotExist.
func ReadStatus(dir string) (*Status, error) {
	b, err := os.ReadFile(filepath.Join(dir, StatusFile))
	if err != nil {
		return nil, err
	}
	var s Status
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("parse %s: %w", filepath.Join(dir, StatusFile), err)
	}
	return &s, nil
}

func writeStatus(dir string, s *Status) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return config.WriteFileAtomic(filepath.Join(dir, StatusFile), append(b, '\n'), 0o600)
}

// ReadCheckpoints returns the persisted checkpoints in dir (nil when there
// are none yet).
func ReadCheckpoints(dir string) (*State, error) {
	b, err := os.ReadFile(filepath.Join(dir, CheckpointsFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func sortLag(l []FileLag) {
	sort.Slice(l, func(i, j int) bool { return l[i].Path < l[j].Path })
}
