package brainsrv

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/endpoint/service"
)

// VectorLogName is the file the launchd job appends Vector's stdout and stderr to, inside the
// 0700 state directory. It is where Vector records batches brainsrv rejected with a 4xx
// ("not retriable; dropping the request").
const VectorLogName = "vector.log"

// VectorLogPath locates Vector's log for a launchd forwarder.
func VectorLogPath(userMode bool) string { return filepath.Join(Dir(userMode), VectorLogName) }

// ForwarderManager is the upstream service.ForwarderManager with one change: on launchd the
// job logs into the forwarder's own 0700 state directory rather than upstream's predictable
// /tmp/<label>.{out,err}. A root LaunchDaemon opening a /tmp path another local user could
// have pre-created as a symlink would append to whatever it points at, and /tmp is cleared at
// reboot, taking the record of dropped batches with it. Load, Unload and Status are
// upstream's, keyed by the plist path and label, so the rewritten plist loads the same way.
type ForwarderManager struct {
	service.ForwarderManager
}

func (m ForwarderManager) kind() service.Kind {
	if m.Kind != service.KindAuto {
		return m.Kind
	}
	return service.DetectKind()
}

// WriteUnit writes the launchd plist itself and defers to upstream for systemd, which already
// logs to the journal.
func (m ForwarderManager) WriteUnit(vectorBin, configPath string) (string, error) {
	if m.kind() != service.KindLaunchd {
		return m.ForwarderManager.WriteUnit(vectorBin, configPath)
	}
	if !m.Supported() {
		return "", fmt.Errorf("%s", m.UnsupportedReason())
	}
	path, err := m.UnitPath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := ensureDir(m.UserMode); err != nil {
		return "", err
	}
	content := forwarderPlist(m.Label(), vectorBin, configPath, VectorLogPath(m.UserMode))
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// VectorLogHint says where to read Vector's own log for this forwarder.
func VectorLogHint(userMode bool, kind service.Kind) string {
	if kind == service.KindSystemd {
		if userMode {
			return "journalctl --user -u " + SystemdUnit
		}
		return "journalctl -u " + SystemdUnit
	}
	return VectorLogPath(userMode)
}

// forwarderPlist matches upstream's forwarder job (resident, KeepAlive, 10 s throttle) with
// the log paths moved.
func forwarderPlist(label, vectorBin, configPath, logPath string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>%s</string>
  <key>ProgramArguments</key>
  <array>
    <string>%s</string>
    <string>--config</string>
    <string>%s</string>
  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>ThrottleInterval</key>
  <integer>10</integer>
  <key>StandardOutPath</key>
  <string>%s</string>
  <key>StandardErrorPath</key>
  <string>%s</string>
</dict>
</plist>
`, xmlEscape(label), xmlEscape(vectorBin), xmlEscape(configPath), xmlEscape(logPath), xmlEscape(logPath))
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;").Replace(s)
}

// stopPollInterval and DefaultStopTimeout bound waitStopped. launchd's bootout returns while
// Vector is still draining (up to a minute), so the wait is longer than that.
var stopPollInterval = 500 * time.Millisecond

const DefaultStopTimeout = 90 * time.Second

// stopForwarder unloads the forwarder and waits until the service manager no longer reports
// it loaded or running, so nothing can write checkpoints or the buffer afterwards. An Unload
// error or a job that is still there after timeout is an error.
func stopForwarder(manager Forwarder, timeout time.Duration) error {
	if err := manager.Unload(); err != nil {
		return fmt.Errorf("could not stop the forwarder: %w", err)
	}
	if timeout <= 0 {
		timeout = DefaultStopTimeout
	}
	deadline := time.Now().Add(timeout)
	for {
		status := manager.Status()
		if !status.Loaded && !status.Running {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the forwarder %s is still running %s after it was stopped; check `launchctl print` or `systemctl status`", manager.Label(), timeout)
		}
		time.Sleep(stopPollInterval)
	}
}
