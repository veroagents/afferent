package cmd

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/endpoint/brainsrv"
)

func seedBrainsrvState(t *testing.T) {
	t.Helper()
	if err := os.MkdirAll(brainsrv.DataDir(true), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{brainsrv.SecretsPath(true), brainsrv.ConnectionPath(true), brainsrv.VectorConfigPath(true)} {
		if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestEndpointUninstallAlsoRemovesTheBrainsrvForwarder(t *testing.T) {
	fwd, _ := isolateBrainsrvCmd(t)
	seedBrainsrvState(t)
	upstreamRan := false
	run := wrapUninstallWithBrainsrv(func(*cobra.Command, []string) error { upstreamRan = true; return errors.New("upstream failed") })
	c, _ := brainsrvTestCommand()
	err := run(c, nil)
	if !upstreamRan || err == nil || !strings.Contains(err.Error(), "upstream failed") {
		t.Fatalf("upstream uninstall must still run and report: ran=%t err=%v", upstreamRan, err)
	}
	if got := strings.Join(fwd.calls, ","); got != "unload,remove" {
		t.Fatalf("brainsrv forwarder calls = %s", got)
	}
	if _, err := os.Stat(brainsrv.Dir(true)); !os.IsNotExist(err) {
		t.Fatal("uninstall must remove the brainsrv state dir, including the stored key")
	}
	if endpointUninstallCmd.RunE == nil || !strings.Contains(endpointUninstallCmd.Long, "brainsrv") {
		t.Fatal("endpoint uninstall is not wrapped")
	}
}

func TestEndpointUninstallKeepConfigKeepsBrainsrvStateAndDryRunTouchesNothing(t *testing.T) {
	fwd, _ := isolateBrainsrvCmd(t)
	seedBrainsrvState(t)
	run := wrapUninstallWithBrainsrv(func(*cobra.Command, []string) error { return nil })
	c, _ := brainsrvTestCommand()

	endpointOpts.dryRun = true
	if err := run(c, nil); err != nil || len(fwd.calls) != 0 {
		t.Fatalf("dry run: err=%v calls=%v", err, fwd.calls)
	}
	endpointOpts.dryRun = false
	endpointOpts.keepConfig = true
	if err := run(c, nil); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(fwd.calls, ","); got != "unload,remove" {
		t.Fatalf("calls = %s", got)
	}
	if _, err := os.Stat(brainsrv.SecretsPath(true)); err != nil {
		t.Fatal("--keep-config must keep the brainsrv key and config")
	}
}
