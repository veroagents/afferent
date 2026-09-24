package brainsrv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/endpoint/service"
)

// Checkpoint is one file-source checkpoint Vector recorded: how far into a tailed file it has
// read. Position is a byte offset.
type Checkpoint struct {
	Position int64  `json:"position"`
	Modified string `json:"modified,omitempty"`
}

// ForwarderStatus is what `beacon endpoint brainsrv status` reports.
type ForwarderStatus struct {
	Connected   bool           `json:"connected"`
	URL         string         `json:"url,omitempty"`
	Scope       string         `json:"scope,omitempty"`
	KeyPrefix   string         `json:"key_prefix,omitempty"`
	LogPath     string         `json:"log_path,omitempty"`
	ConnectedAt string         `json:"connected_at,omitempty"`
	VectorBin   string         `json:"vector_bin,omitempty"`
	Forwarder   service.Status `json:"forwarder"`
	// Checkpoints are read from Vector's checkpoint file for the runtime source;
	// CheckpointMessage says why there are none (not written yet, or unreadable).
	Checkpoints       []Checkpoint `json:"checkpoints,omitempty"`
	CheckpointMessage string       `json:"checkpoint_message,omitempty"`
	DataDirBytes      int64        `json:"data_dir_bytes"`
	// Health is "ok", "unauthorized", "forbidden", "unknown" (unreachable) or "" (skipped).
	Health        string     `json:"health,omitempty"`
	HealthMessage string     `json:"health_message,omitempty"`
	Endpoints     []Endpoint `json:"endpoints,omitempty"`
	// LastSeen is brainsrv's last_seen for this machine's hostname, when listed.
	LastSeen *time.Time `json:"last_seen,omitempty"`
	Message  string     `json:"message,omitempty"`
}

// StatusOptions tunes Status; zero values are the production defaults.
type StatusOptions struct {
	HTTPClient *http.Client
	// SkipHealthCheck avoids the network call (tests, offline diagnostics).
	SkipHealthCheck bool
	// Forwarder overrides the service manager; nil uses NewForwarderManager.
	Forwarder Forwarder
	// Hostname picks this machine's row out of the endpoint list; empty uses os.Hostname.
	Hostname string
}

// Status describes the brainsrv forwarder on this endpoint. The stored key is used for one
// GET and never copied into the result.
func Status(ctx context.Context, userMode bool, opts StatusOptions) ForwarderStatus {
	connection, err := LoadConnection(userMode)
	if err != nil {
		status := ForwarderStatus{}
		if !errors.Is(err, ErrNotConnected) {
			status.Message = err.Error()
		}
		return status
	}
	manager := opts.Forwarder
	if manager == nil {
		manager = NewForwarderManager(userMode)
	}
	status := ForwarderStatus{
		Connected:    true,
		URL:          connection.URL,
		Scope:        connection.Scope,
		KeyPrefix:    connection.KeyPrefix,
		LogPath:      connection.LogPath,
		ConnectedAt:  connection.ConnectedAt.UTC().Format(time.RFC3339),
		VectorBin:    connection.VectorBin,
		Forwarder:    manager.Status(),
		DataDirBytes: dirSize(DataDir(userMode)),
	}
	status.Checkpoints, err = ReadCheckpoints(userMode)
	switch {
	case errors.Is(err, os.ErrNotExist):
		status.CheckpointMessage = "no checkpoint yet (Vector has not read the log)"
	case err != nil:
		status.CheckpointMessage = err.Error()
	}
	if opts.SkipHealthCheck {
		return status
	}
	key, err := ReadStoredKey(userMode)
	if err != nil {
		status.Health, status.HealthMessage = "unknown", err.Error()
		return status
	}
	health, err := Client{BaseURL: connection.URL, Key: key, HTTP: opts.HTTPClient}.Health(ctx, connection.Scope)
	switch {
	case errors.Is(err, ErrUnauthorized):
		status.Health, status.HealthMessage = "unauthorized", err.Error()
	case errors.Is(err, ErrForbidden):
		status.Health, status.HealthMessage = "forbidden", err.Error()
	case err != nil:
		status.Health, status.HealthMessage = "unknown", err.Error()
	default:
		status.Health = "ok"
		status.Endpoints = health.Endpoints
		hostname := opts.Hostname
		if hostname == "" {
			hostname, _ = os.Hostname()
		}
		for _, ep := range health.Endpoints {
			if ep.Hostname == hostname {
				seen := ep.LastSeen
				status.LastSeen = &seen
				break
			}
		}
	}
	return status
}

// ReadCheckpoints parses Vector's file-source checkpoint file for the runtime source
// (<data_dir>/<SourceID>/checkpoints.json). It returns an error wrapping os.ErrNotExist when
// Vector has not written one yet.
func ReadCheckpoints(userMode bool) ([]Checkpoint, error) {
	path := filepath.Join(CheckpointDir(userMode), "checkpoints.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Checkpoints []Checkpoint `json:"checkpoints"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("checkpoint file %s is not readable: %w", path, err)
	}
	return doc.Checkpoints, nil
}

// dirSize sums regular files under root.
func dirSize(root string) int64 {
	var total int64
	_ = filepath.WalkDir(root, func(_ string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}
