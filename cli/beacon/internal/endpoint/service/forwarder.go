package service

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	// ForwarderLabel is the launchd label of the Asymptote managed-ingest forwarder. It is a
	// LaunchAgent for a user-mode endpoint and a LaunchDaemon for a system-mode one.
	ForwarderLabel = "com.beacon.endpoint.asymptote-forwarder"
	// ForwarderSystemdUnit is the systemd equivalent, under the user or system scope.
	ForwarderSystemdUnit = "beacon-asymptote-forwarder.service"
)

// ForwarderManager manages the long-running Vector process that ships Beacon JSONL to
// Asymptote. It follows UpdaterManager's shape (unit write/load/unload/status) but is resident
// like the collector rather than scheduled, and unlike the collector it exists in both user and
// system modes, because a per-user Beacon install forwards that user's log.
type ForwarderManager struct {
	UserMode bool
	// Kind selects the backend; the zero value auto-detects.
	Kind Kind
	// LaunchdLabel, SystemdUnit and Description name a second forwarder (afferent's brainsrv
	// forwarder). Zero values keep ForwarderLabel, ForwarderSystemdUnit and the Asymptote
	// description. The label field cannot be called Label: that is the method below.
	LaunchdLabel string
	SystemdUnit  string
	Description  string
}

func (m ForwarderManager) launchdLabel() string {
	if m.LaunchdLabel != "" {
		return m.LaunchdLabel
	}
	return ForwarderLabel
}

func (m ForwarderManager) systemdUnit() string {
	if m.SystemdUnit != "" {
		return m.SystemdUnit
	}
	return ForwarderSystemdUnit
}

func (m ForwarderManager) resolvedKind() Kind {
	if m.Kind != KindAuto {
		return m.Kind
	}
	return DetectKind()
}

// Supported reports whether a supervised forwarder can be installed here.
//
// Supervised mode is deliberately not offered: a forwarder nothing restarts would silently stop
// shipping after the first crash or reboot, which is worse than telling the operator to run the
// pack's vector.toml under their own supervisor.
func (m ForwarderManager) Supported() bool {
	switch m.resolvedKind() {
	case KindLaunchd:
		return runtime.GOOS == "darwin"
	case KindSystemd:
		return systemdIsInit()
	default:
		return false
	}
}

// UnsupportedReason explains why Supported is false.
func (m ForwarderManager) UnsupportedReason() string {
	switch m.resolvedKind() {
	case KindLaunchd:
		return "launchd service management is supported only on macOS"
	case KindSystemd:
		return "systemd is not PID 1 on this host, so the forwarder cannot be installed as a service"
	default:
		return "the Asymptote forwarder needs launchd or systemd to stay running; on this host run Vector yourself with the config from `beacon endpoint asymptote install-pack`"
	}
}

// Label is the service identifier for status output.
func (m ForwarderManager) Label() string {
	if m.resolvedKind() == KindSystemd {
		return m.systemdUnit()
	}
	return m.launchdLabel()
}

// UnitPath returns where the forwarder's service definition lives.
func (m ForwarderManager) UnitPath() (string, error) {
	switch m.resolvedKind() {
	case KindSystemd:
		if m.UserMode {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", err
			}
			return filepath.Join(home, ".config", "systemd", "user", m.systemdUnit()), nil
		}
		return filepath.Join("/etc/systemd/system", m.systemdUnit()), nil
	default:
		if m.UserMode {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", err
			}
			return filepath.Join(home, "Library", "LaunchAgents", m.launchdLabel()+".plist"), nil
		}
		return filepath.Join("/Library/LaunchDaemons", m.launchdLabel()+".plist"), nil
	}
}

// RemoveUnits deletes the service definition. Missing files are not an error.
func (m ForwarderManager) RemoveUnits() {
	if path, err := m.UnitPath(); err == nil {
		_ = os.Remove(path)
	}
}

// WriteUnit installs a service definition that runs `vectorBin --config configPath`.
func (m ForwarderManager) WriteUnit(vectorBin, configPath string) (string, error) {
	if !m.Supported() {
		return "", fmt.Errorf("%s", m.UnsupportedReason())
	}
	path, err := m.UnitPath()
	if err != nil {
		return "", err
	}
	if err := ensureDir(path); err != nil {
		return "", err
	}
	var content string
	if m.resolvedKind() == KindSystemd {
		content = describedForwarderUnitFile(m.Description, vectorBin, configPath, m.UserMode)
	} else {
		content = forwarderPlist(m.launchdLabel(), vectorBin, configPath)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "", err
	}
	if m.resolvedKind() == KindSystemd {
		if out, err := runSystemctlCommand(systemctlArgs(m.UserMode, "daemon-reload")...); err != nil {
			return path, systemctlError(out, err, "daemon-reload")
		}
	}
	return path, nil
}

// Load registers and starts the forwarder. An already-loaded launchd job is re-bootstrapped so a
// rewritten config takes effect.
func (m ForwarderManager) Load() error {
	if !m.Supported() {
		return fmt.Errorf("%s", m.UnsupportedReason())
	}
	if m.resolvedKind() == KindSystemd {
		if out, err := runSystemctlCommand(systemctlArgs(m.UserMode, "enable", "--now", m.systemdUnit())...); err != nil {
			return systemctlError(out, err, "enable --now "+m.systemdUnit())
		}
		if out, err := runSystemctlCommand(systemctlArgs(m.UserMode, "restart", m.systemdUnit())...); err != nil {
			return systemctlError(out, err, "restart "+m.systemdUnit())
		}
		return nil
	}
	path, err := m.UnitPath()
	if err != nil {
		return err
	}
	domain := serviceDomain(m.UserMode)
	target := domain + "/" + m.launchdLabel()
	// bootout first so a config rewrite (re-enrollment rotates the key) restarts Vector, then
	// wait for the old instance to be gone: it drains in-flight requests for up to a minute,
	// and a bootstrap issued while it is still registered is silently lost with it. Finally
	// confirm the new instance is actually running rather than trusting the old pid.
	_ = runLaunchctlWithContext(domain, m.launchdLabel(), "", "bootout", target)
	if !waitForLaunchdJobGone(domain, m.launchdLabel()) {
		return fmt.Errorf("the previous forwarder did not stop within %s; check `launchctl print %s`", launchdStopTimeout, target)
	}
	if err := loadLaunchdJob(domain, m.launchdLabel(), path); err != nil {
		return err
	}
	if !waitForLaunchdJobRunning(domain, m.launchdLabel()) {
		return fmt.Errorf("the forwarder was loaded but has not started within %s; check `launchctl print %s` and Vector's log", launchdStartTimeout, target)
	}
	return nil
}

// Unload stops and deregisters the forwarder. A missing unit is not an error.
func (m ForwarderManager) Unload() error {
	switch m.resolvedKind() {
	case KindSystemd:
		if !systemdIsInit() {
			return nil
		}
		if out, err := runSystemctlCommand(systemctlArgs(m.UserMode, "stop", m.systemdUnit())...); err != nil && !systemdUnitMissing(out) {
			return systemctlError(out, err, "stop "+m.systemdUnit())
		}
		if out, err := runSystemctlCommand(systemctlArgs(m.UserMode, "disable", m.systemdUnit())...); err != nil && !systemdUnitMissing(out) {
			return systemctlError(out, err, "disable "+m.systemdUnit())
		}
		return nil
	case KindLaunchd:
		if runtime.GOOS != "darwin" {
			return nil
		}
		return bootoutLaunchdJob(serviceDomain(m.UserMode), m.launchdLabel())
	default:
		return nil
	}
}

// Status reports whether the forwarder is loaded and running.
func (m ForwarderManager) Status() Status {
	kind := m.resolvedKind()
	if !m.Supported() {
		return Status{Label: m.Label(), Kind: string(kind), Message: m.UnsupportedReason()}
	}
	if kind == KindSystemd {
		status := Status{Label: m.systemdUnit(), Kind: string(KindSystemd)}
		enabledOut, _ := runSystemctlCommand(systemctlArgs(m.UserMode, "is-enabled", m.systemdUnit())...)
		switch strings.TrimSpace(enabledOut) {
		case "enabled", "enabled-runtime", "static":
			status.Loaded = true
		}
		activeOut, _ := runSystemctlCommand(systemctlArgs(m.UserMode, "is-active", m.systemdUnit())...)
		status.Running = strings.TrimSpace(activeOut) == "active"
		if status.Running {
			status.Loaded = true
		}
		if !status.Loaded && !status.Running {
			status.Message = "forwarder not installed"
		}
		return status
	}
	status := Status{Label: m.launchdLabel(), Kind: string(KindLaunchd)}
	out, err := runLaunchctlCommand("print", serviceDomain(m.UserMode)+"/"+m.launchdLabel())
	if err != nil {
		status.Message = strings.TrimSpace(out)
		return status
	}
	status.Loaded = true
	status.Running = strings.Contains(out, "state = running") || strings.Contains(out, "pid =")
	return status
}

// forwarderUnitFile renders the systemd unit. Vector reads its own config, so the unit needs no
// environment; the rendered vector.toml carries literal paths and URLs.
func forwarderUnitFile(vectorBin, configPath string, userMode bool) string {
	return describedForwarderUnitFile("", vectorBin, configPath, userMode)
}

// describedForwarderUnitFile is forwarderUnitFile with an optional Description; a non-empty one
// also drops the Asymptote Documentation link.
func describedForwarderUnitFile(description, vectorBin, configPath string, userMode bool) string {
	var b strings.Builder
	b.WriteString("[Unit]\n")
	if description != "" {
		fmt.Fprintf(&b, "Description=%s\n", description)
	} else {
		b.WriteString("Description=Beacon endpoint forwarder to Asymptote managed ingest\n")
		b.WriteString("Documentation=https://docs.asymptotelabs.ai/cli/endpoint-connect\n")
	}
	b.WriteString("After=network-online.target\n")
	b.WriteString("Wants=network-online.target\n")
	b.WriteString("\n[Service]\n")
	b.WriteString("Type=simple\n")
	fmt.Fprintf(&b, "ExecStart=%s --config %s\n", systemdArg(vectorBin), systemdArg(configPath))
	b.WriteString("Restart=always\n")
	b.WriteString("RestartSec=5\n")
	b.WriteString("StandardOutput=journal\n")
	b.WriteString("StandardError=journal\n")
	if !userMode {
		// System mode reads /var/log/beacon-agent, which root owns.
		b.WriteString("User=root\n")
	}
	b.WriteString("\n[Install]\n")
	if userMode {
		b.WriteString("WantedBy=default.target\n")
	} else {
		b.WriteString("WantedBy=multi-user.target\n")
	}
	return b.String()
}

// forwarderPlist renders the launchd job: resident, restarted by launchd, started at load.
func forwarderPlist(label, vectorBin, configPath string) string {
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
  <string>/tmp/%s.out</string>
  <key>StandardErrorPath</key>
  <string>/tmp/%s.err</string>
</dict>
</plist>
`, xmlEscape(label), xmlEscape(vectorBin), xmlEscape(configPath), xmlEscape(label), xmlEscape(label))
}

func xmlEscape(s string) string {
	replacer := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return replacer.Replace(s)
}
