package cmd

// endpoint_brainsrv.go adds `beacon endpoint brainsrv connect|status|disconnect|print-config|
// install-pack|validate` (afferent SPEC §4 B3, PLAN §4 Phase 5). It registers itself on
// endpointCmd from its own init(), so upstream's cmd/endpoint.go needs no edit (PLAN B-5).

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/endpoint/asymptote"
	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/endpoint/brainsrv"
	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/endpoint/lifecycle"
)

var brainsrvOpts struct {
	url         string
	scope       string
	keyFile     string
	backfill    bool
	vectorBin   string
	purge       bool
	noHealth    bool
	template    bool
	packOutput  string
	packLogPath string
}

// Seams for tests: the forwarder service manager and system-privilege check.
var (
	brainsrvForwarder    func(userMode bool) brainsrv.Forwarder = func(userMode bool) brainsrv.Forwarder { return brainsrv.NewForwarderManager(userMode) }
	brainsrvHasSystemRun                                        = lifecycle.HasSystemPrivileges
)

var endpointBrainsrvCmd = &cobra.Command{
	Use:   "brainsrv",
	Short: "Forward runtime telemetry to brainsrv (afferent)",
	Long: `Forward this endpoint's runtime JSONL to brainsrv's Beacon ingest.

A Vector forwarder, separate from the Beacon Managed one, tails runtime.jsonl and
POSTs gzip NDJSON to <url>/v1/ingest/beacon/runtime with X-Scope: <scope> and
the member's own spk_ API key. brainsrv files each session under
<scope>.<repo_label>.<harness_label>.`,
}

var endpointBrainsrvConnectCmd = &cobra.Command{
	Use:   "connect",
	Short: "Check the key against brainsrv and start the brainsrv forwarder",
	Long: `Connect this endpoint to brainsrv.

The key file must be a regular file owned by you with mode 0600 holding one spk_
key minted for you with narrow_scope = your base scope
(ws.<ws_id>.people.<member>.harness) and read+write grants there. connect checks
with GET /v1/ingest/beacon/health?scope=<scope> (and an empty write probe) that
the key can read and write --scope, and refuses otherwise, before writing
anything. It then stores the key in a 0600 secrets file, renders and validates
the Vector config, and installs com.afferent.brainsrv-forwarder (launchd) or
afferent-brainsrv-forwarder.service (systemd).

By default only activity recorded after the forwarder first starts is sent.
--backfill sends the existing log too: it renders read_from = "beginning" and
clears this forwarder's checkpoints. A later connect without --backfill renders
read_from = "end" again and keeps the checkpoints, so it resumes where it was.
Re-sent lines are safe: brainsrv deduplicates on event.id.`,
	SilenceUsage: true,
	RunE:         runEndpointBrainsrvConnect,
}

var endpointBrainsrvStatusCmd = &cobra.Command{
	Use:          "status",
	Short:        "Show the brainsrv forwarder, its checkpoint, and brainsrv's last_seen",
	SilenceUsage: true,
	RunE:         runEndpointBrainsrvStatus,
}

var endpointBrainsrvDisconnectCmd = &cobra.Command{
	Use:   "disconnect",
	Short: "Stop and remove the brainsrv forwarder and its stored key",
	Long: `Stop the brainsrv forwarder, remove its service unit, rendered config,
connection record and stored key. Vector's data dir (checkpoints and any
undelivered buffer) is kept unless --purge is given. The key is not revoked in
brainsrv; revoke it there.`,
	SilenceUsage: true,
	RunE:         runEndpointBrainsrvDisconnect,
}

var endpointBrainsrvPrintConfigCmd = &cobra.Command{
	Use:          "print-config",
	Short:        "Print the brainsrv forwarder's Vector config",
	Long:         "Print the rendered Vector config of the connected forwarder, or the template (with environment references) when not connected or with --template.",
	SilenceUsage: true,
	RunE:         runEndpointBrainsrvPrintConfig,
}

var endpointBrainsrvInstallPackCmd = &cobra.Command{
	Use:          "install-pack",
	Short:        "Write the brainsrv forwarding pack (vector.toml, README) for running Vector by hand",
	SilenceUsage: true,
	RunE:         runEndpointBrainsrvInstallPack,
}

var endpointBrainsrvValidateCmd = &cobra.Command{
	Use:          "validate",
	Short:        "Run vector validate on the brainsrv forwarder config",
	SilenceUsage: true,
	RunE:         runEndpointBrainsrvValidate,
}

func init() {
	endpointCmd.AddCommand(endpointBrainsrvCmd)
	subcommands := []*cobra.Command{
		endpointBrainsrvConnectCmd, endpointBrainsrvStatusCmd, endpointBrainsrvDisconnectCmd,
		endpointBrainsrvPrintConfigCmd, endpointBrainsrvInstallPackCmd, endpointBrainsrvValidateCmd,
	}
	for _, c := range subcommands {
		endpointBrainsrvCmd.AddCommand(c)
		if c != endpointBrainsrvInstallPackCmd {
			addEndpointPathFlags(c)
		}
	}
	for _, c := range []*cobra.Command{endpointBrainsrvConnectCmd, endpointBrainsrvStatusCmd, endpointBrainsrvDisconnectCmd} {
		c.Flags().BoolVar(&endpointOpts.jsonOutput, "json", false, "Print output as JSON")
	}
	f := endpointBrainsrvConnectCmd.Flags()
	f.StringVar(&brainsrvOpts.url, "url", "", "brainsrv base URL (https; http only for localhost)")
	f.StringVar(&brainsrvOpts.scope, "scope", "", "Your base scope, e.g. ws.<ws_id>.people.<member>.harness")
	f.StringVar(&brainsrvOpts.keyFile, "key-file", "", "0600 file holding your own spk_ key")
	f.BoolVar(&brainsrvOpts.backfill, "backfill", false, "Also send the existing runtime log (read_from = beginning; clears this forwarder's checkpoints)")
	for _, c := range []*cobra.Command{endpointBrainsrvConnectCmd, endpointBrainsrvValidateCmd} {
		c.Flags().StringVar(&brainsrvOpts.vectorBin, "vector-bin", "", "Vector binary to run (defaults to "+asymptote.VectorBinEnv+", "+asymptote.PackagedVectorPath+", Homebrew, then PATH)")
	}
	for _, name := range []string{"url", "scope", "key-file"} {
		_ = endpointBrainsrvConnectCmd.MarkFlagRequired(name)
	}
	endpointBrainsrvStatusCmd.Flags().BoolVar(&brainsrvOpts.noHealth, "offline", false, "Skip the brainsrv health call")
	endpointBrainsrvDisconnectCmd.Flags().BoolVar(&brainsrvOpts.purge, "purge", false, "Also remove Vector's data dir (checkpoints and undelivered buffer)")
	endpointBrainsrvPrintConfigCmd.Flags().BoolVar(&brainsrvOpts.template, "template", false, "Print the template even when connected")
	endpointBrainsrvInstallPackCmd.Flags().StringVar(&brainsrvOpts.packOutput, "output", brainsrv.DefaultOutputDir, "Directory to write the pack to")
	endpointBrainsrvInstallPackCmd.Flags().StringVar(&brainsrvOpts.packLogPath, "log-path", "", "Runtime JSONL log path to template in (defaults to "+brainsrv.DefaultLogPath+")")
}

func brainsrvRequireSystem(userMode bool, verb string) error {
	if !userMode && !brainsrvHasSystemRun() {
		return fmt.Errorf("%s the brainsrv forwarder for a system endpoint needs root: rerun with sudo, or pass --user", verb)
	}
	return nil
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func runEndpointBrainsrvConnect(cmd *cobra.Command, _ []string) error {
	userMode := endpointUserMode()
	if err := brainsrvRequireSystem(userMode, "connecting"); err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	progress := out
	if endpointOpts.jsonOutput {
		progress = cmd.ErrOrStderr()
	}
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	result, err := brainsrv.Connect(ctx, brainsrv.ConnectOptions{
		UserMode:  userMode,
		URL:       brainsrvOpts.url,
		Scope:     brainsrvOpts.scope,
		KeyFile:   brainsrvOpts.keyFile,
		Backfill:  brainsrvOpts.backfill,
		LogPath:   loadOrDefaultConfig().LogPath,
		VectorBin: brainsrvOpts.vectorBin,
		Forwarder: brainsrvForwarder(userMode),
		Out:       progress,
	})
	if err != nil {
		return err
	}
	if endpointOpts.jsonOutput {
		return writeJSON(out, result)
	}
	fmt.Fprintf(out, "Forwarding %s to %s as %s\n", result.Connection.LogPath, result.Connection.URL, result.Connection.Scope)
	fmt.Fprintf(out, "Forwarder: %s (loaded=%t running=%t)\n", result.Forwarder, result.ForwarderState.Loaded, result.ForwarderState.Running)
	fmt.Fprintf(out, "Vector config: %s\n", result.VectorConfig)
	fmt.Fprintf(out, "Key: %s (never printed; the Vector forwarder reads it)\n", result.SecretsFile)
	if result.Connection.Backfill {
		fmt.Fprintln(out, "Backfill: the existing log is being sent; run connect without --backfill later to keep resuming from the checkpoint.")
	} else {
		fmt.Fprintln(out, "Only activity from now on is sent; use --backfill to send the existing log.")
	}
	return nil
}

func runEndpointBrainsrvStatus(cmd *cobra.Command, _ []string) error {
	userMode := endpointUserMode()
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	status := brainsrv.Status(ctx, userMode, brainsrv.StatusOptions{
		SkipHealthCheck: brainsrvOpts.noHealth,
		Forwarder:       brainsrvForwarder(userMode),
	})
	out := cmd.OutOrStdout()
	if endpointOpts.jsonOutput {
		return writeJSON(out, status)
	}
	if !status.Connected {
		if status.Message != "" {
			fmt.Fprintf(out, "brainsrv: not connected (%s)\n", status.Message)
		} else {
			fmt.Fprintln(out, "brainsrv: not connected (run `beacon endpoint brainsrv connect`)")
		}
		return nil
	}
	fmt.Fprintf(out, "brainsrv: connected to %s as %s (key %s…)\n", status.URL, status.Scope, status.KeyPrefix)
	fmt.Fprintf(out, "Forwarder: %s (loaded=%t running=%t)", status.Forwarder.Label, status.Forwarder.Loaded, status.Forwarder.Running)
	if status.Forwarder.Message != "" {
		fmt.Fprintf(out, " %s", status.Forwarder.Message)
	}
	fmt.Fprintln(out)
	if len(status.Checkpoints) > 0 {
		for _, cp := range status.Checkpoints {
			fmt.Fprintf(out, "Checkpoint: %s at byte %d\n", status.LogPath, cp.Position)
		}
	} else if status.CheckpointMessage != "" {
		fmt.Fprintf(out, "Checkpoint: %s\n", status.CheckpointMessage)
	}
	fmt.Fprintf(out, "Vector data dir: %d bytes\n", status.DataDirBytes)
	switch status.Health {
	case "":
	case "ok":
		if status.LastSeen != nil {
			fmt.Fprintf(out, "brainsrv last_seen for this host: %s\n", status.LastSeen.Local().Format(time.RFC3339))
		} else {
			fmt.Fprintln(out, "brainsrv has not received a batch from this host yet")
		}
	default:
		fmt.Fprintf(out, "brainsrv health: %s (%s)\n", status.Health, status.HealthMessage)
	}
	return nil
}

func runEndpointBrainsrvDisconnect(cmd *cobra.Command, _ []string) error {
	userMode := endpointUserMode()
	if err := brainsrvRequireSystem(userMode, "disconnecting"); err != nil {
		return err
	}
	connection, _ := brainsrv.LoadConnection(userMode)
	if err := brainsrv.Disconnect(brainsrv.DisconnectOptions{
		UserMode:  userMode,
		Forwarder: brainsrvForwarder(userMode),
		Purge:     brainsrvOpts.purge,
	}); err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	if endpointOpts.jsonOutput {
		return writeJSON(out, map[string]any{"disconnected": true, "purged": brainsrvOpts.purge})
	}
	fmt.Fprintln(out, "brainsrv forwarder stopped and removed; stored key deleted.")
	if brainsrvOpts.purge {
		fmt.Fprintf(out, "Removed %s.\n", brainsrv.Dir(userMode))
	} else {
		fmt.Fprintf(out, "Kept Vector checkpoints and buffer in %s (--purge removes them).\n", brainsrv.DataDir(userMode))
	}
	if connection != nil {
		fmt.Fprintf(out, "Key %s… is still valid in brainsrv; revoke it there if it should never be used again.\n", connection.KeyPrefix)
	}
	return nil
}

func runEndpointBrainsrvPrintConfig(cmd *cobra.Command, _ []string) error {
	userMode := endpointUserMode()
	if !brainsrvOpts.template {
		if data, err := brainsrvReadIfExists(brainsrv.VectorConfigPath(userMode)); err != nil {
			return err
		} else if data != nil {
			_, err := cmd.OutOrStdout().Write(data)
			return err
		}
	}
	content, err := brainsrv.VectorConfig(loadOrDefaultConfig().LogPath)
	if err != nil {
		return err
	}
	_, err = io.WriteString(cmd.OutOrStdout(), content)
	return err
}

func runEndpointBrainsrvInstallPack(cmd *cobra.Command, _ []string) error {
	logPath := brainsrvOpts.packLogPath
	if logPath == "" {
		logPath = brainsrv.DefaultLogPath
	}
	if err := brainsrv.InstallPack(brainsrvOpts.packOutput, logPath); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Wrote the brainsrv forwarding pack to %s\n", brainsrvOpts.packOutput)
	return nil
}

func runEndpointBrainsrvValidate(cmd *cobra.Command, _ []string) error {
	userMode := endpointUserMode()
	vector, target, err := brainsrv.Validate(userMode, brainsrvOpts.vectorBin, loadOrDefaultConfig().LogPath)
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Vector %s at %s accepts the brainsrv forwarder config (%s)\n", vector.Version, vector.Path, target)
	return nil
}

// brainsrvReadIfExists returns nil, nil for a missing file.
func brainsrvReadIfExists(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	return data, err
}
