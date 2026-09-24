package cmd

// memory_brainsrv.go adds `beacon memory brainsrv status|sync` (afferent
// PLAN §4 Phase 2). It registers itself on memoryCmd from its own init(), so
// upstream's cmd/memory.go needs no edit beyond the OpenConfigured switch.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/brainsrvcfg"
	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/learning"
)

var memoryBrainsrvOpts struct {
	all bool
}

var memoryBrainsrvCmd = &cobra.Command{
	Use:   "brainsrv",
	Short: "Inspect and sync the brainsrv memory backend",
	Long: `Inspect and sync the brainsrv memory backend.

The backend is configured from the environment:
  BEACON_MEMORY_BACKEND=brainsrv
  BEACON_BRAINSRV_URL       https URL (http only for localhost)
  BEACON_BRAINSRV_SCOPE     your base scope, e.g. ws.<id>.people.<member>.harness
  BEACON_BRAINSRV_KEY_FILE  0600 file holding your spk_ key`,
}

var memoryBrainsrvStatusCmd = &cobra.Command{
	Use:          "status",
	Short:        "Show brainsrv reachability and memory sync counts",
	SilenceUsage: true,
	RunE:         runMemoryBrainsrvStatus,
}

var memoryBrainsrvSyncCmd = &cobra.Command{
	Use:          "sync",
	Short:        "Retry pending and failed memory syncs to brainsrv",
	SilenceUsage: true,
	RunE:         runMemoryBrainsrvSync,
}

func init() {
	memoryCmd.AddCommand(memoryBrainsrvCmd)
	memoryBrainsrvCmd.AddCommand(memoryBrainsrvStatusCmd)
	memoryBrainsrvCmd.AddCommand(memoryBrainsrvSyncCmd)
	for _, c := range []*cobra.Command{memoryBrainsrvStatusCmd, memoryBrainsrvSyncCmd} {
		c.Flags().BoolVar(&memoryOpts.userMode, "user", true, "Use per-user endpoint paths")
		c.Flags().BoolVar(&memoryOpts.systemMode, "system", false, "Use system endpoint paths")
		c.Flags().StringVar(&memoryOpts.logPath, "log-path", "", "Runtime JSONL log path")
		c.Flags().BoolVar(&memoryOpts.jsonOutput, "json", false, "Print machine-readable JSON")
	}
	memoryBrainsrvSyncCmd.Flags().BoolVar(&memoryBrainsrvOpts.all, "all", false, "Re-send every local memory, not only pending and failed ones")
}

// brainsrvBackendForCmd opens the configured store and returns its brainsrv
// backend, or explains why there is none.
func brainsrvBackendForCmd() (*learning.Store, *learning.BrainsrvBackend, error) {
	cfg, err := brainsrvcfg.FromEnv()
	if errors.Is(err, brainsrvcfg.ErrNotConfigured) {
		return nil, nil, fmt.Errorf("brainsrv backend not configured: set %s=brainsrv, %s, %s and %s",
			brainsrvcfg.EnvBackend, brainsrvcfg.EnvURL, brainsrvcfg.EnvScope, brainsrvcfg.EnvKeyFile)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("brainsrv backend misconfigured: %w", err)
	}
	if _, err := brainsrvcfg.ReadKeyFile(cfg.KeyFile); err != nil {
		return nil, nil, fmt.Errorf("brainsrv backend misconfigured: %s: %w", brainsrvcfg.EnvKeyFile, err)
	}
	store := memoryStore()
	backend, ok := learning.BrainsrvOf(store)
	if !ok {
		return nil, nil, errors.New("brainsrv backend could not be opened")
	}
	return store, backend, nil
}

type memoryBrainsrvStatusResult struct {
	URL         string                   `json:"url"`
	Scope       string                   `json:"scope"`
	StorePath   string                   `json:"store_path"`
	Reachable   bool                     `json:"reachable"`
	Error       string                   `json:"error,omitempty"`
	Health      *learning.BrainsrvHealth `json:"health,omitempty"`
	SyncCounts  map[string]int           `json:"sync_counts"`
	SyncBacklog int                      `json:"sync_backlog"`
}

func runMemoryBrainsrvStatus(cmd *cobra.Command, args []string) error {
	store, backend, err := brainsrvBackendForCmd()
	if err != nil {
		return err
	}
	cfg := backend.Config()
	result := memoryBrainsrvStatusResult{URL: cfg.URL, Scope: cfg.Scope, StorePath: store.Path()}
	ctx, cancel := context.WithTimeout(cmdContext(cmd), 10*time.Second)
	defer cancel()
	health, herr := backend.Health(ctx)
	if herr != nil {
		result.Error = herr.Error()
	} else {
		result.Reachable = health.OK
		result.Health = &health
	}
	counts, err := backend.SyncCounts()
	if err != nil {
		return err
	}
	result.SyncCounts = counts
	result.SyncBacklog = counts[learning.SyncStatePending] + counts[learning.SyncStateFailed]
	out := cmd.OutOrStdout()
	if memoryOpts.jsonOutput {
		if err := writeJSONTo(out, result); err != nil {
			return err
		}
	} else {
		reach := "reachable"
		if !result.Reachable {
			reach = "UNREACHABLE"
			if result.Error != "" {
				reach += " (" + result.Error + ")"
			}
		}
		fmt.Fprintf(out, "brainsrv  %s  %s\n", result.URL, reach)
		fmt.Fprintf(out, "scope     %s\n", result.Scope)
		fmt.Fprintf(out, "store     %s\n", result.StorePath)
		if result.Health != nil {
			fmt.Fprintf(out, "endpoints %d\n", len(result.Health.Endpoints))
		}
		fmt.Fprintf(out, "sync      %d synced, %d pending, %d failed\n",
			counts[learning.SyncStateSynced], counts[learning.SyncStatePending], counts[learning.SyncStateFailed])
	}
	if herr != nil {
		return fmt.Errorf("brainsrv health check failed: %w", herr)
	}
	return nil
}

func runMemoryBrainsrvSync(cmd *cobra.Command, args []string) error {
	_, backend, err := brainsrvBackendForCmd()
	if err != nil {
		return err
	}
	report, err := backend.Sync(cmdContext(cmd), memoryBrainsrvOpts.all)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	if memoryOpts.jsonOutput {
		if err := writeJSONTo(out, report); err != nil {
			return err
		}
	} else {
		fmt.Fprintf(out, "attempted %d, synced %d, pending %d, failed %d\n", report.Attempted, report.Synced, report.Pending, report.Failed)
		for _, e := range report.Errors {
			fmt.Fprintln(cmd.ErrOrStderr(), "  "+e)
		}
	}
	if report.Pending+report.Failed > 0 {
		return fmt.Errorf("%d memories not synced", report.Pending+report.Failed)
	}
	return nil
}

func cmdContext(cmd *cobra.Command) context.Context {
	if ctx := cmd.Context(); ctx != nil {
		return ctx
	}
	return context.Background()
}

func writeJSONTo(w io.Writer, v interface{}) error {
	if w == nil {
		w = os.Stdout
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
