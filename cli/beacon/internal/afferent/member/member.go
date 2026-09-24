// Package member finds the signed-in afferent member outside the afferent
// command tree: the settings (config.json plus AFFERENT_* env), the stored
// credentials with a refreshing TokenSource, and the member base scope (the
// same resolution the forwarder uses: configured, else brainsrv /v1/whoami,
// cached in the state directory).
//
// Beacon's memory backend uses it to talk to brainsrv with afferent's
// authsrv token instead of an spk_ key file (PLAN D7).
package member

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/afferent/auth"
	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/afferent/brain"
	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/afferent/config"
	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/afferent/forward"
)

// ErrNotSignedIn means there are no stored afferent credentials.
var ErrNotSignedIn = errors.New("not signed in to afferent")

// Options locate the afferent settings. Zero values use the real
// environment.
type Options struct {
	// ConfigDir overrides AFFERENT_CONFIG_DIR and the default directory.
	ConfigDir string
	Getenv    func(string) string
	// NewStore builds the credential store; nil means auth.DefaultStore
	// (the macOS Keychain with a file fallback, or the file).
	NewStore func(dir string, cfg config.Config) auth.Store
	HTTP     *http.Client
	Now      func() time.Time
}

// Session is a signed-in member's brainsrv connection settings.
type Session struct {
	Dir    string
	Config config.Config
	Tokens *auth.TokenSource
	Scopes *forward.ScopeResolver
	Brain  *brain.Client
}

// Open loads the settings and checks that credentials are stored. It does
// not refresh the token or call any server. It returns ErrNotSignedIn when
// nothing is stored.
func Open(opts Options) (*Session, error) {
	getenv := opts.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	dir := opts.ConfigDir
	if dir == "" {
		dir = strings.TrimSpace(getenv(config.EnvConfigDir))
	}
	if dir == "" {
		var err error
		if dir, err = config.Dir(); err != nil {
			return nil, err
		}
	}
	cfg, err := config.Load(dir)
	if err != nil {
		return nil, err
	}
	cfg.ApplyEnv(getenv)
	cfg.Normalize()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	var store auth.Store
	if opts.NewStore != nil {
		store = opts.NewStore(dir, cfg)
	} else {
		store = auth.DefaultStore(dir, cfg.Issuer, cfg.ClientID, func(error) {})
	}
	if _, err := store.Load(); err != nil {
		if errors.Is(err, auth.ErrNotFound) {
			return nil, ErrNotSignedIn
		}
		return nil, fmt.Errorf("read afferent credentials: %w", err)
	}
	client := auth.NewClient(cfg)
	client.HTTP = opts.HTTP
	ts := &auth.TokenSource{Store: store, LockPath: auth.LockFile(dir), Client: client, Now: opts.Now}
	bc := &brain.Client{BaseURL: cfg.BrainsrvURL, Context: cfg.Context, HTTP: opts.HTTP, Token: ts.Token}
	return &Session{
		Dir:    dir,
		Config: cfg,
		Tokens: ts,
		Brain:  bc,
		Scopes: &forward.ScopeResolver{
			Override: cfg.Scope,
			StateDir: config.StateDir(dir, getenv),
			URL:      cfg.BrainsrvURL,
			Context:  cfg.Context,
			Whoami:   bc.Whoami,
			Now:      opts.Now,
		},
	}, nil
}

// Scope returns the member base scope without a network call when it can:
// the configured scope, else the forwarder's cached answer, else brainsrv
// /v1/whoami (which caches it).
func (s *Session) Scope(ctx context.Context) (string, error) {
	if v := strings.TrimSpace(s.Scopes.Override); v != "" {
		return v, nil
	}
	if v := s.Scopes.Cached(); v != "" {
		return v, nil
	}
	v, _, err := s.Scopes.Resolve(ctx)
	return v, err
}
