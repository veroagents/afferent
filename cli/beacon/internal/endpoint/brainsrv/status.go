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
	// Write is the result of an empty write probe on the scope ("ok", "unauthorized",
	// "forbidden", "unknown", or "" when skipped): the read health route alone cannot show a
	// key that lost its write grant, and Vector drops every batch brainsrv answers with a 4xx.
	Write        string `json:"write,omitempty"`
	WriteMessage string `json:"write_message,omitempty"`
	// VectorLog is where Vector records dropped batches: a file for launchd, a journalctl
	// command for systemd.
	VectorLog string `json:"vector_log,omitempty"`
	// Warnings flag telemetry that is probably not arriving.
	Warnings []string `json:"warnings,omitempty"`
	Message  string   `json:"message,omitempty"`
}

// DeliveryGrace is how long after a line is written brainsrv is expected to have it: the
// sink's 60 s batch timeout plus a 30 s request and retries.
const DeliveryGrace = 3 * time.Minute

// StatusOptions tunes Status; zero values are the production defaults.
type StatusOptions struct {
	HTTPClient *http.Client
	// SkipHealthCheck avoids the network call (tests, offline diagnostics).
	SkipHealthCheck bool
	// Forwarder overrides the service manager; nil uses NewForwarderManager.
	Forwarder Forwarder
	// Hostname picks this machine's row out of the endpoint list; empty uses os.Hostname.
	Hostname string
	// Now is injectable for tests.
	Now func() time.Time
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
	kind := service.Kind(status.Forwarder.Kind)
	if fm, ok := manager.(ForwarderManager); ok && kind == "" {
		kind = fm.kind()
	}
	status.VectorLog = VectorLogHint(userMode, kind)
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
	client := Client{BaseURL: connection.URL, Key: key, HTTP: opts.HTTPClient}
	health, err := client.Health(ctx, connection.Scope)
	status.Health, status.HealthMessage = classifyStatus(err)
	if err == nil {
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
	status.Write, status.WriteMessage = classifyStatus(client.ProbeWrite(ctx, connection.Scope))
	if status.Write != "ok" && status.Write != "unknown" {
		status.Warnings = append(status.Warnings, fmt.Sprintf("brainsrv refuses writes on %s (%s): Vector drops every batch it rejects with a 4xx, see %s", connection.Scope, status.WriteMessage, status.VectorLog))
	}
	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}
	if warning := undeliveredWarning(connection, status.LastSeen, now(), status.VectorLog); status.Health == "ok" && warning != "" {
		status.Warnings = append(status.Warnings, warning)
	}
	return status
}

func classifyStatus(err error) (string, string) {
	switch {
	case err == nil:
		return "ok", ""
	case errors.Is(err, ErrUnauthorized):
		return "unauthorized", err.Error()
	case errors.Is(err, ErrForbidden):
		return "forbidden", err.Error()
	default:
		return "unknown", err.Error()
	}
}

// undeliveredWarning flags a runtime log written well after brainsrv last heard from this
// host, once that write is old enough that the batch should have arrived. Vector's checkpoint
// tracks reading, not delivery, so a batch brainsrv rejected (403 on a derived scope, 413,
// 400) leaves no other trace here.
func undeliveredWarning(connection *Connection, lastSeen *time.Time, now time.Time, vectorLog string) string {
	info, err := os.Stat(connection.LogPath)
	if err != nil {
		return ""
	}
	written := info.ModTime()
	reference := connection.ConnectedAt
	if lastSeen != nil && lastSeen.After(reference) {
		reference = *lastSeen
	}
	if written.Sub(reference) <= DeliveryGrace || now.Sub(written) <= DeliveryGrace {
		return ""
	}
	seen := "never"
	if lastSeen != nil {
		seen = lastSeen.UTC().Format(time.RFC3339)
	}
	return fmt.Sprintf("the runtime log was written at %s but brainsrv last heard from this host %s: batches may have been rejected and dropped; see %s, then reconnect with --backfill to re-send", written.UTC().Format(time.RFC3339), seen, vectorLog)
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
