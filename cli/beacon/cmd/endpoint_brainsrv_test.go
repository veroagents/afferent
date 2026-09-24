package cmd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/endpoint/brainsrv"
	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/endpoint/service"
	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/testenv"
)

// brainsrvCmdForwarder is a service-manager fake so no command test reaches launchctl or
// systemctl.
type brainsrvCmdForwarder struct{ calls []string }

func (f *brainsrvCmdForwarder) Supported() bool           { return true }
func (f *brainsrvCmdForwarder) UnsupportedReason() string { return "" }
func (f *brainsrvCmdForwarder) Label() string             { return brainsrv.LaunchdLabel }
func (f *brainsrvCmdForwarder) UnitPath() (string, error) { return "", nil }
func (f *brainsrvCmdForwarder) WriteUnit(string, string) (string, error) {
	f.calls = append(f.calls, "write")
	return "/fake/unit", nil
}
func (f *brainsrvCmdForwarder) Load() error   { f.calls = append(f.calls, "load"); return nil }
func (f *brainsrvCmdForwarder) Unload() error { f.calls = append(f.calls, "unload"); return nil }
func (f *brainsrvCmdForwarder) RemoveUnits()  { f.calls = append(f.calls, "remove") }
func (f *brainsrvCmdForwarder) Status() service.Status {
	return service.Status{Label: brainsrv.LaunchdLabel, Loaded: true, Running: true}
}

func isolateBrainsrvCmd(t *testing.T) (*brainsrvCmdForwarder, string) {
	t.Helper()
	home := t.TempDir()
	testenv.SetHome(t, home)
	t.Setenv("BEACON_VECTOR_BIN", "")
	oldOpts, oldEndpoint, oldForwarder := brainsrvOpts, endpointOpts, brainsrvForwarder
	fwd := &brainsrvCmdForwarder{}
	brainsrvForwarder = func(bool) brainsrv.Forwarder { return fwd }
	t.Cleanup(func() { brainsrvOpts, endpointOpts, brainsrvForwarder = oldOpts, oldEndpoint, oldForwarder })
	endpointOpts.userMode, endpointOpts.systemMode, endpointOpts.jsonOutput = true, false, false
	endpointOpts.logPath = filepath.Join(home, "runtime.jsonl")
	return fwd, home
}

func brainsrvTestCommand() (*cobra.Command, *bytes.Buffer) {
	var buf bytes.Buffer
	c := &cobra.Command{}
	c.SetOut(&buf)
	c.SetErr(&buf)
	return c, &buf
}

func brainsrvFakeVector(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake vector script needs a POSIX shell")
	}
	path := filepath.Join(t.TempDir(), "vector")
	script := "#!/bin/sh\ncase \"$1\" in\n  --version) echo \"vector 0.56.0 (test)\";;\n  *) exit 0;;\nesac\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestEndpointBrainsrvCommandsRegistered(t *testing.T) {
	for _, name := range []string{"connect", "status", "disconnect", "print-config", "install-pack", "validate"} {
		c, _, err := rootCmd.Find([]string{"endpoint", "brainsrv", name})
		if err != nil || c == nil || c.Name() != name {
			t.Fatalf("endpoint brainsrv %s not registered: %v", name, err)
		}
	}
	for _, flag := range []string{"url", "scope", "key-file", "backfill", "user", "vector-bin"} {
		if endpointBrainsrvConnectCmd.Flags().Lookup(flag) == nil {
			t.Fatalf("connect --%s missing", flag)
		}
	}
	if endpointBrainsrvDisconnectCmd.Flags().Lookup("purge") == nil || endpointBrainsrvInstallPackCmd.Flags().Lookup("output") == nil {
		t.Fatal("disconnect --purge / install-pack --output missing")
	}
}

func TestEndpointBrainsrvConnectSurfacesKeyFileRejection(t *testing.T) {
	if !testenv.HasPOSIXFileModes() {
		t.Skip("POSIX file modes only")
	}
	fwd, _ := isolateBrainsrvCmd(t)
	hits := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
	t.Cleanup(server.Close)
	key := filepath.Join(t.TempDir(), "beacon.key")
	if err := os.WriteFile(key, []byte("spk_cmdtest\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(key, 0o644); err != nil {
		t.Fatal(err)
	}
	brainsrvOpts.url, brainsrvOpts.scope, brainsrvOpts.keyFile = server.URL, "ws.dev.people.m.harness", key
	c, _ := brainsrvTestCommand()
	err := runEndpointBrainsrvConnect(c, nil)
	if err == nil || !strings.Contains(err.Error(), "--key-file") || !strings.Contains(err.Error(), "permissions") {
		t.Fatalf("err = %v", err)
	}
	if hits != 0 || len(fwd.calls) != 0 {
		t.Fatalf("rejected key file reached the network (%d) or the service manager (%v)", hits, fwd.calls)
	}
}

func TestEndpointBrainsrvConnectStatusDisconnect(t *testing.T) {
	fwd, _ := isolateBrainsrvCmd(t)
	health := http.StatusOK
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer spk_cmdtest" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path == brainsrv.HealthPath {
			w.WriteHeader(health)
			_, _ = w.Write([]byte(`{"ok":true,"endpoints":[]}`))
			return
		}
		_, _ = w.Write([]byte(`{"accepted":0}`))
	}))
	t.Cleanup(server.Close)
	key := filepath.Join(t.TempDir(), "beacon.key")
	if err := os.WriteFile(key, []byte("spk_cmdtest\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	brainsrvOpts.url, brainsrvOpts.scope, brainsrvOpts.keyFile = server.URL, "ws.dev.people.m.harness", key
	brainsrvOpts.vectorBin = brainsrvFakeVector(t)

	health = http.StatusForbidden
	c, _ := brainsrvTestCommand()
	if err := runEndpointBrainsrvConnect(c, nil); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("connect on 403: %v", err)
	}
	health = http.StatusOK
	c, out := brainsrvTestCommand()
	if err := runEndpointBrainsrvConnect(c, nil); err != nil {
		t.Fatalf("connect: %v\n%s", err, out)
	}
	if !strings.Contains(out.String(), "ws.dev.people.m.harness") || strings.Join(fwd.calls, ",") != "write,load" {
		t.Fatalf("connect output %q calls %v", out, fwd.calls)
	}

	c, out = brainsrvTestCommand()
	endpointOpts.jsonOutput = true
	if err := runEndpointBrainsrvStatus(c, nil); err != nil {
		t.Fatal(err)
	}
	var status brainsrv.ForwarderStatus
	if err := json.Unmarshal(out.Bytes(), &status); err != nil || !status.Connected || status.Health != "ok" {
		t.Fatalf("status = %+v %v\n%s", status, err, out)
	}
	endpointOpts.jsonOutput = false

	c, out = brainsrvTestCommand()
	if err := runEndpointBrainsrvPrintConfig(c, nil); err != nil || !strings.Contains(out.String(), `X-Scope = "ws.dev.people.m.harness"`) {
		t.Fatalf("print-config: %v\n%s", err, out)
	}

	c, out = brainsrvTestCommand()
	if err := runEndpointBrainsrvDisconnect(c, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(brainsrv.SecretsPath(true)); !os.IsNotExist(err) {
		t.Fatal("disconnect left the key behind")
	}
	if !strings.Contains(out.String(), "revoke") {
		t.Fatalf("disconnect should point at server-side revocation: %s", out)
	}
}

func TestEndpointBrainsrvPrintConfigAndInstallPackWithoutConnection(t *testing.T) {
	isolateBrainsrvCmd(t)
	c, out := brainsrvTestCommand()
	if err := runEndpointBrainsrvPrintConfig(c, nil); err != nil || !strings.Contains(out.String(), "${BEACON_BRAINSRV_URL}") {
		t.Fatalf("print-config template: %v\n%s", err, out)
	}
	brainsrvOpts.packOutput = filepath.Join(t.TempDir(), "pack")
	c, _ = brainsrvTestCommand()
	if err := runEndpointBrainsrvInstallPack(c, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(brainsrvOpts.packOutput, "vector.toml")); err != nil {
		t.Fatal(err)
	}
	c, out = brainsrvTestCommand()
	if err := runEndpointBrainsrvStatus(c, nil); err != nil || !strings.Contains(out.String(), "not connected") {
		t.Fatalf("status: %v %s", err, out)
	}
}
