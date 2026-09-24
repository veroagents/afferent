package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/afferent/auth"
	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/afferent/brain"
	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/afferent/forward"
)

func init() { register((*app).statusCmd) }

func (a *app) statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show sign-in, scope, forwarder service and delivery status",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			r, err := a.resolve()
			if err != nil {
				return err
			}
			out := a.env.Stdout
			now := a.now()

			// Sign-in.
			signedIn := false
			ts := r.tokenSource(a.env.Now)
			creds, err := ts.Credentials(ctx)
			switch {
			case errors.Is(err, auth.ErrLoginRequired):
				fmt.Fprintln(out, "Signed in:  no (run `afferent login`)")
			case err != nil:
				fmt.Fprintf(out, "Signed in:  unknown (%v)\n", err)
			default:
				signedIn = true
				who := "unknown user"
				var exp time.Time
				if c, err := auth.DecodeClaims(creds.AccessToken); err == nil {
					who = firstNonEmpty(c.Email, c.Subject, who)
					exp = c.Expiry()
				}
				line := fmt.Sprintf("Signed in:  %s at %s", who, creds.Issuer)
				if !exp.IsZero() {
					line += fmt.Sprintf(" (token valid %s more)", exp.Sub(now).Round(time.Second))
				}
				fmt.Fprintln(out, line)
			}

			// Identity and scope.
			fmt.Fprintf(out, "brainsrv:   %s (context %s)\n", r.cfg.BrainsrvURL, r.cfg.Context)
			resolver, bc := a.scopeResolver(r, "")
			switch {
			case resolver.Override != "":
				fmt.Fprintf(out, "Scope:      %s (configured)\n", resolver.Override)
			case !signedIn:
				if c := resolver.Cached(); c != "" {
					fmt.Fprintf(out, "Scope:      %s (cached)\n", c)
				} else {
					fmt.Fprintln(out, "Scope:      unknown until you sign in")
				}
			default:
				printScope(out, bc, resolver, cmd)
			}

			// Service.
			if m, err := a.serviceManager(); err != nil {
				fmt.Fprintf(out, "Service:    unavailable (%v)\n", err)
			} else {
				printServiceStatus(out, m.Status())
			}

			// Forwarder.
			stateDir := a.stateDir(r)
			s, err := forward.ReadStatus(stateDir)
			if errors.Is(err, os.ErrNotExist) {
				fmt.Fprintf(out, "Forwarder:  has not run yet (state in %s)\n", stateDir)
				return nil
			}
			if err != nil {
				fmt.Fprintf(out, "Forwarder:  unreadable status: %v\n", err)
				return nil
			}
			printForwarder(out, s, now)
			return nil
		},
	}
}

func printScope(out io.Writer, bc *brain.Client, resolver *forward.ScopeResolver, cmd *cobra.Command) {
	w, err := bc.Whoami(cmd.Context())
	if err != nil {
		if c := resolver.Cached(); c != "" {
			fmt.Fprintf(out, "Scope:      %s (cached; brainsrv: %v)\n", c, err)
		} else {
			fmt.Fprintf(out, "Scope:      unknown (brainsrv: %v)\n", err)
		}
		return
	}
	printIf(out, "Principal:", w.PrincipalID)
	scope, err := forward.MemberScope(w)
	if err != nil {
		fmt.Fprintf(out, "Scope:      none: %v\n", err)
		return
	}
	fmt.Fprintf(out, "Scope:      %s\n", scope)
}

func printForwarder(out io.Writer, s *forward.Status, now time.Time) {
	ago := func(t time.Time) string {
		if t.IsZero() {
			return "never"
		}
		return fmt.Sprintf("%s (%s ago)", t.Local().Format(time.RFC3339), now.Sub(t).Round(time.Second))
	}
	state := s.State
	if s.PausedReason != "" {
		state += ": " + s.PausedReason
	}
	fmt.Fprintf(out, "Forwarder:  %s (pid %d, status updated %s)\n", state, s.PID, ago(s.UpdatedAt))
	fmt.Fprintf(out, "Log:        %s", s.LogPath)
	if !s.LogFound {
		fmt.Fprint(out, " (not found; is Beacon capture installed?)")
	}
	fmt.Fprintln(out)
	if s.Scope != "" {
		fmt.Fprintf(out, "Sending as: %s\n", s.Scope)
	}
	fmt.Fprintf(out, "Last sent:  %s\n", ago(s.LastSuccessAt))
	if s.LastError != "" {
		fmt.Fprintf(out, "Last error: %s at %s\n", s.LastError, ago(s.LastErrorAt))
	}
	if !s.NextRetryAt.IsZero() && s.State != forward.StateStopped {
		fmt.Fprintf(out, "Next retry: %s\n", s.NextRetryAt.Local().Format(time.RFC3339))
	}
	fmt.Fprintf(out, "Totals:     %d lines in %d batches (%d bytes); brainsrv accepted %d, duplicate %d, rejected %d; skipped %d\n",
		s.LinesSent, s.Batches, s.BytesSent, s.Accepted, s.Duplicate, s.Rejected, s.LinesSkipped)
	fmt.Fprintf(out, "Lag:        %d bytes behind\n", s.LagBytes)
	for _, l := range s.Lag {
		if l.Behind > 0 {
			fmt.Fprintf(out, "            %s: %d of %d bytes behind\n", l.Path, l.Behind, l.Size)
		}
	}
}
