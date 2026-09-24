package service

import (
	"encoding/xml"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files")

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Fatalf("%s differs (go test -update to accept):\n--- got\n%s\n--- want\n%s", name, got, want)
	}
}

func spec() Spec {
	return Spec{
		Program: "/opt/homebrew/bin/afferent",
		Args:    []string{"forward", "--config-dir", "/Users/dev/.config/afferent"},
		LogPath: "/Users/dev/.config/afferent/state/forwarder.log",
		Env:     map[string]string{"AFFERENT_STATE_DIR": "/Users/dev/state & more $X"},
	}
}

func TestPlistGolden(t *testing.T) {
	got := Plist(spec())
	golden(t, "forwarder.plist", got)
	d := xml.NewDecoder(strings.NewReader(got))
	for {
		if _, err := d.Token(); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("plist is not well-formed XML: %v", err)
		}
	}
}

func TestSystemdUnitGolden(t *testing.T) {
	s := spec()
	s.Program = "/home/dev/bin/afferent"
	s.Args = []string{"forward", "--config-dir", "/home/dev/.config/afferent 100%", "--x=$HOME"}
	golden(t, "afferent-forwarder.service", SystemdUnitFile(s))
}

type fakeLoader struct {
	calls   []string
	loadErr error
	state   State
}

func (f *fakeLoader) Load(p string) error   { f.calls = append(f.calls, "load "+p); return f.loadErr }
func (f *fakeLoader) Unload(p string) error { f.calls = append(f.calls, "unload "+p); return nil }
func (f *fakeLoader) Status() (State, error) {
	f.calls = append(f.calls, "status")
	return f.state, nil
}

func TestInstallUninstallWithFakeLoader(t *testing.T) {
	for _, kind := range []Kind{KindLaunchd, KindSystemd} {
		t.Run(string(kind), func(t *testing.T) {
			home := t.TempDir()
			fl := &fakeLoader{state: State{Loaded: true, Running: true, PID: 42}}
			m := Manager{Kind: kind, Home: home, Loader: fl}
			sp := spec()
			sp.LogPath = filepath.Join(home, ".config", "afferent", "state", "forwarder.log")
			path, err := m.Install(sp)
			if err != nil {
				t.Fatal(err)
			}
			wantPath := filepath.Join(home, "Library", "LaunchAgents", "com.veroagents.afferent.forwarder.plist")
			if kind == KindSystemd {
				wantPath = filepath.Join(home, ".config", "systemd", "user", "afferent-forwarder.service")
			}
			if path != wantPath {
				t.Fatalf("unit at %s, want %s", path, wantPath)
			}
			fi, err := os.Stat(path)
			if err != nil || fi.Mode().Perm() != 0o644 {
				t.Fatalf("unit file %v %v", err, fi)
			}
			if di, err := os.Stat(filepath.Dir(sp.LogPath)); err != nil || di.Mode().Perm() != 0o700 {
				t.Fatalf("log dir %v %v", err, di)
			}
			want, _ := m.Render(sp)
			if got, _ := os.ReadFile(path); string(got) != want {
				t.Fatal("unit content differs from Render")
			}
			st := m.Status()
			if !st.Installed || !st.State.Running || st.State.PID != 42 {
				t.Fatalf("status %+v", st)
			}
			if err := m.Uninstall(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("unit file left behind")
			}
			if strings.Join(fl.calls, ";") != "load "+path+";status;unload "+path {
				t.Fatalf("calls %v", fl.calls)
			}
			// Uninstalling again is fine.
			if err := m.Uninstall(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestInstallRefusesRelativeProgram(t *testing.T) {
	m := Manager{Kind: KindLaunchd, Home: t.TempDir(), Loader: &fakeLoader{}}
	if _, err := m.Install(Spec{Program: "afferent"}); err == nil {
		t.Fatal("relative program accepted")
	}
}

// The real loaders are exercised with a recording Runner: no launchctl or
// systemctl ever runs.
type recRunner struct {
	calls []string
	out   map[string]string
	fail  map[string]bool
}

func (r *recRunner) run(name string, args ...string) (string, error) {
	c := name + " " + strings.Join(args, " ")
	r.calls = append(r.calls, c)
	if r.fail[c] {
		return r.out[c], errors.New("exit status 1")
	}
	return r.out[c], nil
}

func TestLaunchctlCommands(t *testing.T) {
	r := &recRunner{out: map[string]string{
		"launchctl print gui/501/" + Label:   "gui/501/com.veroagents.afferent.forwarder = {\n\tstate = running\n\tpid = 777\n\tsubsystem = {\n\t\tstate = waiting\n\t}\n}",
		"launchctl bootout gui/501/" + Label: "Boot-out failed: 3: No such process",
	}, fail: map[string]bool{"launchctl bootout gui/501/" + Label: true}}
	l := Launchctl{Domain: "gui/501", Label: Label, Run: r.run}
	if err := l.Load("/p.plist"); err != nil {
		t.Fatal(err)
	}
	if err := l.Unload("/p.plist"); err != nil {
		t.Fatalf("unload of a job that is not loaded: %v", err)
	}
	st, err := l.Status()
	if err != nil || !st.Loaded || !st.Running || st.PID != 777 {
		t.Fatalf("status %+v %v", st, err)
	}
	want := []string{
		"launchctl bootout gui/501/" + Label,
		"launchctl bootstrap gui/501 /p.plist",
		"launchctl bootout gui/501/" + Label,
		"launchctl print gui/501/" + Label,
	}
	if strings.Join(r.calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls\n%s", strings.Join(r.calls, "\n"))
	}
	r.fail["launchctl print gui/501/"+Label] = true
	if st, _ := l.Status(); st.Loaded {
		t.Fatal("failed print reported loaded")
	}
}

func TestSystemctlCommands(t *testing.T) {
	r := &recRunner{out: map[string]string{
		"systemctl --user show -p LoadState -p ActiveState -p SubState -p MainPID " + SystemdUnit: "LoadState=loaded\nActiveState=active\nSubState=running\nMainPID=99\n",
	}}
	s := Systemctl{Unit: SystemdUnit, Run: r.run}
	if err := s.Load("/u"); err != nil {
		t.Fatal(err)
	}
	if err := s.Unload("/u"); err != nil {
		t.Fatal(err)
	}
	st, err := s.Status()
	if err != nil || !st.Loaded || !st.Running || st.PID != 99 || st.Detail != "active/running" {
		t.Fatalf("status %+v %v", st, err)
	}
	want := []string{
		"systemctl --user daemon-reload",
		"systemctl --user enable " + SystemdUnit,
		"systemctl --user restart " + SystemdUnit,
		"systemctl --user disable --now " + SystemdUnit,
		"systemctl --user daemon-reload",
		"systemctl --user show -p LoadState -p ActiveState -p SubState -p MainPID " + SystemdUnit,
	}
	if strings.Join(r.calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls\n%s", strings.Join(r.calls, "\n"))
	}
}
