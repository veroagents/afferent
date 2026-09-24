package auth

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/afferent/config"
)

// ErrLoginRequired means there is no usable session; the user must run
// `afferent login`.
var ErrLoginRequired = errors.New("not signed in; run `afferent login`")

// DefaultSkew is how long before expiry a token is refreshed.
const DefaultSkew = 60 * time.Second

// LockFile returns the cross-process lock path inside the config dir.
func LockFile(dir string) string { return filepath.Join(dir, "credentials.lock") }

// DefaultStore returns the Keychain (with file fallback) on macOS and the
// 0600 file elsewhere.
func DefaultStore(dir, issuer, clientID string, warn func(error)) Store {
	file := &FileStore{Path: CredentialsFile(dir)}
	if runtime.GOOS != "darwin" {
		return file
	}
	return &FallbackStore{
		Primary:   &KeychainStore{Account: KeychainAccount(issuer, clientID)},
		Secondary: file,
		Warn:      warn,
	}
}

// TokenSource hands out valid access tokens, refreshing them when they are
// within Skew of expiry.
//
// Refresh tokens rotate on use, so two refreshers racing with the same
// refresh token would leave one of them holding a revoked token (and trip
// reuse detection). Refresh therefore happens under an exclusive lock on
// LockPath, and after taking it the source reloads the stored credentials:
// if another process (the forwarder, another CLI command) already refreshed,
// it uses that result instead of refreshing again.
type TokenSource struct {
	Store    Store
	LockPath string
	Client   *Client
	Skew     time.Duration
	Now      func() time.Time

	mu sync.Mutex
	ep *Endpoints
}

func (ts *TokenSource) now() time.Time {
	if ts.Now != nil {
		return ts.Now()
	}
	return time.Now()
}

func (ts *TokenSource) skew() time.Duration {
	if ts.Skew > 0 {
		return ts.Skew
	}
	return DefaultSkew
}

// Token returns a valid access token.
func (ts *TokenSource) Token(ctx context.Context) (string, error) {
	c, err := ts.Credentials(ctx)
	if err != nil {
		return "", err
	}
	return c.AccessToken, nil
}

// Credentials returns credentials whose access token is valid for at least
// Skew, refreshing if needed.
func (ts *TokenSource) Credentials(ctx context.Context) (*Credentials, error) {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	c, err := ts.load()
	if err != nil {
		return nil, err
	}
	if c.ValidFor(ts.now(), ts.skew()) {
		return c, nil
	}
	if err := config.EnsureDir(filepath.Dir(ts.LockPath)); err != nil {
		return nil, err
	}
	unlock, err := lockFile(ctx, ts.LockPath)
	if err != nil {
		return nil, err
	}
	defer unlock()

	// Someone else may have refreshed while we waited for the lock.
	c, err = ts.load()
	if err != nil {
		return nil, err
	}
	if c.ValidFor(ts.now(), ts.skew()) {
		return c, nil
	}
	if c.RefreshToken == "" {
		return nil, fmt.Errorf("session expired and no refresh token is stored: %w", ErrLoginRequired)
	}
	ep, err := ts.endpoints(ctx)
	if err != nil {
		return nil, err
	}
	tr, err := ts.Client.Refresh(ctx, ep, c.RefreshToken)
	if err != nil {
		var oe *OAuthError
		if errors.As(err, &oe) {
			switch oe.Code {
			case "invalid_grant":
				// The refresh token is revoked, expired or already used.
				_ = ts.Store.Delete()
				return nil, fmt.Errorf("session ended (%s): %w", oe, ErrLoginRequired)
			case "unsupported_grant_type", "unauthorized_client":
				return nil, fmt.Errorf("session expired and the server cannot refresh it (%s): %w", oe, ErrLoginRequired)
			}
		}
		return nil, fmt.Errorf("refresh access token: %w", err)
	}
	nc, err := NewCredentials(tr, c.Issuer, c.ClientID, c.RefreshToken, ts.now())
	if err != nil {
		return nil, err
	}
	if err := ts.Store.Save(nc); err != nil {
		// The old refresh token is likely spent; losing the new one means
		// the next call will need a login. Say so rather than hide it.
		return nil, fmt.Errorf("save refreshed credentials: %w", err)
	}
	return nc, nil
}

func (ts *TokenSource) load() (*Credentials, error) {
	c, err := ts.Store.Load()
	if errors.Is(err, ErrNotFound) {
		return nil, ErrLoginRequired
	}
	return c, err
}

func (ts *TokenSource) endpoints(ctx context.Context) (*Endpoints, error) {
	if ts.ep != nil {
		return ts.ep, nil
	}
	ep, err := ts.Client.Discover(ctx)
	if err != nil {
		return nil, err
	}
	ts.ep = ep
	return ep, nil
}

// SaveLocked stores credentials under the same lock refresh uses, so a login
// never interleaves with a refresh in another process.
func SaveLocked(ctx context.Context, store Store, lockPath string, c *Credentials) error {
	if err := config.EnsureDir(filepath.Dir(lockPath)); err != nil {
		return err
	}
	unlock, err := lockFile(ctx, lockPath)
	if err != nil {
		return err
	}
	defer unlock()
	return store.Save(c)
}

// DeleteLocked removes credentials under the refresh lock.
func DeleteLocked(ctx context.Context, store Store, lockPath string) error {
	if err := config.EnsureDir(filepath.Dir(lockPath)); err != nil {
		return err
	}
	unlock, err := lockFile(ctx, lockPath)
	if err != nil {
		return err
	}
	defer unlock()
	return store.Delete()
}
