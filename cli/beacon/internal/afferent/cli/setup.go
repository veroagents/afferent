package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/afferent/auth"
	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/afferent/capture"
	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/afferent/history"
	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/afferent/mcpconfig"
)

func init() { register((*app).setupCmd) }

// Setup steps, in order.
const (
	stepCapture = "capture"
	stepLogin   = "login"
	stepScope   = "scope"
	stepService = "service"
	stepMCP     = "mcp"
	stepSync    = "sync"
)

var setupSteps = []string{stepCapture, stepLogin, stepScope, stepService, stepMCP, stepSync}

var stepTitles = map[string]string{
	stepCapture: "Beacon capture",
	stepLogin:   "Sign-in",
	stepScope:   "brainsrv scope",
	stepService: "Forwarder service",
	stepMCP:     "Agents' MCP config",
	stepSync:    "History backfill",
}

type setupOptions struct {
	harness   string
	yes       bool
	dryRun    bool
	skip      []string
	since     time.Duration
	noBrowser bool
}

type setupRun struct {
	a       *app
	o       setupOptions
	ctx     context.Context
	out     io.Writer
	in      *bufio.Reader
	skip    map[string]bool
	results map[string]string
	failed  bool
}

func (a *app) setupCmd() *cobra.Command {
	var o setupOptions
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Set up this machine: capture, sign-in, forwarder, agents' MCP, history",
		Long: `setup walks through everything afferent needs, and is safe to run again:

  1. capture   install Beacon's capture hooks for your coding agents
  2. login     sign in with your browser, if you are not signed in
  3. scope     ask brainsrv which member scope you write to
  4. service   run the forwarder in the background (launchd / systemd --user)
  5. mcp       register the "brain" MCP server in your agents' configs
  6. sync      offer to backfill the history your agents already keep

Steps that change files show what they will change and ask first, unless
--yes. --skip leaves steps out (for example --skip sync,mcp). --dry-run
shows every step and changes nothing anywhere.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runSetup(cmd.Context(), o)
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.harness, "harness", "auto", "agents to set up: claude, codex, cursor (comma-separated), all, or auto")
	f.BoolVar(&o.yes, "yes", false, "answer yes to every question")
	f.BoolVar(&o.dryRun, "dry-run", false, "show what each step would do, and change nothing")
	f.StringSliceVar(&o.skip, "skip", nil, "steps to leave out: "+strings.Join(setupSteps, ", "))
	f.DurationVar(&o.since, "since", 0, "history backfill: only sessions written within this long; 0 means all")
	f.BoolVar(&o.noBrowser, "no-browser", false, "print the sign-in link instead of opening a browser")
	return cmd
}

func (a *app) runSetup(ctx context.Context, o setupOptions) error {
	s := &setupRun{a: a, o: o, ctx: ctx, out: a.env.Stdout, in: bufio.NewReader(a.stdin()), skip: map[string]bool{}, results: map[string]string{}}
	for _, k := range o.skip {
		k = strings.ToLower(strings.TrimSpace(k))
		if _, ok := stepTitles[k]; !ok {
			return fmt.Errorf("unknown step %q (steps: %s)", k, strings.Join(setupSteps, ", "))
		}
		s.skip[k] = true
	}
	r, err := a.resolve()
	if err != nil {
		return err
	}
	home, err := a.home()
	if err != nil {
		return err
	}
	harnesses, err := mcpconfig.ParseHarnesses(o.harness, mcpconfig.Options{Home: home, LookPath: a.env.LookPath})
	if err != nil {
		return err
	}
	if o.dryRun {
		fmt.Fprintln(s.out, "Dry run: nothing will be changed.")
	}
	fmt.Fprintf(s.out, "brainsrv %s, Context %s, issuer %s\n", r.cfg.BrainsrvURL, r.cfg.Context, r.cfg.Issuer)
	if len(harnesses) == 0 {
		fmt.Fprintln(s.out, "No coding agent found in your home directory (Claude Code, Codex, Cursor); name them with --harness.")
	} else {
		names := make([]string, len(harnesses))
		for i, h := range harnesses {
			names[i] = displayName(h)
		}
		fmt.Fprintf(s.out, "Agents: %s\n", strings.Join(names, ", "))
	}

	s.step(stepCapture, func() string { return s.capture(harnesses) })
	signedIn := false
	s.step(stepLogin, func() string {
		var res string
		signedIn, res = s.login(r)
		return res
	})
	s.step(stepScope, func() string { return s.scope(r, signedIn) })
	s.step(stepService, func() string { return s.service(r) })
	s.step(stepMCP, func() string { return s.mcp(harnesses) })
	s.step(stepSync, func() string { return s.sync(harnesses, home) })

	fmt.Fprintln(s.out, "\nSummary")
	for _, k := range setupSteps {
		fmt.Fprintf(s.out, "  %-20s %s\n", stepTitles[k], s.results[k])
	}
	if s.failed {
		return errors.New("setup finished with errors (see above); fix them and run `afferent setup` again")
	}
	if !o.dryRun {
		fmt.Fprintln(s.out, "\nCheck on it any time with `afferent status`.")
	}
	return nil
}

func (s *setupRun) step(k string, run func() string) {
	fmt.Fprintf(s.out, "\n== %s ==\n", stepTitles[k])
	if s.skip[k] {
		s.results[k] = "skipped (--skip)"
		fmt.Fprintln(s.out, "Skipped.")
		return
	}
	s.results[k] = run()
}

func (s *setupRun) fail(format string, args ...any) string {
	s.failed = true
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintln(s.out, "Error:", msg)
	return "failed: " + msg
}

// ask asks a yes/no question (default yes). --yes answers it; no answer
// (end of input) is no.
func (s *setupRun) ask(q string) bool {
	if s.o.yes {
		fmt.Fprintf(s.out, "%s yes (--yes)\n", q)
		return true
	}
	fmt.Fprintf(s.out, "%s [Y/n] ", q)
	line, err := s.in.ReadString('\n')
	if err != nil && strings.TrimSpace(line) == "" {
		fmt.Fprintln(s.out, "\n(no answer; skipping)")
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "", "y", "yes":
		return true
	}
	return false
}

func (s *setupRun) capture(harnesses []string) string {
	logPath := s.a.runtimeLog(logFlags{})
	inst := s.a.capture()
	supported := map[string]bool{}
	for _, h := range capture.Harnesses {
		supported[h] = true
	}
	var todo, have []string
	for _, h := range harnesses {
		if !supported[h] {
			continue
		}
		installed, path, err := inst.Status(h)
		if err != nil {
			return s.fail("%s: %v", displayName(h), err)
		}
		if installed {
			have = append(have, displayName(h))
			continue
		}
		todo = append(todo, h)
		fmt.Fprintf(s.out, "%s: add Beacon's capture hooks to %s\n", displayName(h), path)
	}
	if len(have) > 0 {
		fmt.Fprintf(s.out, "Already capturing: %s\n", strings.Join(have, ", "))
	}
	if len(todo) == 0 {
		if len(have) == 0 {
			return "no agent to capture"
		}
		return "already installed"
	}
	fmt.Fprintf(s.out, "Events go to %s.\n", logPath)
	if s.o.dryRun {
		return "would install for " + names(todo)
	}
	if !s.ask("Install Beacon capture now?") {
		return "not installed (declined)"
	}
	var done []string
	for _, h := range todo {
		path, err := inst.Install(h, logPath, true)
		if err != nil {
			return s.fail("%s: %v (installed so far: %s)", displayName(h), err, strings.Join(done, ", "))
		}
		fmt.Fprintf(s.out, "%s: capture hooks installed in %s\n", displayName(h), path)
		done = append(done, displayName(h))
	}
	return "installed for " + strings.Join(done, ", ")
}

func names(hs []string) string {
	out := make([]string, len(hs))
	for i, h := range hs {
		out[i] = displayName(h)
	}
	return strings.Join(out, ", ")
}

// login signs in when needed. In a dry run it only reads the stored
// credentials: a refresh would rotate them.
func (s *setupRun) login(r *resolved) (bool, string) {
	if s.o.dryRun {
		c, err := r.store.Load()
		if err != nil {
			fmt.Fprintln(s.out, "Not signed in.")
			return false, "would sign in (browser)"
		}
		who := "a stored session"
		if cl, err := auth.DecodeClaims(c.AccessToken); err == nil {
			who = firstNonEmpty(cl.Email, cl.Subject, who)
		}
		fmt.Fprintf(s.out, "Signed in as %s.\n", who)
		return true, "signed in as " + who
	}
	creds, err := r.tokenSource(s.a.env.Now).Credentials(s.ctx)
	if err == nil {
		who := "unknown user"
		if cl, err := auth.DecodeClaims(creds.AccessToken); err == nil {
			who = firstNonEmpty(cl.Email, cl.Subject, who)
		}
		fmt.Fprintf(s.out, "Already signed in as %s.\n", who)
		return true, "signed in as " + who
	}
	if !errors.Is(err, auth.ErrLoginRequired) {
		return false, s.fail("%v", err)
	}
	fmt.Fprintln(s.out, "You are not signed in.")
	if !s.ask("Sign in now (opens your browser)?") {
		return false, "not signed in (declined)"
	}
	if err := s.a.login(s.ctx, s.o.noBrowser, auth.DefaultScope); err != nil {
		return false, s.fail("sign-in: %v", err)
	}
	return true, "signed in"
}

func (s *setupRun) scope(r *resolved, signedIn bool) string {
	if !signedIn {
		if s.o.dryRun {
			return "would ask brainsrv after sign-in"
		}
		return "unknown (not signed in)"
	}
	if s.o.dryRun {
		if r.cfg.Scope != "" {
			return "configured: " + r.cfg.Scope
		}
		resolver, _ := s.a.scopeResolver(r, "")
		if c := resolver.Cached(); c != "" {
			return "cached: " + c + " (would ask brainsrv again)"
		}
		return "would ask brainsrv /v1/whoami"
	}
	resolver, _ := s.a.scopeResolver(r, "")
	scope, src, err := resolver.Resolve(s.ctx)
	if err != nil {
		return s.fail("%v", err)
	}
	fmt.Fprintf(s.out, "Your member scope: %s (%s)\n", scope, src)
	return scope
}

func (s *setupRun) service(r *resolved) string {
	m, err := s.a.serviceManager()
	if err != nil {
		return s.fail("%v", err)
	}
	spec, err := s.a.serviceSpec(r, logFlags{}, "")
	if err != nil {
		return s.fail("%v", err)
	}
	st := m.Status()
	fmt.Fprintf(s.out, "%s %s runs: %s %s\n", m.Kind, m.Name(), spec.Program, strings.Join(spec.Args, " "))
	if s.o.dryRun {
		if st.Installed {
			return "installed; would update and restart it"
		}
		return "would install and start it"
	}
	if err := s.a.installService(s.ctx, logFlags{}, ""); err != nil {
		return s.fail("%v", err)
	}
	return "running"
}

func (s *setupRun) mcp(harnesses []string) string {
	if len(harnesses) == 0 {
		return "no agent to configure"
	}
	o := mcpOptions{harness: strings.Join(harnesses, ","), dryRun: true}
	var plan strings.Builder
	results, err := s.a.runMCPConfig(s.ctx, o, &plan)
	fmt.Fprint(s.out, plan.String())
	if err != nil {
		return s.fail("%v", err)
	}
	var todo []string
	for _, r := range results {
		if r.Changed() {
			todo = append(todo, r.Harness)
		}
	}
	if len(todo) == 0 {
		return "already configured"
	}
	if s.o.dryRun {
		return "would add the brain server for " + names(todo)
	}
	if !s.ask("Add the brain MCP server to these agents?") {
		return "not changed (declined)"
	}
	o.dryRun = false
	o.harness = strings.Join(todo, ",")
	if _, err := s.a.runMCPConfig(s.ctx, o, s.out); err != nil {
		return s.fail("%v", err)
	}
	return "brain server added for " + names(todo)
}

func (s *setupRun) sync(harnesses []string, home string) string {
	var hs []string
	for _, h := range harnesses {
		if (h == history.Claude || h == history.Codex) && history.Present(home, h) {
			hs = append(hs, h)
		}
	}
	if len(hs) == 0 {
		return "no Claude Code or Codex history found"
	}
	what := "all of it"
	if s.o.since > 0 {
		what = "sessions from the last " + s.o.since.String()
	}
	fmt.Fprintf(s.out, "Found session history from %s on this machine. afferent can send it to brainsrv (%s); sessions already read are never read twice.\n", names(hs), what)
	if s.o.dryRun {
		return "would offer to backfill " + names(hs)
	}
	if !s.ask("Backfill that history now?") {
		return "not now (run `afferent sync` later)"
	}
	if err := s.a.runSync(s.ctx, syncOptions{harness: strings.Join(hs, ","), since: s.o.since}, s.out); err != nil {
		return s.fail("%v", err)
	}
	return "backfilled " + names(hs)
}
