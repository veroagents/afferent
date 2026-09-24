package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/afferent/auth"
	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/afferent/config"
	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/afferent/forward"
	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/afferent/service"
)

func init() { register((*app).serviceCmd) }

func (a *app) serviceManager() (service.Manager, error) {
	if a.env.Service != nil {
		return a.env.Service()
	}
	return service.Default()
}

func (a *app) serviceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "service",
		Short: "Run the forwarder in the background (launchd on macOS, systemd --user on Linux)",
		Args:  cobra.NoArgs,
	}
	cmd.AddCommand(a.serviceInstallCmd(), a.serviceUninstallCmd(), a.serviceStatusCmd())
	return cmd
}

func (a *app) serviceInstallCmd() *cobra.Command {
	var (
		lf      logFlags
		program string
	)
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install and start `afferent forward` as a per-user service",
		Long: `install writes a launchd LaunchAgent (` + service.Label + `) on macOS, or a
systemd --user unit (` + service.SystemdUnit + `) on Linux, that runs
` + "`afferent forward`" + ` and restarts it if it stops. It also saves the current
settings to config.json, so the service uses the same brainsrv, Context and
issuer as this command. Running it again updates and restarts the service.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			r, err := a.resolve()
			if err != nil {
				return err
			}
			m, err := a.serviceManager()
			if err != nil {
				return err
			}
			if program == "" {
				exe := os.Executable
				if a.env.Executable != nil {
					exe = a.env.Executable
				}
				if program, err = exe(); err != nil {
					return fmt.Errorf("locate the afferent binary (use --program): %w", err)
				}
			}
			if program, err = filepath.Abs(program); err != nil {
				return err
			}
			stateDir := a.stateDir(r)
			args := []string{"forward", "--config-dir", r.dir}
			if lf.logPath != "" {
				args = append(args, "--log-path", a.runtimeLog(lf))
			} else if lf.system {
				args = append(args, "--system")
			}
			spec := service.Spec{
				Program: program,
				Args:    args,
				LogPath: filepath.Join(stateDir, forward.ServiceLogFile),
			}
			if d := a.env.Getenv(config.EnvStateDir); d != "" {
				spec.Env = map[string]string{config.EnvStateDir: d}
			}
			// The service reads config.json, not this shell's environment.
			if err := config.Save(r.dir, r.cfg); err != nil {
				return fmt.Errorf("save settings for the service: %w", err)
			}
			if _, err := r.tokenSource(a.env.Now).Credentials(cmd.Context()); errors.Is(err, auth.ErrLoginRequired) {
				fmt.Fprintln(a.env.Stderr, "Note: you are not signed in. The forwarder will wait until you run `afferent login`.")
			}
			path, err := m.Install(spec)
			if err != nil {
				return err
			}
			out := a.env.Stdout
			fmt.Fprintf(out, "Installed %s (%s) at %s.\n", m.Name(), m.Kind, path)
			fmt.Fprintf(out, "It runs: %s %s\n", program, strings.Join(args, " "))
			if m.Kind == service.KindSystemd {
				fmt.Fprintf(out, "Logs:    journalctl --user -u %s\n", service.SystemdUnit)
			} else {
				fmt.Fprintf(out, "Logs:    %s\n", spec.LogPath)
			}
			fmt.Fprintln(out, "Check it with `afferent status`.")
			return nil
		},
	}
	lf.add(cmd)
	cmd.Flags().StringVar(&program, "program", "", "afferent binary the service runs (default: this binary)")
	return cmd
}

func (a *app) serviceUninstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall",
		Short: "Stop and remove the forwarder service (checkpoints are kept)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			m, err := a.serviceManager()
			if err != nil {
				return err
			}
			if err := m.Uninstall(); err != nil {
				return err
			}
			fmt.Fprintf(a.env.Stdout, "Removed %s. Checkpoints stay, so a later install resumes where it stopped.\n", m.Name())
			return nil
		},
	}
}

func (a *app) serviceStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show whether the forwarder service is installed and running",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			m, err := a.serviceManager()
			if err != nil {
				return err
			}
			printServiceStatus(a.env.Stdout, m.Status())
			return nil
		},
	}
}

func printServiceStatus(out interface{ Write([]byte) (int, error) }, s service.Status) {
	switch {
	case !s.Installed && !s.State.Loaded:
		fmt.Fprintf(out, "Service:    not installed (run `afferent service install`)\n")
		return
	case s.State.Running:
		fmt.Fprintf(out, "Service:    running (%s %s, pid %d)\n", s.Kind, s.Name, s.State.PID)
	case s.State.Loaded:
		fmt.Fprintf(out, "Service:    loaded but not running (%s %s: %s)\n", s.Kind, s.Name, s.State.Detail)
	default:
		fmt.Fprintf(out, "Service:    installed but not loaded (%s)\n", s.UnitPath)
	}
	if s.Err != nil {
		fmt.Fprintf(out, "            %v\n", s.Err)
	}
	fmt.Fprintf(out, "Unit:       %s\n", s.UnitPath)
}
