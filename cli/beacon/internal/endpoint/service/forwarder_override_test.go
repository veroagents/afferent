package service

// afferent PLAN B-6: a second forwarder (the brainsrv one) names itself through the optional
// LaunchdLabel/SystemdUnit/Description fields. These tests pin that the zero value is exactly
// the Asymptote forwarder and that an override reaches every place the label or unit is used.

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/testenv"
)

const (
	testOverrideLabel = "com.afferent.brainsrv-forwarder"
	testOverrideUnit  = "afferent-brainsrv-forwarder.service"
)

func TestForwarderZeroOverridesKeepTheAsymptoteNames(t *testing.T) {
	home := t.TempDir()
	testenv.SetHome(t, home)
	for _, kind := range []Kind{KindLaunchd, KindSystemd} {
		plain := ForwarderManager{UserMode: true, Kind: kind}
		for _, m := range []ForwarderManager{plain, {UserMode: true, Kind: kind, LaunchdLabel: "", SystemdUnit: "", Description: ""}} {
			if m.Label() != plain.Label() {
				t.Fatalf("%s: zero overrides changed the label: %q", kind, m.Label())
			}
		}
	}
	if (ForwarderManager{Kind: KindLaunchd}).Label() != ForwarderLabel || (ForwarderManager{Kind: KindSystemd}).Label() != ForwarderSystemdUnit {
		t.Fatal("zero-value labels must be the upstream constants")
	}
	if got := describedForwarderUnitFile("", "/v", "/c", true); got != forwarderUnitFile("/v", "/c", true) {
		t.Fatalf("empty description must render the upstream unit:\n%s", got)
	}
	if !strings.Contains(forwarderUnitFile("/v", "/c", true), "Asymptote managed ingest") {
		t.Fatal("upstream unit description changed")
	}
}

func TestForwarderOverridesReachUnitPathLabelAndUnitFile(t *testing.T) {
	home := t.TempDir()
	testenv.SetHome(t, home)
	cases := []struct {
		kind     Kind
		userMode bool
		want     string
	}{
		{KindLaunchd, true, filepath.Join(home, "Library", "LaunchAgents", testOverrideLabel+".plist")},
		{KindLaunchd, false, filepath.Join("/Library/LaunchDaemons", testOverrideLabel+".plist")},
		{KindSystemd, true, filepath.Join(home, ".config", "systemd", "user", testOverrideUnit)},
		{KindSystemd, false, filepath.Join("/etc/systemd/system", testOverrideUnit)},
	}
	for _, c := range cases {
		m := ForwarderManager{UserMode: c.userMode, Kind: c.kind, LaunchdLabel: testOverrideLabel, SystemdUnit: testOverrideUnit}
		got, err := m.UnitPath()
		if err != nil || got != c.want {
			t.Errorf("%s user=%t: got %q (%v), want %q", c.kind, c.userMode, got, err, c.want)
		}
		upstream, _ := ForwarderManager{UserMode: c.userMode, Kind: c.kind}.UnitPath()
		if upstream == got {
			t.Errorf("%s user=%t: override shares the upstream unit path %q", c.kind, c.userMode, got)
		}
	}
	if (ForwarderManager{Kind: KindLaunchd, LaunchdLabel: testOverrideLabel}).Label() != testOverrideLabel {
		t.Fatal("launchd label override ignored")
	}
	if (ForwarderManager{Kind: KindSystemd, SystemdUnit: testOverrideUnit}).Label() != testOverrideUnit {
		t.Fatal("systemd unit override ignored")
	}
	unit := describedForwarderUnitFile("afferent forwarder to brainsrv", "/usr/bin/vector", "/c.toml", true)
	if !strings.Contains(unit, "Description=afferent forwarder to brainsrv\n") || strings.Contains(unit, "asymptotelabs") {
		t.Fatalf("description override not rendered:\n%s", unit)
	}
}

func TestForwarderLaunchdOverrideDrivesLaunchctl(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("launchd only")
	}
	shrinkLaunchdWaits(t)
	testenv.SetHome(t, t.TempDir())
	var calls []string
	bootstrapped := false
	oldRun := runLaunchctlCommand
	runLaunchctlCommand = func(args ...string) (string, error) {
		calls = append(calls, strings.Join(args, " "))
		switch args[0] {
		case "bootout":
			return "", nil
		case "bootstrap":
			bootstrapped = true
			return "", nil
		case "print":
			if !bootstrapped {
				return "Could not find service", errors.New("exit status 113")
			}
			return "state = running\npid = 1\n", nil
		}
		return "", nil
	}
	t.Cleanup(func() { runLaunchctlCommand = oldRun })

	m := ForwarderManager{UserMode: true, Kind: KindLaunchd, LaunchdLabel: testOverrideLabel}
	path, err := m.WriteUnit("/usr/bin/vector", "/tmp/brainsrv.toml")
	if err != nil {
		t.Fatalf("WriteUnit: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), "<string>"+testOverrideLabel+"</string>") {
		t.Fatalf("plist at %s lacks the override label: %v\n%s", path, err, data)
	}
	if err := m.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if status := m.Status(); status.Label != testOverrideLabel || !status.Running {
		t.Fatalf("status = %+v", status)
	}
	if err := m.Unload(); err != nil {
		t.Fatalf("Unload: %v", err)
	}
	for _, call := range calls {
		if strings.Contains(call, ForwarderLabel) {
			t.Fatalf("launchctl touched the Asymptote forwarder: %q", call)
		}
	}
}
