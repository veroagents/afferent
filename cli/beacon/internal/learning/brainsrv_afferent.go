package learning

// brainsrv_afferent.go lets the brainsrv memory backend authenticate with the
// signed-in afferent member's authsrv token (PLAN D7) instead of an spk_ key
// file:
//
//   - Authorization: Bearer <access JWT>, refreshed by afferent's
//     TokenSource, and X-Context: <the afferent Context>;
//   - the base scope is the member scope (afferent config "scope", else the
//     forwarder's cached /v1/whoami answer, else /v1/whoami).
//
// Selection (configuredBackend):
//
//	BEACON_MEMORY_BACKEND=local|off|none   local SQLite only
//	BEACON_BRAINSRV_KEY_FILE set           the key-file path, unchanged
//	                                       (needs BEACON_MEMORY_BACKEND=brainsrv)
//	otherwise, signed in to afferent       afferent credentials
//	otherwise                              local only (a warning when
//	                                       BEACON_MEMORY_BACKEND=brainsrv)
//
// BEACON_BRAINSRV_URL and BEACON_BRAINSRV_SCOPE, when set, still override the
// URL and the base scope on the afferent path.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/afferent/member"
	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/brainsrvcfg"
)

// TokenSource hands out bearer tokens for brainsrv. afferent's
// auth.TokenSource implements it.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
	// ForceRefresh returns a token other than rejected (after a 401).
	ForceRefresh(ctx context.Context, rejected string) (string, error)
}

// NewBrainsrvBackendWithTokens returns a backend that authenticates with
// tokens and sends X-Context: contextSlug, instead of an spk_ key.
func NewBrainsrvBackendWithTokens(storePath string, cfg brainsrvcfg.Config, contextSlug string, tokens TokenSource, client *http.Client) *BrainsrvBackend {
	b := NewBrainsrvBackend(storePath, cfg, "", client)
	b.tokens, b.xContext = tokens, contextSlug
	return b
}

// AuthKind reports how the backend authenticates: "afferent" (authsrv token)
// or "key" (spk_ key file).
func (b *BrainsrvBackend) AuthKind() string {
	if b.tokens != nil {
		return "afferent"
	}
	return "key"
}

// send authorizes and sends one request built by newReq. With afferent
// tokens, a 401 forces one refresh and the request is sent again.
func (b *BrainsrvBackend) send(ctx context.Context, newReq func() (*http.Request, error)) (*http.Response, error) {
	req, err := newReq()
	if err != nil {
		return nil, err
	}
	if b.tokens == nil {
		req.Header.Set("Authorization", "Bearer "+b.key)
		return b.client.Do(req)
	}
	tok, err := b.tokens.Token(ctx)
	if err != nil {
		return nil, fmt.Errorf("afferent token: %w", err)
	}
	b.authorize(req, tok)
	resp, err := b.client.Do(req)
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		return resp, err
	}
	resp.Body.Close()
	tok, err = b.tokens.ForceRefresh(ctx, tok)
	if err != nil {
		return nil, fmt.Errorf("afferent token refresh after a 401: %w", err)
	}
	if req, err = newReq(); err != nil {
		return nil, err
	}
	b.authorize(req, tok)
	return b.client.Do(req)
}

func (b *BrainsrvBackend) authorize(req *http.Request, tok string) {
	req.Header.Set("Authorization", "Bearer "+tok)
	if b.xContext != "" {
		req.Header.Set("X-Context", b.xContext)
	}
}

// configuredBackend picks the memory backend from the environment and the
// afferent sign-in, or returns nil for local SQLite only.
func configuredBackend(path string, getenv func(string) string) MemoryBackend {
	mode := strings.ToLower(strings.TrimSpace(getenv(brainsrvcfg.EnvBackend)))
	switch mode {
	case "local", "off", "none", "sqlite":
		return nil
	}
	if strings.TrimSpace(getenv(brainsrvcfg.EnvKeyFile)) != "" || (mode != "" && mode != brainsrvcfg.BackendBrainsrv) {
		return keyFileBackend(path, getenv)
	}
	b, err := afferentBackend(path, getenv)
	if err != nil {
		if !errors.Is(err, member.ErrNotSignedIn) || mode == brainsrvcfg.BackendBrainsrv {
			warnConfigOnce(err)
		}
		return nil
	}
	return b
}

// keyFileBackend is the spk_ key-file configuration (PLAN Phase 2).
func keyFileBackend(path string, getenv func(string) string) MemoryBackend {
	cfg, err := brainsrvcfg.Load(getenv)
	if errors.Is(err, brainsrvcfg.ErrNotConfigured) {
		return nil
	}
	if err != nil {
		warnConfigOnce(err)
		return nil
	}
	key, err := brainsrvcfg.ReadKeyFile(cfg.KeyFile)
	if err != nil {
		warnConfigOnce(fmt.Errorf("%s: %w", brainsrvcfg.EnvKeyFile, err))
		return nil
	}
	return NewBrainsrvBackend(path, cfg, key, nil)
}

// afferentBackend builds a backend on the signed-in afferent member.
func afferentBackend(path string, getenv func(string) string) (*BrainsrvBackend, error) {
	sess, err := afferentMember()
	if err != nil {
		if errors.Is(err, member.ErrNotSignedIn) {
			return nil, fmt.Errorf("%w; run `afferent login`, or set %s", err, brainsrvcfg.EnvKeyFile)
		}
		return nil, fmt.Errorf("afferent credentials: %w", err)
	}
	cfg := brainsrvcfg.Config{URL: sess.Config.BrainsrvURL}
	// The member's authsrv token, Context and scope belong to afferent's
	// brainsrv_url. A leftover BEACON_BRAINSRV_URL (from the key-file
	// setup) may only restate it; a different host would receive the token.
	if u := strings.TrimSpace(getenv(brainsrvcfg.EnvURL)); u != "" {
		v, err := brainsrvcfg.ValidateURL(u)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", brainsrvcfg.EnvURL, err)
		}
		if !strings.EqualFold(strings.TrimRight(v, "/"), strings.TrimRight(cfg.URL, "/")) {
			return nil, fmt.Errorf("%s=%s differs from afferent's brainsrv_url %s; afferent credentials are only sent to afferent's brainsrv: unset %s (or change brainsrv_url with afferent)", brainsrvcfg.EnvURL, v, cfg.URL, brainsrvcfg.EnvURL)
		}
	}
	if s := strings.TrimSpace(getenv(brainsrvcfg.EnvScope)); s != "" {
		cfg.Scope = s
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), backendTimeout)
		defer cancel()
		if cfg.Scope, err = sess.Scope(ctx); err != nil {
			return nil, fmt.Errorf("afferent member scope: %w", err)
		}
	}
	if !brainsrvcfg.ValidScope(cfg.Scope) {
		return nil, fmt.Errorf("member scope %q is not a valid brainsrv scope", cfg.Scope)
	}
	return NewBrainsrvBackendWithTokens(path, cfg, sess.Config.Context, sess.Tokens, nil), nil
}

// afferentMember returns the signed-in member. Stores are opened per request
// (and on macOS reading the credentials runs /usr/bin/security), so the
// answer is kept for memberTTL.
var afferentMember = cachedMember(defaultMember, 30*time.Second)

// defaultMember reads the real afferent settings and credentials. Inside a
// test binary it reports "not signed in", so no test outside this file's own
// (which replace afferentMember) can reach the real Keychain or
// ~/.config/afferent through OpenConfigured.
func defaultMember() (*member.Session, error) {
	if testing.Testing() {
		return nil, member.ErrNotSignedIn
	}
	return member.Open(member.Options{Getenv: os.Getenv})
}

func cachedMember(open func() (*member.Session, error), ttl time.Duration) func() (*member.Session, error) {
	var (
		mu   sync.Mutex
		at   time.Time
		sess *member.Session
		err  error
	)
	return func() (*member.Session, error) {
		mu.Lock()
		defer mu.Unlock()
		if !at.IsZero() && time.Since(at) < ttl {
			return sess, err
		}
		sess, err = open()
		at = time.Now()
		return sess, err
	}
}
