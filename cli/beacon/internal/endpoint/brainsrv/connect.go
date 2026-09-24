package brainsrv

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/brainsrvcfg"
	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/endpoint/asymptote"
	endpointconfig "github.com/asymptote-labs/agent-beacon/cli/beacon/internal/endpoint/config"
	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/endpoint/service"
)

// Forwarder is the service-manager surface Connect, Disconnect and Status need; it is the same
// seam the asymptote forwarder uses. NewForwarderManager is the production implementation;
// tests supply a fake so no launchctl or systemctl runs.
type Forwarder = asymptote.Forwarder

// NewForwarderManager returns the service manager for the brainsrv forwarder: the upstream
// ForwarderManager with this forwarder's own launchd label, systemd unit and description, so
// it never touches the Beacon Managed forwarder's service.
func NewForwarderManager(userMode bool) ForwarderManager {
	return ForwarderManager{service.ForwarderManager{
		UserMode:     userMode,
		LaunchdLabel: LaunchdLabel,
		SystemdUnit:  SystemdUnit,
		Description:  ServiceDescription,
	}}
}

// ConnectOptions drives Connect.
type ConnectOptions struct {
	UserMode bool
	// URL is the brainsrv base URL; Scope the member's base scope; KeyFile the 0600 file
	// holding the member's own spk_ key.
	URL     string
	Scope   string
	KeyFile string
	// Backfill ships the existing runtime log (read_from = "beginning") and clears this
	// forwarder's file checkpoints so it is re-read once. A later connect without it renders
	// read_from = "end" again and keeps the checkpoints.
	Backfill bool
	// LogPath is the runtime log Vector tails.
	LogPath string
	// VectorBin overrides Vector discovery.
	VectorBin string
	// Forwarder overrides the service manager; nil uses NewForwarderManager.
	Forwarder Forwarder
	// StopTimeout bounds the wait for a running forwarder to exit before its checkpoints or
	// buffer are cleared; zero uses DefaultStopTimeout.
	StopTimeout time.Duration
	// HTTPClient is used for the grant check; nil uses a default with HealthTimeout.
	HTTPClient *http.Client
	Out        io.Writer
	// Now is injectable for tests.
	Now func() time.Time
}

// ConnectResult reports what Connect wrote and where telemetry now goes.
type ConnectResult struct {
	Connection     Connection     `json:"connection"`
	VectorConfig   string         `json:"vector_config"`
	SecretsFile    string         `json:"secrets_file"`
	DataDir        string         `json:"data_dir"`
	UnitPath       string         `json:"unit_path"`
	Forwarder      string         `json:"forwarder"`
	Reconnected    bool           `json:"reconnected"`
	ForwarderState service.Status `json:"forwarder_state"`
}

// Connect checks the member's key against brainsrv and starts the forwarder.
//
// Ordering: everything that can refuse without side effects runs first (URL and scope
// validation, the key file's ownership and mode, the brainsrv grant check for read and write on
// the scope, locating Vector, and a preflight validate of the template in a scratch dir). Only
// then is the key written (0600, atomically), the real config rendered and validated from a
// temp file, and only then written and pointed at by the service unit. The running forwarder
// is stopped (and waited for) only when its checkpoints or buffer must be cleared, after the
// new config and unit are in place. The connection record is saved last, so status never
// claims a connection that did not finish; any failure before that restores what was there.
func Connect(ctx context.Context, opts ConnectOptions) (_ *ConnectResult, err error) {
	out := opts.Out
	if out == nil {
		out = io.Discard
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	base, err := brainsrvcfg.ValidateURL(opts.URL)
	if err != nil {
		return nil, fmt.Errorf("--url: %w", err)
	}
	if !brainsrvcfg.ValidScope(opts.Scope) {
		return nil, fmt.Errorf("--scope %q is not a valid brainsrv scope (lowercase [a-z0-9_] labels joined by dots)", opts.Scope)
	}
	key, err := brainsrvcfg.ReadKeyFile(opts.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("--key-file: %w", err)
	}

	// PLAN §1.2: refuse a scope the key cannot read and write before writing any config.
	client := Client{BaseURL: base, Key: key, HTTP: opts.HTTPClient}
	if _, err := client.Health(ctx, opts.Scope); err != nil {
		return nil, fmt.Errorf("refusing to connect: %w", err)
	}
	if err := client.ProbeWrite(ctx, opts.Scope); err != nil {
		return nil, fmt.Errorf("refusing to connect: %w", err)
	}
	fmt.Fprintf(out, "brainsrv accepted key %s… for read and write on %s\n", keyPrefix(key), opts.Scope)

	manager := opts.Forwarder
	if manager == nil {
		manager = NewForwarderManager(opts.UserMode)
	}
	if !manager.Supported() {
		return nil, errors.New(manager.UnsupportedReason())
	}
	vector, err := asymptote.FindVector(opts.VectorBin)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(out, "Using Vector %s at %s\n", vector.Version, vector.Path)

	logPath := opts.LogPath
	if logPath == "" {
		if cfg, loadErr := endpointconfig.Load(opts.UserMode); loadErr == nil {
			logPath = cfg.LogPath
		}
	}
	if logPath == "" {
		return nil, errors.New("no runtime log path: pass --log-path or install the endpoint first")
	}
	previous, err := LoadConnection(opts.UserMode)
	if err != nil && !errors.Is(err, ErrNotConnected) {
		return nil, err
	}
	reconnect := previous != nil

	if err := ensureDir(opts.UserMode); err != nil {
		return nil, err
	}
	renderOpts := RenderOptions{LogPath: logPath, URL: base, Scope: opts.Scope, Backfill: opts.Backfill}
	if err := preflightVectorConfig(vector.Path, Dir(opts.UserMode), renderOpts); err != nil {
		return nil, err
	}

	// From here on every change is undone if a later step fails: a first connect leaves
	// nothing installed, a reconnect puts back the previous key, config, checkpoints and
	// buffer and restarts the previous forwarder.
	tx := newConnectTx(opts.UserMode, manager, reconnect)
	defer func() {
		if err != nil {
			err = tx.rollback(err)
		}
	}()

	// Secrets first, then the config that references them, then the unit that runs it.
	if err := checkStorableKey(key); err != nil {
		return nil, err
	}
	if err := tx.writeFile(SecretsPath(opts.UserMode), []byte(SecretsFileContent(key)), 0o600); err != nil {
		return nil, fmt.Errorf("could not store the brainsrv key: %w", err)
	}
	dataDir := DataDir(opts.UserMode)
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	renderOpts.SecretsFile = SecretsPath(opts.UserMode)
	renderOpts.DataDir = dataDir
	rendered, err := RenderVectorConfig(renderOpts)
	if err != nil {
		return nil, err
	}
	configPath := VectorConfigPath(opts.UserMode)
	if err := preValidateVectorConfig(vector.Path, configPath, []byte(rendered)); err != nil {
		return nil, err
	}
	// Vector runs without --watch-config, so rewriting the config and unit does not disturb a
	// running forwarder; nothing is stopped until both are in place.
	if err := tx.writeFile(configPath, []byte(rendered), 0o644); err != nil {
		return nil, err
	}
	tx.forwarderTouched = true
	unitPath, err := manager.WriteUnit(vector.Path, configPath)
	if err != nil {
		return nil, err
	}

	// The disk buffer and checkpoints belong to one (url, scope). Lines buffered for another
	// brainsrv or scope must never be posted here with this key, so a changed or unknown
	// destination starts from an empty data dir.
	destination := Destination{URL: base, Scope: opts.Scope}
	resetData := dataDirDestinationMismatch(opts.UserMode, destination)
	if resetData || opts.Backfill {
		// Vector must be fully gone first: launchd's bootout returns while it is still draining
		// and it would write its checkpoints back on exit.
		tx.stopped = true
		if err := stopForwarder(manager, opts.StopTimeout); err != nil {
			return nil, err
		}
		if resetData {
			if err := tx.moveAside(dataDir); err != nil {
				return nil, fmt.Errorf("could not clear the data dir of the previous brainsrv destination: %w", err)
			}
			if err := os.MkdirAll(dataDir, 0o700); err != nil {
				return nil, err
			}
			fmt.Fprintln(out, "Cleared the Vector data dir: its checkpoints and buffer belonged to a different brainsrv URL or scope")
		} else {
			// read_from only applies to a file with no checkpoint, so a backfill drops this
			// forwarder's checkpoints. The disk buffer is left alone: lines already buffered
			// for this destination are still delivered.
			if err := tx.moveAside(CheckpointDir(opts.UserMode)); err != nil {
				return nil, fmt.Errorf("could not clear checkpoints for --backfill: %w", err)
			}
		}
	}
	if err := tx.writeFile(DestinationPath(opts.UserMode), destination.encode(), 0o600); err != nil {
		return nil, err
	}
	tx.loaded = true
	if err := manager.Load(); err != nil {
		return nil, fmt.Errorf("forwarder installed at %s but could not be started: %w", unitPath, err)
	}

	connection := Connection{
		URL:           base,
		Scope:         opts.Scope,
		KeyPrefix:     keyPrefix(key),
		LogPath:       logPath,
		ConnectedAt:   now().UTC(),
		Backfill:      opts.Backfill,
		VectorBin:     vector.Path,
		VectorVersion: vector.Version,
	}
	if err := tx.saveConnection(connection); err != nil {
		return nil, err
	}
	tx.commit()
	return &ConnectResult{
		Connection:     connection,
		VectorConfig:   configPath,
		SecretsFile:    SecretsPath(opts.UserMode),
		DataDir:        dataDir,
		UnitPath:       unitPath,
		Forwarder:      manager.Label(),
		Reconnected:    reconnect,
		ForwarderState: manager.Status(),
	}, nil
}

// DisconnectOptions drives Disconnect.
type DisconnectOptions struct {
	UserMode bool
	// Forwarder overrides the service manager; nil uses NewForwarderManager.
	Forwarder Forwarder
	// Purge also removes the Vector data dir (checkpoints and undelivered disk buffer).
	Purge bool
	// KeepState stops and removes the service but leaves the key, config and connection
	// record (endpoint uninstall --keep-config).
	KeepState bool
}

// Disconnect stops and removes the forwarder's service and deletes the connection record,
// secrets file and rendered config. The data dir stays unless Purge is set. Nothing is revoked
// server-side: revoke the key in brainsrv.
func Disconnect(opts DisconnectOptions) error {
	manager := opts.Forwarder
	if manager == nil {
		manager = NewForwarderManager(opts.UserMode)
	}
	var problems []error
	// Only touch the service manager when this forwarder was installed here, so a disconnect on
	// a machine that never connected issues no bootout at all.
	if forwarderInstalled(opts.UserMode, manager) {
		if err := manager.Unload(); err != nil {
			problems = append(problems, fmt.Errorf("stop forwarder: %w", err))
		}
		manager.RemoveUnits()
	}
	if opts.KeepState {
		return errors.Join(problems...)
	}
	if err := RemoveState(opts.UserMode, opts.Purge); err != nil {
		problems = append(problems, fmt.Errorf("remove %s: %w", Dir(opts.UserMode), err))
	}
	return errors.Join(problems...)
}

// forwarderInstalled reports whether Connect got as far as writing state or a unit.
func forwarderInstalled(userMode bool, manager Forwarder) bool {
	for _, path := range []string{ConnectionPath(userMode), VectorConfigPath(userMode), SecretsPath(userMode)} {
		if _, err := os.Stat(path); err == nil {
			return true
		}
	}
	if path, err := manager.UnitPath(); err == nil && path != "" {
		if _, err := os.Stat(path); err == nil {
			return true
		}
	}
	return false
}

// preflightIngestURL stands in for brainsrv on the template preflight; validate never
// connects, and .invalid (RFC 2606) resolves to nothing.
const preflightIngestURL = "https://brainsrv.invalid"

// preflightVectorConfig proves Vector accepts the rendered template before any credential or
// config is written. Everything it points at lives in a scratch dir under stateDir: a data
// dir (Vector refuses a missing one) and a placeholder secrets file.
func preflightVectorConfig(vectorBin, stateDir string, opts RenderOptions) error {
	scratch, err := os.MkdirTemp(stateDir, ".vector-preflight-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(scratch)
	opts.URL = preflightIngestURL
	opts.DataDir = filepath.Join(scratch, DataDirName)
	if err := os.Mkdir(opts.DataDir, 0o700); err != nil {
		return err
	}
	opts.SecretsFile = filepath.Join(scratch, SecretsFileName)
	if err := os.WriteFile(opts.SecretsFile, []byte(SecretsFileContent("spk_preflight")), 0o600); err != nil {
		return err
	}
	rendered, err := RenderVectorConfig(opts)
	if err != nil {
		return err
	}
	configPath := filepath.Join(scratch, VectorConfigName)
	if err := os.WriteFile(configPath, []byte(rendered), 0o600); err != nil {
		return err
	}
	return asymptote.ValidateVectorConfig(vectorBin, configPath)
}

// preValidateVectorConfig validates data from a temp file next to configPath, so a running
// forwarder is never pointed at a config Vector would refuse.
func preValidateVectorConfig(vectorBin, configPath string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(configPath), ".vector-validate-*.toml")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return asymptote.ValidateVectorConfig(vectorBin, tmpName)
}

// Validate renders the config for the stored connection (or, when not connected, the template
// against placeholders) and runs `vector validate --skip-healthchecks` on it.
func Validate(userMode bool, vectorBin, logPath string) (asymptote.VectorInfo, string, error) {
	vector, err := asymptote.FindVector(vectorBin)
	if err != nil {
		return asymptote.VectorInfo{}, "", err
	}
	if _, err := os.Stat(VectorConfigPath(userMode)); err == nil {
		return vector, VectorConfigPath(userMode), asymptote.ValidateVectorConfig(vector.Path, VectorConfigPath(userMode))
	}
	if logPath == "" {
		logPath = DefaultLogPath
	}
	scratch, err := os.MkdirTemp("", "afferent-brainsrv-validate-*")
	if err != nil {
		return vector, "", err
	}
	defer os.RemoveAll(scratch)
	return vector, "template", preflightVectorConfig(vector.Path, scratch, RenderOptions{LogPath: logPath, Scope: "preflight"})
}
