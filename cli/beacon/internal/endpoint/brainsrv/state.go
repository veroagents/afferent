package brainsrv

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/brainsrvcfg"
	endpointconfig "github.com/asymptote-labs/agent-beacon/cli/beacon/internal/endpoint/config"
)

// Files under <BaseDir>/afferent-brainsrv/. The directory is 0700; the secrets file and the
// connection record are 0600; vector.toml is 0644 because it holds no secret, only the
// SECRET[] reference. None of these names or the directory is shared with the asymptote
// forwarder (<BaseDir>/asymptote/).
const (
	DirName            = "afferent-brainsrv"
	ConnectionFileName = "connection.json"
	SecretsFileName    = "vector-secrets.json"
	VectorConfigName   = "vector.toml"
	DataDirName        = "vector-data"
	// DestinationFileName records which (url, scope) the data dir's checkpoints and disk
	// buffer belong to. It survives a plain disconnect, like the data dir.
	DestinationFileName = "data-destination.json"
)

// Dir is the forwarder's state directory for the selected endpoint mode.
func Dir(userMode bool) string { return filepath.Join(endpointconfig.BaseDir(userMode), DirName) }

// ConnectionPath, SecretsPath, VectorConfigPath and DataDir locate the individual files.
func ConnectionPath(userMode bool) string   { return filepath.Join(Dir(userMode), ConnectionFileName) }
func SecretsPath(userMode bool) string      { return filepath.Join(Dir(userMode), SecretsFileName) }
func VectorConfigPath(userMode bool) string { return filepath.Join(Dir(userMode), VectorConfigName) }
func DataDir(userMode bool) string          { return filepath.Join(Dir(userMode), DataDirName) }

// DestinationPath locates the data dir's destination record.
func DestinationPath(userMode bool) string { return filepath.Join(Dir(userMode), DestinationFileName) }

// Destination is where the lines in the data dir's buffer were meant to go.
type Destination struct {
	URL   string `json:"url"`
	Scope string `json:"scope"`
}

func (d Destination) encode() []byte {
	data, _ := json.MarshalIndent(d, "", "  ")
	return append(data, '\n')
}

// dataDirDestinationMismatch reports whether the data dir holds anything (checkpoints or a
// buffer) recorded for a destination other than want, or for an unknown one.
func dataDirDestinationMismatch(userMode bool, want Destination) bool {
	entries, err := os.ReadDir(DataDir(userMode))
	if err != nil || len(entries) == 0 {
		return false
	}
	data, err := os.ReadFile(DestinationPath(userMode))
	if err != nil {
		return true
	}
	var got Destination
	if json.Unmarshal(data, &got) != nil {
		return true
	}
	return got != want
}

// CheckpointDir is where Vector keeps the file source's checkpoints inside DataDir.
func CheckpointDir(userMode bool) string { return filepath.Join(DataDir(userMode), SourceID) }

// StatePaths lists every path this forwarder owns, for the coexistence check.
func StatePaths(userMode bool) []string {
	return []string{Dir(userMode), ConnectionPath(userMode), SecretsPath(userMode), VectorConfigPath(userMode), DataDir(userMode)}
}

// Connection is the non-secret record of a finished connect. The key itself is only in the
// secrets file; KeyPrefix is enough to tell keys apart.
type Connection struct {
	URL           string    `json:"url"`
	Scope         string    `json:"scope"`
	KeyPrefix     string    `json:"key_prefix"`
	LogPath       string    `json:"log_path"`
	ConnectedAt   time.Time `json:"connected_at"`
	Backfill      bool      `json:"backfill,omitempty"`
	VectorBin     string    `json:"vector_bin,omitempty"`
	VectorVersion string    `json:"vector_version,omitempty"`
}

// ErrNotConnected is returned when no connection record exists.
var ErrNotConnected = errors.New("this endpoint is not forwarding to brainsrv")

// LoadConnection reads the connection record.
func LoadConnection(userMode bool) (*Connection, error) {
	data, err := os.ReadFile(ConnectionPath(userMode))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotConnected
		}
		return nil, err
	}
	var c Connection
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("connection record %s is not valid JSON: %w", ConnectionPath(userMode), err)
	}
	return &c, nil
}

// SaveConnection writes the record 0600 inside the 0700 state directory.
func SaveConnection(userMode bool, c Connection) error {
	if err := ensureDir(userMode); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(ConnectionPath(userMode), append(data, '\n'), 0o600)
}

// checkStorableKey refuses to put anything but a brainsrv spk_ key in the secrets file.
func checkStorableKey(key string) error {
	if !strings.HasPrefix(key, brainsrvcfg.KeyPrefix) || len(key) == len(brainsrvcfg.KeyPrefix) {
		return errors.New("refusing to store a credential that is not a brainsrv spk_ key")
	}
	return nil
}

// ReadStoredKey returns the key from the secrets file, for status's health check.
func ReadStoredKey(userMode bool) (string, error) {
	data, err := os.ReadFile(SecretsPath(userMode))
	if err != nil {
		if os.IsNotExist(err) {
			return "", ErrNotConnected
		}
		return "", err
	}
	var secrets map[string]string
	if err := json.Unmarshal(data, &secrets); err != nil {
		return "", fmt.Errorf("secrets file %s is not valid JSON: %w", SecretsPath(userMode), err)
	}
	key := secrets[SecretsKey]
	if key == "" {
		return "", fmt.Errorf("secrets file %s has no %s", SecretsPath(userMode), SecretsKey)
	}
	return key, nil
}

// RemoveState deletes the connection record, secrets file and rendered config. The Vector
// data dir (checkpoints and any undelivered disk buffer) stays unless purge is set, so a later
// connect resumes where this one stopped instead of re-reading or losing buffered lines.
func RemoveState(userMode bool, purge bool) error {
	if purge {
		if err := os.RemoveAll(Dir(userMode)); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	for _, path := range []string{SecretsPath(userMode), ConnectionPath(userMode), VectorConfigPath(userMode)} {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// keyPrefix is the part of a key safe to show: spk_ plus up to 8 characters.
func keyPrefix(key string) string {
	n := len(brainsrvcfg.KeyPrefix) + 8
	if len(key) < n {
		n = len(key)
	}
	return key[:n]
}

func ensureDir(userMode bool) error {
	dir := Dir(userMode)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// MkdirAll leaves an existing directory's mode alone; tighten it.
	return os.Chmod(dir, 0o700)
}

// writeFileAtomic writes via a temp file in the same directory so a crash never leaves a
// half-written credential, and sets the mode before the rename so the content is never
// readable with wider permissions.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return err
	}
	return nil
}
