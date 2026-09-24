package forward

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/afferent/brain"
	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/afferent/config"
)

// ErrNoScope means brainsrv lists no writable ".harness" grant for this user
// in the Context.
var ErrNoScope = errors.New("no writable member scope")

// ErrAmbiguousScope means brainsrv lists more than one writable ".harness"
// grant, so the forwarder cannot pick one on its own.
var ErrAmbiguousScope = errors.New("more than one writable member scope")

// MemberScope picks the member base scope from a /v1/whoami answer: the single
// grant whose scope ends in ".harness" and that includes the write verb.
func MemberScope(w *brain.Whoami) (string, error) {
	var found []string
	for _, g := range w.Grants {
		if !strings.HasSuffix(g.Scope, ".harness") {
			continue
		}
		for _, v := range g.Verbs {
			if v == "write" {
				found = append(found, g.Scope)
				break
			}
		}
	}
	sort.Strings(found)
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return "", fmt.Errorf("%w: brainsrv lists no grant ending in \".harness\" with write for %s in Context %q; ask the Context admin for a member grant, or set the scope with --scope / AFFERENT_SCOPE", ErrNoScope, firstNonEmpty(w.Subject, w.PrincipalID, "this user"), w.Context)
	default:
		return "", fmt.Errorf("%w: %s; choose one with --scope or AFFERENT_SCOPE", ErrAmbiguousScope, strings.Join(found, ", "))
	}
}

// scopeCache remembers the last scope brainsrv reported, so the forwarder can
// start while brainsrv is unreachable.
type scopeCache struct {
	BrainsrvURL string    `json:"brainsrv_url"`
	Context     string    `json:"context"`
	Principal   string    `json:"principal,omitempty"`
	Scope       string    `json:"scope"`
	ResolvedAt  time.Time `json:"resolved_at"`
}

// ScopeResolver finds the X-Scope to send.
type ScopeResolver struct {
	// Override is a configured scope (flag, env or config.json). When set it
	// is used as is; brainsrv still enforces the grant.
	Override string
	StateDir string
	URL      string
	Context  string
	Whoami   func(ctx context.Context) (*brain.Whoami, error)
	Now      func() time.Time
}

// Resolve returns the scope and where it came from ("configured",
// "brainsrv" or "cached"). A brainsrv answer is cached; if brainsrv cannot be
// reached (a network error or 5xx), a cached scope for the same brainsrv and
// Context is used. A clear answer (no grant, ambiguous, 401/403) is never
// papered over with the cache. Being signed out is not a clear answer: the
// cached scope is returned, and err is nil.
func (r *ScopeResolver) Resolve(ctx context.Context) (scope, source string, err error) {
	if s := strings.TrimSpace(r.Override); s != "" {
		return s, "configured", nil
	}
	w, err := r.Whoami(ctx)
	if err == nil {
		s, err := MemberScope(w)
		if err != nil {
			return "", "", err
		}
		r.save(scopeCache{BrainsrvURL: r.URL, Context: r.Context, Principal: w.PrincipalID, Scope: s})
		return s, "brainsrv", nil
	}
	if ctx.Err() != nil {
		return "", "", ctx.Err()
	}
	// Signed out or brainsrv unreachable: the cached answer still holds, and
	// the forwarder reports "login required" itself when it needs a token.
	var se *brain.StatusError
	transient := !errors.Is(err, brain.ErrWhoamiUnsupported) && (!errors.As(err, &se) || se.Status >= 500)
	if transient {
		if c := r.load(); c != nil && c.BrainsrvURL == r.URL && c.Context == r.Context && c.Scope != "" {
			return c.Scope, "cached", nil
		}
	}
	if errors.Is(err, brain.ErrWhoamiUnsupported) {
		return "", "", fmt.Errorf("this brainsrv has no /v1/whoami, so the member scope is unknown; set it with --scope or AFFERENT_SCOPE")
	}
	return "", "", fmt.Errorf("learn the member scope from brainsrv /v1/whoami: %w", err)
}

func (r *ScopeResolver) path() string { return filepath.Join(r.StateDir, ScopeCacheFile) }

func (r *ScopeResolver) load() *scopeCache {
	b, err := os.ReadFile(r.path())
	if err != nil {
		return nil
	}
	var c scopeCache
	if json.Unmarshal(b, &c) != nil {
		return nil
	}
	return &c
}

func (r *ScopeResolver) save(c scopeCache) {
	now := time.Now
	if r.Now != nil {
		now = r.Now
	}
	c.ResolvedAt = now().UTC()
	if config.EnsureDir(r.StateDir) != nil {
		return
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return
	}
	_ = config.WriteFileAtomic(r.path(), append(b, '\n'), 0o600)
}

// Cached returns the cached scope for display, or "".
func (r *ScopeResolver) Cached() string {
	if c := r.load(); c != nil && c.BrainsrvURL == r.URL && c.Context == r.Context {
		return c.Scope
	}
	return ""
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}
