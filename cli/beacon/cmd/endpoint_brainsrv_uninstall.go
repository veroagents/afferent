package cmd

// endpoint_brainsrv_uninstall.go makes `beacon endpoint uninstall` also remove the brainsrv
// forwarder. Upstream's lifecycle.Uninstall stops only the Asymptote and package forwarders;
// without this the brainsrv launchd job or systemd unit (KeepAlive/Restart=always) and its
// 0600 spk_ secrets file would survive an uninstall that reports nothing left behind. It wraps
// endpointUninstallCmd's RunE from this file's init(), so upstream's cmd files need no edit
// (PLAN B-5).

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/endpoint/brainsrv"
)

func init() {
	endpointUninstallCmd.RunE = wrapUninstallWithBrainsrv(endpointUninstallCmd.RunE)
	endpointUninstallCmd.Long = `Remove local endpoint service files.

This also stops and removes the afferent brainsrv forwarder, if one is connected:
its service, stored spk_ key, config and Vector data dir. With --keep-config only
its service is removed and the key and config are kept for a later
` + "`beacon endpoint brainsrv connect`" + `. The key is not revoked in brainsrv.`
}

// wrapUninstallWithBrainsrv runs the brainsrv teardown before upstream's uninstall, and runs
// upstream's even when the teardown fails, reporting both.
func wrapUninstallWithBrainsrv(upstream func(*cobra.Command, []string) error) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		if endpointOpts.dryRun {
			return upstream(cmd, args)
		}
		userMode := endpointUserMode()
		brainsrvErr := brainsrv.Disconnect(brainsrv.DisconnectOptions{
			UserMode:  userMode,
			Forwarder: brainsrvForwarder(userMode),
			Purge:     true,
			KeepState: endpointOpts.keepConfig,
		})
		if brainsrvErr != nil {
			brainsrvErr = fmt.Errorf("remove brainsrv forwarder: %w", brainsrvErr)
		}
		return errors.Join(brainsrvErr, upstream(cmd, args))
	}
}
