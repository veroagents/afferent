// Package service installs `afferent forward` as a per-user background
// service: a launchd LaunchAgent on macOS, a systemd --user unit on Linux.
//
// Unit generation is pure (Plist, SystemdUnit). Loading, unloading and
// status go through a Loader, and the real loaders run launchctl or systemctl
// through a Runner, so tests use fakes and never touch the service manager.
package service

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/afferent/config"
)

// Names of the service.
const (
	Label       = "com.veroagents.afferent.forwarder"
	SystemdUnit = "afferent-forwarder.service"
	Description = "afferent forwarder: Beacon runtime log to brainsrv"
)

// Kind is the service manager.
type Kind string

const (
	KindLaunchd Kind = "launchd"
	KindSystemd Kind = "systemd"
)

// Detect returns the service manager for this OS, or "" if there is none.
func Detect() Kind {
	switch runtime.GOOS {
	case "darwin":
		return KindLaunchd
	case "linux":
		return KindSystemd
	}
	return ""
}

// Spec is what the service runs.
type Spec struct {
	Program string   // absolute path of the afferent binary
	Args    []string // e.g. forward --config-dir …
	// LogPath receives stdout and stderr on launchd. systemd uses the
	// journal.
	LogPath string
	// Env is extra environment for the job.
	Env map[string]string
}

// State is what the service manager reports.
type State struct {
	Loaded  bool
	Running bool
	PID     int
	Detail  string
}

// Loader starts, stops and inspects the job.
type Loader interface {
	Load(unitPath string) error
	Unload(unitPath string) error
	Status() (State, error)
}

// Manager installs the forwarder service for the current user.
type Manager struct {
	Kind Kind
	// Home is the user's home directory; unit files go under it.
	Home   string
	Loader Loader
}

// Default returns the manager for this OS, using the real launchctl or
// systemctl.
func Default() (Manager, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Manager{}, err
	}
	k := Detect()
	m := Manager{Kind: k, Home: home}
	switch k {
	case KindLaunchd:
		m.Loader = Launchctl{Domain: "gui/" + strconv.Itoa(os.Getuid()), Label: Label, Run: ExecRunner}
	case KindSystemd:
		m.Loader = Systemctl{Unit: SystemdUnit, Run: ExecRunner}
	default:
		return m, fmt.Errorf("afferent can install its forwarder as a service on macOS (launchd) and Linux (systemd) only; run `afferent forward` yourself on %s", runtime.GOOS)
	}
	return m, nil
}

// UnitPath is where the plist or unit file goes.
func (m Manager) UnitPath() string {
	if m.Kind == KindSystemd {
		return filepath.Join(m.Home, ".config", "systemd", "user", SystemdUnit)
	}
	return filepath.Join(m.Home, "Library", "LaunchAgents", Label+".plist")
}

// Name is the label or unit name.
func (m Manager) Name() string {
	if m.Kind == KindSystemd {
		return SystemdUnit
	}
	return Label
}

// Render returns the unit file content for spec.
func (m Manager) Render(spec Spec) (string, error) {
	switch m.Kind {
	case KindLaunchd:
		return Plist(spec), nil
	case KindSystemd:
		return SystemdUnitFile(spec), nil
	}
	return "", fmt.Errorf("no service manager on this system")
}

// Install writes the unit file (0644) and (re)loads the job. The log
// directory is created 0700.
func (m Manager) Install(spec Spec) (string, error) {
	if m.Loader == nil {
		return "", errors.New("no service loader")
	}
	if !filepath.IsAbs(spec.Program) {
		return "", fmt.Errorf("the service needs an absolute program path, got %q", spec.Program)
	}
	content, err := m.Render(spec)
	if err != nil {
		return "", err
	}
	if spec.LogPath != "" {
		if err := config.EnsureDir(filepath.Dir(spec.LogPath)); err != nil {
			return "", err
		}
	}
	path := m.UnitPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := config.WriteFileAtomic(path, []byte(content), 0o644); err != nil {
		return "", err
	}
	if err := m.Loader.Load(path); err != nil {
		return path, fmt.Errorf("start %s: %w", m.Name(), err)
	}
	return path, nil
}

// Uninstall stops the job and removes the unit file. Neither being absent is
// an error.
func (m Manager) Uninstall() error {
	path := m.UnitPath()
	var uerr error
	if m.Loader != nil {
		uerr = m.Loader.Unload(path)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if uerr != nil {
		return fmt.Errorf("stop %s: %w", m.Name(), uerr)
	}
	return nil
}

// Status is the installed and running state.
type Status struct {
	Kind      Kind
	Name      string
	UnitPath  string
	Installed bool
	State     State
	Err       error
}

// Status reports whether the unit is installed and what the manager says.
func (m Manager) Status() Status {
	s := Status{Kind: m.Kind, Name: m.Name(), UnitPath: m.UnitPath()}
	if _, err := os.Stat(s.UnitPath); err == nil {
		s.Installed = true
	}
	if m.Loader != nil {
		s.State, s.Err = m.Loader.Status()
	}
	return s
}

// Plist renders the LaunchAgent: resident, restarted on exit (throttled to
// 10 s), logging to spec.LogPath.
func Plist(spec Spec) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>` + xmlEscape(Label) + `</string>
  <key>ProgramArguments</key>
  <array>
`)
	for _, a := range append([]string{spec.Program}, spec.Args...) {
		b.WriteString("    <string>" + xmlEscape(a) + "</string>\n")
	}
	b.WriteString("  </array>\n")
	if len(spec.Env) > 0 {
		b.WriteString("  <key>EnvironmentVariables</key>\n  <dict>\n")
		for _, k := range sortedKeys(spec.Env) {
			b.WriteString("    <key>" + xmlEscape(k) + "</key>\n    <string>" + xmlEscape(spec.Env[k]) + "</string>\n")
		}
		b.WriteString("  </dict>\n")
	}
	b.WriteString(`  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>ThrottleInterval</key>
  <integer>10</integer>
  <key>ProcessType</key>
  <string>Background</string>
`)
	if spec.LogPath != "" {
		b.WriteString("  <key>StandardOutPath</key>\n  <string>" + xmlEscape(spec.LogPath) + "</string>\n")
		b.WriteString("  <key>StandardErrorPath</key>\n  <string>" + xmlEscape(spec.LogPath) + "</string>\n")
	}
	b.WriteString("</dict>\n</plist>\n")
	return b.String()
}

// SystemdUnitFile renders the systemd --user unit.
func SystemdUnitFile(spec Spec) string {
	var b strings.Builder
	b.WriteString("[Unit]\nDescription=" + Description + "\nAfter=network-online.target\nWants=network-online.target\n\n[Service]\nType=simple\n")
	argv := append([]string{spec.Program}, spec.Args...)
	q := make([]string, len(argv))
	for i, a := range argv {
		q[i] = systemdQuote(a)
	}
	b.WriteString("ExecStart=" + strings.Join(q, " ") + "\n")
	for _, k := range sortedKeys(spec.Env) {
		b.WriteString("Environment=" + systemdQuoteEnv(k+"="+spec.Env[k]) + "\n")
	}
	b.WriteString("Restart=always\nRestartSec=10\n\n[Install]\nWantedBy=default.target\n")
	return b.String()
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;").Replace(s)
}

// systemdQuote double-quotes a word for ExecStart/Environment: backslash and
// quote are escaped, % (specifiers) and $ (variable expansion) doubled.
func systemdQuote(s string) string {
	s = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%", "$", "$$", "\n", `\n`).Replace(s)
	return `"` + s + `"`
}

// systemdQuoteEnv quotes an Environment= assignment. systemd expands
// specifiers there but not variables, so $ stays as is.
func systemdQuoteEnv(s string) string {
	s = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%", "\n", `\n`).Replace(s)
	return `"` + s + `"`
}

func sortedKeys(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// Runner runs a command and returns its combined output.
type Runner func(name string, args ...string) (string, error)

// ExecRunner runs the real command.
func ExecRunner(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return string(out), err
}

// Launchctl loads the LaunchAgent into the user's GUI domain.
type Launchctl struct {
	Domain string // gui/<uid>
	Label  string
	Run    Runner
}

func (l Launchctl) target() string { return l.Domain + "/" + l.Label }

// Load boots out any previous instance, then bootstraps the plist.
func (l Launchctl) Load(unitPath string) error {
	_, _ = l.Run("launchctl", "bootout", l.target())
	if out, err := l.Run("launchctl", "bootstrap", l.Domain, unitPath); err != nil {
		return fmt.Errorf("launchctl bootstrap %s %s: %v: %s", l.Domain, unitPath, err, strings.TrimSpace(out))
	}
	return nil
}

// Unload boots the job out; a job that is not loaded is fine.
func (l Launchctl) Unload(string) error {
	out, err := l.Run("launchctl", "bootout", l.target())
	if err != nil && !launchctlNotLoaded(out) {
		return fmt.Errorf("launchctl bootout %s: %v: %s", l.target(), err, strings.TrimSpace(out))
	}
	return nil
}

func launchctlNotLoaded(out string) bool {
	o := strings.ToLower(out)
	return strings.Contains(o, "no such process") || strings.Contains(o, "could not find service") || strings.Contains(o, "not loaded")
}

// Status parses `launchctl print`.
func (l Launchctl) Status() (State, error) {
	out, err := l.Run("launchctl", "print", l.target())
	if err != nil {
		return State{Detail: "not loaded"}, nil
	}
	s := State{Loaded: true}
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), " = ")
		if !ok {
			continue
		}
		switch k {
		case "state":
			if s.Detail == "" {
				s.Detail = v
				s.Running = v == "running"
			}
		case "pid":
			if s.PID == 0 {
				s.PID, _ = strconv.Atoi(v)
			}
		}
	}
	return s, nil
}

// Systemctl manages the unit with `systemctl --user`.
type Systemctl struct {
	Unit string
	Run  Runner
}

func (s Systemctl) run(args ...string) (string, error) {
	return s.Run("systemctl", append([]string{"--user"}, args...)...)
}

// Load reloads unit files, enables the unit and (re)starts it.
func (s Systemctl) Load(string) error {
	for _, args := range [][]string{{"daemon-reload"}, {"enable", s.Unit}, {"restart", s.Unit}} {
		if out, err := s.run(args...); err != nil {
			return fmt.Errorf("systemctl --user %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(out))
		}
	}
	return nil
}

// Unload disables and stops the unit; a unit that does not exist is fine.
func (s Systemctl) Unload(string) error {
	out, err := s.run("disable", "--now", s.Unit)
	if err != nil && !strings.Contains(strings.ToLower(out), "not loaded") && !strings.Contains(strings.ToLower(out), "does not exist") {
		return fmt.Errorf("systemctl --user disable --now %s: %v: %s", s.Unit, err, strings.TrimSpace(out))
	}
	_, _ = s.run("daemon-reload")
	return nil
}

// Status parses `systemctl --user show`.
func (s Systemctl) Status() (State, error) {
	out, err := s.run("show", "-p", "LoadState", "-p", "ActiveState", "-p", "SubState", "-p", "MainPID", s.Unit)
	if err != nil {
		return State{}, fmt.Errorf("systemctl --user show %s: %v: %s", s.Unit, err, strings.TrimSpace(out))
	}
	st := State{}
	var active, sub string
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch k {
		case "LoadState":
			st.Loaded = v == "loaded"
		case "ActiveState":
			active = v
		case "SubState":
			sub = v
		case "MainPID":
			st.PID, _ = strconv.Atoi(v)
		}
	}
	st.Running = active == "active" && sub == "running"
	st.Detail = strings.Trim(active+"/"+sub, "/")
	return st, nil
}
