// Package cli is the afferent command tree. cmd/afferent/main.go only calls
// Execute; everything else lives here so tests can drive commands with fake
// servers, a fake credential store and a fake browser.
package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/afferent/auth"
	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/afferent/config"
	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/afferent/service"
	beaconauth "github.com/asymptote-labs/agent-beacon/cli/beacon/internal/auth"
)

// Env is everything the commands touch outside the process.
type Env struct {
	Stdout, Stderr io.Writer
	Getenv         func(string) string
	OpenBrowser    func(url string) error
	// NewStore builds the credential store; nil means auth.DefaultStore.
	NewStore func(dir string, cfg config.Config, warn func(error)) auth.Store
	HTTP     *http.Client
	Now      func() time.Time
	Sleep    func(ctx context.Context, d time.Duration) error

	// ResolveLog finds Beacon's runtime log; nil means the Beacon endpoint
	// configuration (lifecycle.ResolveRuntimeLog).
	ResolveLog func(userMode bool) string
	// Service builds the forwarder service manager; nil means the real
	// launchd/systemd one.
	Service func() (service.Manager, error)
	// Executable is this binary's path, for the service; nil means
	// os.Executable.
	Executable func() (string, error)
}

// commands are added to the root by init() in the command files, so each
// command group lives in its own file.
var commands []func(*app) *cobra.Command

func register(f func(*app) *cobra.Command) { commands = append(commands, f) }

// DefaultEnv is the real environment.
func DefaultEnv() *Env {
	return &Env{
		Stdout:      os.Stdout,
		Stderr:      os.Stderr,
		Getenv:      os.Getenv,
		OpenBrowser: beaconauth.OpenBrowser,
	}
}

type flags struct {
	configDir   string
	issuer      string
	issuerDial  string
	clientID    string
	brainsrvURL string
	context     string
}

type app struct {
	env   *Env
	flags flags
	root  *cobra.Command
}

// NewRootCmd builds the afferent command tree.
func NewRootCmd(env *Env) *cobra.Command {
	if env == nil {
		env = DefaultEnv()
	}
	if env.Getenv == nil {
		env.Getenv = os.Getenv
	}
	a := &app{env: env}
	root := &cobra.Command{
		Use:   "afferent",
		Short: "Sign in to authsrv and connect this machine's agent activity to brainsrv",
		Long: `afferent connects coding-agent activity on this machine to brainsrv.

It signs you in to authsrv with a browser (OAuth device flow), keeps the
tokens in the macOS Keychain (or a 0600 file elsewhere), and refreshes them
automatically.

Settings come from ~/.config/afferent/config.json, then AFFERENT_ISSUER,
AFFERENT_ISSUER_DIAL, AFFERENT_CLIENT_ID, AFFERENT_BRAINSRV_URL and
AFFERENT_CONTEXT, then the flags below.`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetOut(env.Stdout)
	root.SetErr(env.Stderr)
	pf := root.PersistentFlags()
	pf.StringVar(&a.flags.configDir, "config-dir", "", "config directory (default ~/.config/afferent; env AFFERENT_CONFIG_DIR)")
	pf.StringVar(&a.flags.issuer, "issuer", "", "authsrv issuer URL (env AFFERENT_ISSUER)")
	pf.StringVar(&a.flags.issuerDial, "issuer-dial", "", "base URL to reach the issuer at, if its hostname does not resolve here (env AFFERENT_ISSUER_DIAL)")
	pf.StringVar(&a.flags.clientID, "client-id", "", "OAuth client id (env AFFERENT_CLIENT_ID; default afferent-cli)")
	pf.StringVar(&a.flags.brainsrvURL, "brainsrv-url", "", "brainsrv base URL (env AFFERENT_BRAINSRV_URL)")
	pf.StringVar(&a.flags.context, "context", "", "brainsrv Context slug (env AFFERENT_CONTEXT)")
	a.root = root

	root.AddCommand(a.loginCmd(), a.logoutCmd(), a.whoamiCmd(), a.versionCmd())
	for _, f := range commands {
		root.AddCommand(f(a))
	}
	return root
}

// Execute runs the real CLI and returns the process exit code.
func Execute() int {
	env := DefaultEnv()
	root := NewRootCmd(env)
	if err := root.Execute(); err != nil {
		fmt.Fprintln(env.Stderr, "afferent:", err)
		return 1
	}
	return 0
}

// resolved is the effective configuration plus the pieces built from it.
type resolved struct {
	dir    string
	cfg    config.Config
	client *auth.Client
	store  auth.Store
	lock   string
}

func (a *app) resolve() (*resolved, error) {
	dir := a.flags.configDir
	if dir == "" {
		if d := a.env.Getenv(config.EnvConfigDir); d != "" {
			dir = d
		} else {
			var err error
			if dir, err = config.Dir(); err != nil {
				return nil, err
			}
		}
	}
	cfg, err := config.Load(dir)
	if err != nil {
		return nil, err
	}
	cfg.ApplyEnv(a.env.Getenv)
	f := a.root.PersistentFlags()
	override := func(name string, dst *string, v string) {
		if f.Changed(name) {
			*dst = v
		}
	}
	override("issuer", &cfg.Issuer, a.flags.issuer)
	override("issuer-dial", &cfg.IssuerDial, a.flags.issuerDial)
	override("client-id", &cfg.ClientID, a.flags.clientID)
	override("brainsrv-url", &cfg.BrainsrvURL, a.flags.brainsrvURL)
	override("context", &cfg.Context, a.flags.context)
	cfg.Normalize()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	client := auth.NewClient(cfg)
	client.HTTP, client.Now, client.Sleep = a.env.HTTP, a.env.Now, a.env.Sleep
	warn := func(err error) { fmt.Fprintln(a.env.Stderr, "warning:", err) }
	var store auth.Store
	if a.env.NewStore != nil {
		store = a.env.NewStore(dir, cfg, warn)
	} else {
		store = auth.DefaultStore(dir, cfg.Issuer, cfg.ClientID, warn)
	}
	return &resolved{dir: dir, cfg: cfg, client: client, store: store, lock: auth.LockFile(dir)}, nil
}

func (r *resolved) tokenSource(now func() time.Time) *auth.TokenSource {
	return &auth.TokenSource{Store: r.store, LockPath: r.lock, Client: r.client, Now: now}
}

func (a *app) now() time.Time {
	if a.env.Now != nil {
		return a.env.Now()
	}
	return time.Now()
}
