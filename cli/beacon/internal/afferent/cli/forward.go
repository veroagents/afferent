package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/afferent/brain"
	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/afferent/config"
	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/afferent/forward"
	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/endpoint/lifecycle"
)

func init() { register((*app).forwardCmd) }

// logFlags select Beacon's runtime log, shared by forward and service.
type logFlags struct {
	logPath string
	system  bool
}

func (l *logFlags) add(cmd *cobra.Command) {
	cmd.Flags().StringVar(&l.logPath, "log-path", "", "Beacon runtime log to forward (default: from the Beacon endpoint configuration)")
	cmd.Flags().BoolVar(&l.system, "system", false, "forward the system-mode Beacon runtime log instead of the per-user one")
}

func (a *app) runtimeLog(l logFlags) string {
	if l.logPath != "" {
		if abs, err := filepath.Abs(l.logPath); err == nil {
			return abs
		}
		return l.logPath
	}
	if a.env.ResolveLog != nil {
		return a.env.ResolveLog(!l.system)
	}
	return lifecycle.ResolveRuntimeLog(!l.system, "").EffectiveLogPath
}

func (a *app) stateDir(r *resolved) string { return config.StateDir(r.dir, a.env.Getenv) }

// scopeResolver learns the member scope for r, preferring a configured one.
func (a *app) scopeResolver(r *resolved, override string) (*forward.ScopeResolver, *brain.Client) {
	ts := r.tokenSource(a.env.Now)
	bc := &brain.Client{BaseURL: r.cfg.BrainsrvURL, Context: r.cfg.Context, HTTP: a.env.HTTP, Token: ts.Token}
	if override == "" {
		override = r.cfg.Scope
	}
	return &forward.ScopeResolver{
		Override: override,
		StateDir: a.stateDir(r),
		URL:      r.cfg.BrainsrvURL,
		Context:  r.cfg.Context,
		Whoami:   bc.Whoami,
		Now:      a.env.Now,
		Account: func() string {
			c, err := r.store.Load()
			if err != nil {
				return ""
			}
			return c.Account()
		},
	}, bc
}

func (a *app) forwardCmd() *cobra.Command {
	var (
		lf       logFlags
		backfill bool
		once     bool
		scope    string
		flush    time.Duration
	)
	cmd := &cobra.Command{
		Use:   "forward",
		Short: "Forward Beacon's runtime log to brainsrv (runs in the foreground)",
		Long: `forward tails Beacon's runtime log (runtime.jsonl and its rotated archives)
and posts it to brainsrv's Beacon ingest as you, refreshing your access token
as needed. It keeps a checkpoint per file, so a restart resumes where brainsrv
last acknowledged. ` + "`afferent service install`" + ` runs it in the background.

A first run starts at the end of the log. --backfill starts from the oldest
retained archive instead (brainsrv drops events it already has).

If you are signed out or your scope is denied, forward pauses and retries; it
does not exit and does not drop anything. ` + "`afferent status`" + ` shows why.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			r, err := a.resolve()
			if err != nil {
				return err
			}
			if scope == "" {
				scope = r.cfg.Scope
			}
			resolver, _ := a.scopeResolver(r, scope)
			ts := r.tokenSource(a.env.Now)
			logger := newLogger(a.env.Stderr, a.now)
			opts := forward.Options{
				LogPath:       a.runtimeLog(lf),
				StateDir:      a.stateDir(r),
				URL:           r.cfg.BrainsrvURL,
				Context:       r.cfg.Context,
				Scope:         scope,
				Tokens:        ts,
				HTTP:          a.env.HTTP,
				Backfill:      backfill,
				Once:          once,
				FlushInterval: flush,
				Logf:          logger,
			}
			if scope == "" {
				opts.Rescope = func(ctx context.Context) (string, error) {
					s, src, err := resolver.Resolve(ctx)
					if err == nil && src == "cached" {
						logger("brainsrv did not answer /v1/whoami; using the cached scope %s", s)
					}
					return s, err
				}
			}
			if a.env.Sleep != nil {
				opts.Sleep = a.env.Sleep
			}
			if a.env.Now != nil {
				opts.Now = a.env.Now
			}
			fw, err := forward.New(opts)
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			if scope != "" {
				logger("forwarding %s to %s (context %s, scope %s)", opts.LogPath, opts.URL, opts.Context, scope)
			}
			if err := fw.Run(ctx); err != nil {
				return err
			}
			if once {
				s, _ := forward.ReadStatus(opts.StateDir)
				if s != nil {
					fmt.Fprintf(a.env.Stdout, "Drained %s: %d lines sent in total, %d bytes behind.\n", opts.LogPath, s.LinesSent, s.LagBytes)
				}
			}
			return nil
		},
	}
	lf.add(cmd)
	cmd.Flags().BoolVar(&backfill, "backfill", false, "start from the oldest retained archive instead of resuming")
	cmd.Flags().BoolVar(&once, "once", false, "send what is in the log now, then exit (errors are returned, not retried)")
	cmd.Flags().StringVar(&scope, "scope", "", "member base scope to send as X-Scope (env AFFERENT_SCOPE; default: learned from brainsrv /v1/whoami)")
	cmd.Flags().DurationVar(&flush, "flush-interval", forward.DefaultFlush, "longest a partial batch waits before it is sent")
	return cmd
}

// newLogger returns a Logf that timestamps lines onto w.
func newLogger(w io.Writer, now func() time.Time) func(string, ...any) {
	return func(format string, args ...any) {
		fmt.Fprintf(w, "%s afferent forward: %s\n", now().Format(time.RFC3339), fmt.Sprintf(format, args...))
	}
}
