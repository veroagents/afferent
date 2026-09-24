package cli

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/afferent/auth"
	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/afferent/brain"
	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/afferent/config"
	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/version"
)

func (a *app) loginCmd() *cobra.Command {
	var noBrowser bool
	var scope string
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Sign in with your browser (OAuth device flow)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.login(cmd.Context(), noBrowser, scope)
		},
	}
	cmd.Flags().BoolVar(&noBrowser, "no-browser", false, "print the sign-in link instead of opening a browser")
	cmd.Flags().StringVar(&scope, "scope", auth.DefaultScope, "OAuth scopes to request")
	return cmd
}

// login runs the device flow and stores the credentials.
func (a *app) login(ctx context.Context, noBrowser bool, scope string) error {
	r, err := a.resolve()
	if err != nil {
		return err
	}
	out := a.env.Stdout
	ep, err := r.client.Discover(ctx)
	if err != nil {
		return err
	}
	dc, err := r.client.RequestDeviceCode(ctx, ep, scope)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "To sign in, open %s\nand enter the code: %s\n\n", dc.VerificationURI, dc.UserCode)
	if u := dc.BrowserURL(); u != "" && !noBrowser && a.env.OpenBrowser != nil {
		if err := a.env.OpenBrowser(u); err != nil {
			fmt.Fprintf(a.env.Stderr, "Could not open a browser (%v); open the link above yourself.\n", err)
		} else {
			fmt.Fprintln(out, "Opened your browser. Waiting for approval…")
		}
	} else {
		fmt.Fprintln(out, "Waiting for approval…")
	}
	tr, err := r.client.PollToken(ctx, ep, dc)
	if err != nil {
		return err
	}
	creds, err := auth.NewCredentials(tr, ep.Issuer, r.cfg.ClientID, "", a.now())
	if err != nil {
		return err
	}
	// Read the previous session under the same lock as the write, so
	// a refresh in another process cannot rotate it in between.
	old, err := auth.SwapLocked(ctx, r.store, r.lock, creds)
	if err != nil {
		return fmt.Errorf("store credentials: %w", err)
	}
	// End the previous session on this machine, if any. Only send its
	// refresh token to the issuer and client that issued it.
	if old != nil && old.RefreshToken != "" && old.RefreshToken != creds.RefreshToken {
		if old.Matches(ep.Issuer, r.cfg.ClientID) {
			_ = r.client.Revoke(ctx, ep, old.RefreshToken, "refresh_token")
		} else {
			fmt.Fprintf(a.env.Stderr, "warning: replaced a session for %s (client %s) without revoking it; it stays valid until it expires\n", old.Issuer, old.ClientID)
		}
	}
	// Remember the settings this login used, so later commands (and
	// the forwarder) find these credentials without the same flags.
	if err := config.Save(r.dir, r.cfg); err != nil {
		fmt.Fprintf(a.env.Stderr, "warning: could not save %s: %v\n", config.Path(r.dir), err)
	}
	who := "unknown user"
	if c, err := auth.DecodeClaims(creds.AccessToken); err == nil {
		who = firstNonEmpty(c.Email, c.Subject, who)
	}
	fmt.Fprintf(out, "Signed in to %s as %s. Credentials stored in %s.\n", ep.Issuer, who, r.store.Name())
	if creds.RefreshToken == "" {
		fmt.Fprintln(out, "Note: the server issued no refresh token; you will need to log in again when this token expires.")
	}
	return nil
}

func (a *app) logoutCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Revoke this machine's session and delete stored credentials",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			r, err := a.resolve()
			if err != nil {
				return err
			}
			// Take the credentials and delete them under the refresh lock, so
			// the refresh token revoked below is the current one.
			creds, lerr, derr := auth.RemoveLocked(ctx, r.store, r.lock)
			if errors.Is(lerr, auth.ErrNotFound) && (derr == nil || errors.Is(derr, auth.ErrNotFound)) {
				fmt.Fprintln(a.env.Stdout, "Not signed in.")
				return nil
			}
			if lerr != nil && !errors.Is(lerr, auth.ErrNotFound) {
				fmt.Fprintf(a.env.Stderr, "warning: could not read stored credentials: %v\n", lerr)
			}
			if creds != nil && creds.RefreshToken != "" {
				ep, discErr := r.client.Discover(ctx)
				switch {
				case discErr != nil:
					fmt.Fprintf(a.env.Stderr, "warning: could not revoke the session on the server: %v\n", discErr)
				case !creds.Matches(ep.Issuer, r.cfg.ClientID):
					// Never send a refresh token to an issuer or client that
					// did not issue it.
					fmt.Fprintf(a.env.Stderr, "warning: the stored session belongs to %s (client %s), not %s (client %s); not revoking it on the server\n", creds.Issuer, creds.ClientID, ep.Issuer, r.cfg.ClientID)
				default:
					if rerr := r.client.Revoke(ctx, ep, creds.RefreshToken, "refresh_token"); rerr != nil {
						fmt.Fprintf(a.env.Stderr, "warning: could not revoke the session on the server: %v\n", rerr)
					}
				}
			}
			if derr != nil && !errors.Is(derr, auth.ErrNotFound) {
				return fmt.Errorf("delete credentials: %w", derr)
			}
			fmt.Fprintln(a.env.Stdout, "Signed out.")
			return nil
		},
	}
}

func (a *app) whoamiCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "Show who you are signed in as and your brainsrv scopes",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			r, err := a.resolve()
			if err != nil {
				return err
			}
			out := a.env.Stdout
			ts := r.tokenSource(a.env.Now)
			creds, err := ts.Credentials(ctx)
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "Issuer:     %s\n", creds.Issuer)
			if c, err := auth.DecodeClaims(creds.AccessToken); err == nil {
				printIf(out, "Subject:", c.Subject)
				printIf(out, "Email:", c.Email)
				printIf(out, "Tenant:", c.TenantID)
				printIf(out, "Account:", c.AccountID)
				if len(c.Audience) > 0 {
					printIf(out, "Audience:", strings.Join(c.Audience, ", "))
				}
				if exp := c.Expiry(); !exp.IsZero() {
					fmt.Fprintf(out, "Expires:    %s (in %s)\n", exp.Local().Format(time.RFC3339), exp.Sub(a.now()).Round(time.Second))
				}
			} else {
				fmt.Fprintf(out, "Token:      opaque (%v)\n", err)
			}

			bc := &brain.Client{BaseURL: r.cfg.BrainsrvURL, Context: r.cfg.Context, HTTP: a.env.HTTP, Token: ts.Token}
			fmt.Fprintf(out, "\nbrainsrv:   %s (context %s)\n", r.cfg.BrainsrvURL, r.cfg.Context)
			w, err := bc.Whoami(ctx)
			if errors.Is(err, brain.ErrWhoamiUnsupported) {
				fmt.Fprintln(out, "Scopes:     unknown (this brainsrv has no /v1/whoami; it predates member scopes)")
				return nil
			}
			if err != nil {
				return fmt.Errorf("brainsrv whoami: %w", err)
			}
			printIf(out, "Principal:", w.PrincipalID)
			printIf(out, "Kind:", w.Kind)
			printIf(out, "Context:", w.Context)
			if w.Subject != "" && w.Subject != w.PrincipalID {
				printIf(out, "As:", w.Subject)
			}
			if len(w.Grants) == 0 {
				fmt.Fprintln(out, "Scopes:     none (this identity has no grant in this Context)")
				return nil
			}
			fmt.Fprintln(out, "Scopes:")
			grants := append([]brain.Grant(nil), w.Grants...)
			sort.Slice(grants, func(i, j int) bool { return grants[i].Scope < grants[j].Scope })
			for _, g := range grants {
				line := fmt.Sprintf("  %s  [%s]", g.Scope, strings.Join(g.Verbs, ","))
				if g.TemplateID != "" {
					line += "  via template " + g.TemplateID
				}
				fmt.Fprintln(out, line)
			}
			return nil
		},
	}
}

func printIf(out interface{ Write([]byte) (int, error) }, label, v string) {
	if v != "" {
		fmt.Fprintf(out, "%-11s %s\n", label, v)
	}
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

func (a *app) versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the afferent version",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, _ []string) {
			fmt.Fprintln(a.env.Stdout, "afferent", version.GetFullVersion())
		},
	}
}
