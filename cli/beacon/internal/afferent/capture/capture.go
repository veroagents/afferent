// Package capture installs Beacon's activity capture for coding agents, for
// `afferent setup` (PLAN D7). It uses Beacon's own hook installers by import
// (internal/endpoint/hooks: the code behind `beacon endpoint hooks install
// --harness X`, which `beacon endpoint install --harness X` also runs for
// these agents).
//
// Only the hooks are installed. Beacon's OTLP path (`beacon endpoint
// install` also starts an OpenTelemetry collector service) needs a collector
// binary that afferent does not ship; the hooks alone write every prompt,
// tool call and session event to the runtime log that `afferent forward`
// ships. The hook binary is the one embedded in this build.
package capture

import (
	"fmt"

	endpointhooks "github.com/asymptote-labs/agent-beacon/cli/beacon/internal/endpoint/hooks"
)

// Harnesses capture can be installed for.
var Harnesses = []string{"claude", "codex", "cursor"}

// Installer installs and inspects capture for one harness at a time.
type Installer interface {
	// Status reports whether capture is installed for h and which settings
	// file holds it.
	Status(h string) (installed bool, path string, err error)
	// Install installs capture for h, writing events to logPath.
	Install(h, logPath string, userMode bool) (path string, err error)
}

// Hooks is the real Installer: Beacon's endpoint hooks, user level.
type Hooks struct{}

// Status implements Installer.
func (Hooks) Status(h string) (bool, string, error) {
	switch h {
	case "claude":
		s := endpointhooks.ClaudeHookStatus(endpointhooks.ClaudeOptions{UserMode: true})
		return s.Installed, s.SettingsPath, nil
	case "codex":
		s := endpointhooks.CodexHookStatus(endpointhooks.CodexOptions{UserMode: true})
		return s.Installed, s.HooksPath, nil
	case "cursor":
		s := endpointhooks.CursorHookStatus(endpointhooks.CursorOptions{UserMode: true})
		return s.Installed, s.HooksJSONPath, nil
	}
	return false, "", fmt.Errorf("unsupported harness %q", h)
}

// Install implements Installer.
func (Hooks) Install(h, logPath string, userMode bool) (string, error) {
	switch h {
	case "claude":
		s, err := endpointhooks.InstallClaude(endpointhooks.ClaudeOptions{LogPath: logPath, UserMode: userMode})
		return s.SettingsPath, err
	case "codex":
		s, err := endpointhooks.InstallCodex(endpointhooks.CodexOptions{LogPath: logPath, UserMode: userMode})
		return s.HooksPath, err
	case "cursor":
		s, err := endpointhooks.InstallCursor(endpointhooks.CursorOptions{LogPath: logPath, UserMode: userMode})
		return s.HooksJSONPath, err
	}
	return "", fmt.Errorf("unsupported harness %q", h)
}
