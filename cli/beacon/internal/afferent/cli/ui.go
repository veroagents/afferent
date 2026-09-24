package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/afferent/auth"
	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/afferent/forward"
	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/afferent/ui"
)

func init() { register((*app).uiCmd) }

func (a *app) uiCmd() *cobra.Command {
	var (
		addr      string
		port      int
		noBrowser bool
	)
	cmd := &cobra.Command{
		Use:   "ui",
		Short: "Open a local web page that shows your brain: scope map, knowledge graph, search and status",
		Long: `ui serves a page on a loopback address and opens it in the browser. The page
shows a map of your scopes (repos and harnesses) sized by turns, the
knowledge graph brainsrv built from them, recall search, sessions and turns,
and the forwarder's status.

The page talks only to this process, which calls brainsrv as you. Your token
never reaches the browser. The link carries a random key for this launch
only; other local programs and web sites cannot use the page's API. The
server runs until Ctrl-C.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if _, err := ui.LoopbackHost(addr); err != nil {
				return err
			}
			r, err := a.resolve()
			if err != nil {
				return err
			}
			ts := r.tokenSource(a.env.Now)
			resolver, _ := a.scopeResolver(r, "")
			srv, err := ui.New(ui.Config{
				BrainsrvURL:  r.cfg.BrainsrvURL,
				Context:      r.cfg.Context,
				HTTP:         a.env.HTTP,
				Token:        ts.Token,
				ForceRefresh: ts.ForceRefresh,
				Scope: func(ctx context.Context) (string, error) {
					sc, _, err := resolver.Resolve(ctx)
					return sc, err
				},
				Status: func(ctx context.Context) any { return a.uiStatus(ctx, r, ts, resolver) },
			}, addr, port)
			if err != nil {
				return err
			}
			url := srv.URL()
			fmt.Fprintf(a.env.Stdout, "afferent ui: %s\n", url)
			fmt.Fprintln(a.env.Stdout, "The link carries a key for this launch; keep it to yourself. Ctrl-C stops the server.")
			if !noBrowser && a.env.OpenBrowser != nil {
				if err := a.env.OpenBrowser(url); err != nil {
					fmt.Fprintf(a.env.Stderr, "warning: could not open a browser (%v); open the link above\n", err)
				}
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return srv.Serve(ctx)
		},
	}
	f := cmd.Flags()
	f.StringVar(&addr, "addr", "127.0.0.1", "loopback address to listen on (127.0.0.1, ::1 or localhost)")
	f.IntVar(&port, "port", 0, "port to listen on (0 picks a free one)")
	f.BoolVar(&noBrowser, "no-browser", false, "print the link instead of opening a browser")
	return cmd
}

// uiStatus is GET /api/status: what `afferent status` prints, as JSON. It
// never carries a token.
type uiStatus struct {
	SignedIn          bool       `json:"signed_in"`
	SignInError       string     `json:"sign_in_error,omitempty"`
	Identity          string     `json:"identity,omitempty"`
	Subject           string     `json:"subject,omitempty"`
	Issuer            string     `json:"issuer,omitempty"`
	TokenExpiresAt    *time.Time `json:"token_expires_at,omitempty"`
	TokenValidSeconds int64      `json:"token_valid_seconds,omitempty"`

	BrainsrvURL string `json:"brainsrv_url"`
	Context     string `json:"context"`
	Principal   string `json:"principal,omitempty"`
	Scope       string `json:"scope,omitempty"`
	ScopeSource string `json:"scope_source,omitempty"`
	ScopeError  string `json:"scope_error,omitempty"`

	Service        uiService       `json:"service"`
	Forwarder      *forward.Status `json:"forwarder,omitempty"`
	ForwarderError string          `json:"forwarder_error,omitempty"`
	StateDir       string          `json:"state_dir"`
	Now            time.Time       `json:"now"`
}

type uiService struct {
	Available bool   `json:"available"`
	Installed bool   `json:"installed"`
	Loaded    bool   `json:"loaded"`
	Running   bool   `json:"running"`
	PID       int    `json:"pid,omitempty"`
	Kind      string `json:"kind,omitempty"`
	Name      string `json:"name,omitempty"`
	Detail    string `json:"detail,omitempty"`
	Error     string `json:"error,omitempty"`
}

func (a *app) uiStatus(ctx context.Context, r *resolved, ts *auth.TokenSource, resolver *forward.ScopeResolver) *uiStatus {
	now := a.now()
	st := &uiStatus{BrainsrvURL: r.cfg.BrainsrvURL, Context: r.cfg.Context, StateDir: a.stateDir(r), Now: now}

	creds, err := ts.Credentials(ctx)
	switch {
	case errors.Is(err, auth.ErrLoginRequired):
		st.SignInError = "not signed in; run `afferent login`"
	case err != nil:
		st.SignInError = err.Error()
	default:
		st.SignedIn = true
		st.Issuer = creds.Issuer
		if c, err := auth.DecodeClaims(creds.AccessToken); err == nil {
			st.Identity = firstNonEmpty(c.Email, c.Subject)
			st.Subject = c.Subject
			if exp := c.Expiry(); !exp.IsZero() {
				st.TokenExpiresAt = &exp
				st.TokenValidSeconds = int64(exp.Sub(now) / time.Second)
			}
		}
	}

	switch {
	case resolver.Override != "":
		st.Scope, st.ScopeSource = resolver.Override, "configured"
	case !st.SignedIn:
		if c := resolver.Cached(); c != "" {
			st.Scope, st.ScopeSource = c, "cached"
		}
	default:
		sc, src, err := resolver.Resolve(ctx)
		if err != nil {
			st.ScopeError = err.Error()
			if c := resolver.Cached(); c != "" {
				st.Scope, st.ScopeSource = c, "cached"
			}
		} else {
			st.Scope, st.ScopeSource = sc, src
		}
	}

	if m, err := a.serviceManager(); err != nil {
		st.Service.Error = err.Error()
	} else {
		s := m.Status()
		st.Service = uiService{
			Available: true, Installed: s.Installed, Loaded: s.State.Loaded, Running: s.State.Running,
			PID: s.State.PID, Kind: string(s.Kind), Name: s.Name, Detail: s.State.Detail,
		}
		if s.Err != nil {
			st.Service.Error = s.Err.Error()
		}
	}

	fs, err := forward.ReadStatus(st.StateDir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		st.ForwarderError = "the forwarder has not run yet"
	case err != nil:
		st.ForwarderError = err.Error()
	default:
		st.Forwarder = fs
	}
	return st
}
