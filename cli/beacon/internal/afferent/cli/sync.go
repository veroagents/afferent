package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/afferent/forward"
	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/afferent/history"
)

func init() { register((*app).syncCmd) }

type syncOptions struct {
	harness string
	since   time.Duration
	noWait  bool
	lf      logFlags
}

// syncMaxRounds bounds the sweep → drain → sweep loop.
const syncMaxRounds = 50

// drainTimeout is how long sync waits for a running forwarder to catch up.
const drainTimeout = 30 * time.Minute

func (a *app) syncCmd() *cobra.Command {
	var o syncOptions
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Backfill existing Claude Code and Codex history so the forwarder sends it",
		Long: `sync reads the session history Claude Code (~/.claude/projects) and Codex
(~/.codex/sessions) already keep on this machine and appends it to Beacon's
runtime log, using Beacon's own collectors (the code behind ` + "`beacon endpoint\nclaude sync`" + ` and ` + "`codex sync`" + `). The forwarder then sends it to brainsrv.

Progress is kept in Beacon's cursor files (~/.beacon/endpoint/state/
claude.json and codex.json), shared with Beacon's own sync, so a session is
never written to the log twice. Running it again only adds what is new.

--since 720h skips sessions not written in the last 30 days (they are
marked as read, so later runs skip them too).

The runtime log keeps about 60 MiB. A large history is written in rounds:
when the log is full of unsent history, sync waits for the forwarder to send
it (or sends it itself when no forwarder is running), then continues.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runSync(cmd.Context(), o, a.env.Stdout)
		},
	}
	cmd.Flags().StringVar(&o.harness, "harness", "auto", "history to read: claude, codex (comma-separated), all, or auto")
	cmd.Flags().DurationVar(&o.since, "since", 0, "only sessions written within this long (for example 720h); 0 means all")
	cmd.Flags().BoolVar(&o.noWait, "no-wait", false, "do not wait for the forwarder when the log fills up; run sync again later")
	o.lf.add(cmd)
	return cmd
}

func syncHarnesses(v, home string) ([]string, error) {
	v = strings.ToLower(strings.TrimSpace(v))
	switch v {
	case "", "auto":
		var out []string
		for _, h := range history.All {
			if history.Present(home, h) {
				out = append(out, h)
			}
		}
		return out, nil
	case "all":
		return append([]string(nil), history.All...), nil
	}
	var out []string
	for _, part := range strings.Split(v, ",") {
		h := strings.TrimSpace(part)
		switch h {
		case "claude-code", "claude_code":
			h = history.Claude
		case "":
			continue
		}
		if h != history.Claude && h != history.Codex {
			return nil, fmt.Errorf("sync supports claude and codex, not %q", part)
		}
		out = append(out, h)
	}
	return out, nil
}

func (a *app) runSync(ctx context.Context, o syncOptions, out io.Writer) error {
	r, err := a.resolve()
	if err != nil {
		return err
	}
	home, err := a.home()
	if err != nil {
		return err
	}
	harnesses, err := syncHarnesses(o.harness, home)
	if err != nil {
		return err
	}
	if len(harnesses) == 0 {
		fmt.Fprintln(out, "No Claude Code or Codex history found in your home directory.")
		return nil
	}
	logPath := a.runtimeLog(o.lf)
	stateDir := a.stateDir(r)

	// The forwarder's first run starts at the end of the log. Place its
	// checkpoints before writing, so the history below counts as new.
	placed, perr := forward.Prime(stateDir, logPath)
	switch {
	case errors.Is(perr, forward.ErrAlreadyRunning):
		if s, _ := forward.ReadStatus(stateDir); s != nil && s.LogPath != "" && s.LogPath != logPath {
			fmt.Fprintf(a.env.Stderr, "warning: the running forwarder reads %s, but sync writes %s\n", s.LogPath, logPath)
		}
	case perr != nil:
		return fmt.Errorf("prepare the forwarder checkpoints: %w", perr)
	case placed:
		fmt.Fprintf(out, "The forwarder had not run yet; its checkpoints now start at the current end of %s.\n", logPath)
	}

	opts := history.Options{Home: home, LogPath: logPath, UserMode: !o.lf.system, Since: o.since, Now: a.env.Now}
	totals := map[string]*history.Report{}
	var errs []error
	for round := 1; ; round++ {
		limited := false
		for _, h := range harnesses {
			rep, err := history.Sync(h, opts)
			t := totals[h]
			if t == nil {
				t = &history.Report{Harness: h, StatePath: rep.StatePath}
				totals[h] = t
			}
			t.Sessions = rep.Sessions
			t.SessionsChanged += rep.SessionsChanged
			t.Events += rep.Events
			t.SkippedOld += rep.SkippedOld
			t.Errors += rep.Errors
			t.SessionsPending = rep.SessionsPending
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", h, err))
			}
			if rep.RetentionLimited {
				limited = true
			}
		}
		opts.Since = 0 // marked once is enough
		if !limited {
			break
		}
		if o.noWait || round >= syncMaxRounds {
			fmt.Fprintln(out, "The runtime log is full of history the forwarder has not sent yet; some sessions are still pending.")
			fmt.Fprintln(out, "Run `afferent sync` again once `afferent status` shows the forwarder caught up.")
			break
		}
		fmt.Fprintln(out, "The runtime log is full of unsent history; sending it before reading more…")
		if err := a.drain(ctx, r, logPath, stateDir); err != nil {
			return fmt.Errorf("history is partly written; the forwarder could not send it yet: %w", err)
		}
	}
	for _, h := range harnesses {
		t := totals[h]
		line := fmt.Sprintf("%-12s %d sessions, %d with new records, %d events written", displayName(h)+":", t.Sessions, t.SessionsChanged, t.Events)
		if t.SkippedOld > 0 {
			line += fmt.Sprintf(", %d older sessions skipped", t.SkippedOld)
		}
		if t.Errors > 0 {
			line += fmt.Sprintf(", %d errors", t.Errors)
		}
		if t.SessionsPending > 0 {
			line += fmt.Sprintf(", %d pending", t.SessionsPending)
		}
		fmt.Fprintln(out, line)
	}
	fmt.Fprintf(out, "Written to %s; the forwarder sends it to brainsrv.\n", logPath)
	if s, _ := forward.ReadStatus(stateDir); s == nil || s.State == forward.StateStopped {
		fmt.Fprintln(out, "No forwarder is running: start one with `afferent service install` (or `afferent forward`).")
	}
	return errors.Join(errs...)
}

// drain sends what is in the log: with a one-shot forwarder when none is
// running, else by waiting for the running one to catch up.
func (a *app) drain(ctx context.Context, r *resolved, logPath, stateDir string) error {
	scope := r.cfg.Scope
	resolver, _ := a.scopeResolver(r, scope)
	opts := forward.Options{
		LogPath:  logPath,
		StateDir: stateDir,
		URL:      r.cfg.BrainsrvURL,
		Context:  r.cfg.Context,
		Scope:    scope,
		Tokens:   r.tokenSource(a.env.Now),
		HTTP:     a.env.HTTP,
		Once:     true,
		Logf:     newLogger(a.env.Stderr, a.now),
	}
	if scope == "" {
		opts.Rescope = func(ctx context.Context) (string, error) {
			s, _, err := resolver.Resolve(ctx)
			return s, err
		}
	}
	if a.env.Now != nil {
		opts.Now = a.env.Now
	}
	fw, err := forward.New(opts)
	if err != nil {
		return err
	}
	err = fw.Run(ctx)
	if !errors.Is(err, forward.ErrAlreadyRunning) {
		return err
	}
	// The service is running: wait until it has sent everything.
	start := a.now()
	for {
		s, _ := forward.ReadStatus(stateDir)
		if s != nil && s.UpdatedAt.After(start) && s.LagBytes == 0 {
			return nil
		}
		if a.now().Sub(start) > drainTimeout {
			reason := "it is behind"
			if s != nil && s.PausedReason != "" {
				reason = "it is paused: " + s.PausedReason
			}
			return fmt.Errorf("waited %s for the forwarder; %s (see `afferent status`)", drainTimeout, reason)
		}
		if err := a.sleep(ctx, 2*time.Second); err != nil {
			return err
		}
	}
}

func (a *app) sleep(ctx context.Context, d time.Duration) error {
	if a.env.Sleep != nil {
		return a.env.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
