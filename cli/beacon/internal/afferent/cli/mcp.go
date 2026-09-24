package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/afferent/mcpconfig"
	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/afferent/mcpproxy"
	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/version"
)

func init() { register((*app).mcpCmd) }

func (a *app) mcpCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Give coding agents brainsrv's MCP tools (a stdio proxy and agent config)",
		Args:  cobra.NoArgs,
	}
	cmd.AddCommand(a.mcpProxyCmd(), a.mcpConfigCmd())
	return cmd
}

func (a *app) mcpProxyCmd() *cobra.Command {
	var scope string
	cmd := &cobra.Command{
		Use:   "proxy",
		Short: "Run a stdio MCP server that relays to brainsrv /mcp as you",
		Long: `proxy is what coding agents start (` + "`afferent mcp config`" + ` registers it as
the "brain" MCP server). It reads JSON-RPC messages from stdin, one per line,
posts each to brainsrv's /mcp as the signed-in user (Bearer token, X-Context,
and your member scope as X-Scope), and writes brainsrv's answers to stdout,
one per line. Diagnostics go to stderr.

brainsrv ties an MCP session to the access token, which is refreshed every
~15 minutes; the proxy opens a new session with the agent's original
initialize parameters and retries, so the agent never notices.`,
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
			logf := func(format string, args ...any) {
				fmt.Fprintf(a.env.Stderr, "%s afferent mcp proxy: %s\n", a.now().Format(time.RFC3339), fmt.Sprintf(format, args...))
			}
			opts := mcpproxy.Options{
				URL:       r.cfg.BrainsrvURL,
				Context:   r.cfg.Context,
				Scope:     scope,
				Tokens:    r.tokenSource(a.env.Now),
				HTTP:      a.env.HTTP,
				Logf:      logf,
				UserAgent: "afferent-mcp-proxy/" + version.GetFullVersion(),
			}
			if scope == "" {
				opts.Resolve = func(ctx context.Context) (string, error) {
					s, src, err := resolver.Resolve(ctx)
					if err == nil && src == "cached" {
						logf("brainsrv did not answer /v1/whoami; using the cached scope %s", s)
					}
					return s, err
				}
			}
			p, err := mcpproxy.New(opts)
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			logf("relaying to %s%s (context %s)", r.cfg.BrainsrvURL, mcpproxy.Path, r.cfg.Context)
			return p.Serve(ctx, a.stdin(), a.env.Stdout)
		},
	}
	cmd.Flags().StringVar(&scope, "scope", "", "member base scope to send as X-Scope (env AFFERENT_SCOPE; default: learned from brainsrv /v1/whoami)")
	return cmd
}

// mcpOptions are the settings `mcp config` and setup share.
type mcpOptions struct {
	harness string
	dryRun  bool
	remove  bool
	program string
}

func (a *app) mcpConfigCmd() *cobra.Command {
	var o mcpOptions
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Register the proxy as the \"brain\" MCP server in Claude Code, Cursor and Codex",
		Long: `config adds an MCP server named "brain" that runs ` + "`afferent mcp proxy`" + ` to
each agent's user-level configuration:

  Claude Code  ~/.claude.json "mcpServers" (through ` + "`claude mcp add --scope user`" + `
               when the claude CLI is installed)
  Cursor       ~/.cursor/mcp.json "mcpServers"
  Codex        ~/.codex/config.toml [mcp_servers.brain], in a marked block

Only that one entry is written; everything else in the files stays as it
was. Each file is copied to <file>.afferent.bak before it changes. Running
it again is a no-op; --remove takes the entry out; --dry-run prints the
diff and changes nothing.

--harness auto (the default) configures the agents found in your home
directory.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if _, err := a.resolve(); err != nil {
				return err
			}
			_, err := a.runMCPConfig(cmd.Context(), o, a.env.Stdout)
			return err
		},
	}
	cmd.Flags().StringVar(&o.harness, "harness", "auto", "agents to configure: claude, cursor, codex (comma-separated), all, or auto")
	cmd.Flags().BoolVar(&o.dryRun, "dry-run", false, "print what would change, and change nothing")
	cmd.Flags().BoolVar(&o.remove, "remove", false, "remove the brain server instead of adding it")
	cmd.Flags().StringVar(&o.program, "program", "", "afferent binary the agents run (default: this binary)")
	return cmd
}

var harnessNames = map[string]string{
	mcpconfig.Claude: "Claude Code",
	mcpconfig.Cursor: "Cursor",
	mcpconfig.Codex:  "Codex",
}

func displayName(h string) string {
	if n, ok := harnessNames[h]; ok {
		return n
	}
	return h
}

// runMCPConfig applies o to every selected harness, reports each on out and
// returns the results. A failure on one harness does not stop the others.
func (a *app) runMCPConfig(ctx context.Context, o mcpOptions, out io.Writer) ([]mcpconfig.Result, error) {
	home, err := a.home()
	if err != nil {
		return nil, err
	}
	program, err := a.program(o.program)
	if err != nil {
		return nil, err
	}
	entry := mcpconfig.Entry{Command: program, Args: []string{"mcp", "proxy"}}
	if a.configDirExplicit() {
		r, err := a.resolve()
		if err != nil {
			return nil, err
		}
		entry.Args = append(entry.Args, "--config-dir", r.dir)
	}
	mo := mcpconfig.Options{Home: home, LookPath: a.env.LookPath, Run: a.env.Run, DryRun: o.dryRun, Remove: o.remove}
	harnesses, err := mcpconfig.ParseHarnesses(o.harness, mo)
	if err != nil {
		return nil, err
	}
	if len(harnesses) == 0 {
		fmt.Fprintln(out, "No supported agent found in your home directory (Claude Code, Cursor, Codex). Name one with --harness.")
		return nil, nil
	}
	var results []mcpconfig.Result
	var errs []error
	changed := false
	for _, h := range harnesses {
		res, err := mcpconfig.Apply(ctx, h, entry, mo)
		if err != nil {
			fmt.Fprintf(out, "%-12s error: %v\n", displayName(h)+":", err)
			errs = append(errs, fmt.Errorf("%s: %w", displayName(h), err))
			continue
		}
		results = append(results, res)
		changed = changed || res.Changed()
		fmt.Fprintf(out, "%-12s %s\n", displayName(h)+":", describe(res, o.dryRun))
		if res.Note != "" {
			fmt.Fprintf(out, "             note: %s\n", res.Note)
		}
		if o.dryRun && res.Changed() {
			if res.Command != "" {
				fmt.Fprintf(out, "             would run: %s\n", res.Command)
			}
			fmt.Fprint(out, indent(res.Diff, "             "))
		}
	}
	if changed && !o.dryRun && !o.remove {
		fmt.Fprintln(out, "Restart the agents (or run /mcp in Claude Code) to load the brain server.")
	}
	return results, errors.Join(errs...)
}

func describe(r mcpconfig.Result, dry bool) string {
	verb := map[string][2]string{
		"added":   {"added the brain server to ", "would add the brain server to "},
		"updated": {"updated the brain server in ", "would update the brain server in "},
		"removed": {"removed the brain server from ", "would remove the brain server from "},
	}
	switch r.Action {
	case "unchanged":
		return "already configured in " + r.Path
	case "absent":
		return "no brain server in " + r.Path
	}
	v := verb[r.Action]
	if dry {
		return v[1] + r.Path
	}
	s := v[0] + r.Path
	if r.Via == "claude CLI" {
		s += " (with the claude CLI)"
	}
	if r.Backup != "" {
		s += "; backup " + r.Backup
	}
	return s
}

func indent(s, prefix string) string {
	if s == "" {
		return ""
	}
	lines := strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	return prefix + strings.Join(lines, "\n"+prefix) + "\n"
}
