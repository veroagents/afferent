package learning

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/afferent/afferenttest"
	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/afferent/auth"
	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/afferent/config"
	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/afferent/member"
	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/brainsrvcfg"
)

// afferentWorld is a signed-in afferent member in a temp config dir (file
// credential store, never the Keychain), a fake authsrv and a fake brainsrv.
type afferentWorld struct {
	t       *testing.T
	dir     string
	authsrv *afferenttest.Authsrv
	brain   *httptest.Server

	mu       sync.Mutex
	rejected map[string]bool // access tokens brainsrv refuses (401)
	seen     []http.Header
	whoamis  int
}

func newAfferentWorld(t *testing.T) *afferentWorld {
	w := &afferentWorld{t: t, dir: t.TempDir(), authsrv: afferenttest.NewAuthsrv(t), rejected: map[string]bool{}}
	w.brain = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		w.mu.Lock()
		w.seen = append(w.seen, r.Header.Clone())
		ok := tok != "" && !w.rejected[tok]
		if r.URL.Path == "/v1/whoami" {
			w.whoamis++
		}
		w.mu.Unlock()
		if !ok || r.Header.Get("X-Context") != "afferent-poc" {
			http.Error(rw, "invalid credentials", 401)
			return
		}
		switch r.URL.Path {
		case "/v1/whoami":
			rw.Write([]byte(`{"principal_id":"p-1","kind":"user","context":"afferent-poc","grants":[{"scope":"ws.t.people.u.harness","verbs":["read","write"]}]}`))
		case "/v1/ingest/beacon/health":
			if r.URL.Query().Get("scope") != "ws.t.people.u.harness" {
				http.Error(rw, "scope", 403)
				return
			}
			rw.Write([]byte(`{"ok":true,"endpoints":[]}`))
		default:
			http.NotFound(rw, r)
		}
	}))
	t.Cleanup(w.brain.Close)
	if err := config.Save(w.dir, config.Config{Issuer: w.authsrv.URL(), ClientID: "afferent-cli", BrainsrvURL: w.brain.URL, Context: "afferent-poc"}); err != nil {
		t.Fatal(err)
	}
	return w
}

func (w *afferentWorld) store() auth.Store {
	return &auth.FileStore{Path: auth.CredentialsFile(w.dir)}
}

// signIn stores credentials whose access token is tok.
func (w *afferentWorld) signIn(tok string) {
	w.authsrv.IssueRefresh("rt-seed")
	c := &auth.Credentials{AccessToken: tok, RefreshToken: "rt-seed", Expiry: time.Now().Add(10 * time.Minute), Issuer: w.authsrv.URL(), ClientID: "afferent-cli"}
	if err := w.store().Save(c); err != nil {
		w.t.Fatal(err)
	}
}

func (w *afferentWorld) reject(tok string) {
	w.mu.Lock()
	w.rejected[tok] = true
	w.mu.Unlock()
}

func (w *afferentWorld) open() (*member.Session, error) {
	return member.Open(member.Options{
		ConfigDir: w.dir,
		Getenv:    func(string) string { return "" },
		NewStore:  func(string, config.Config) auth.Store { return w.store() },
	})
}

// use makes OpenConfigured see this world, and clears the BEACON_* env.
func (w *afferentWorld) use() {
	prev := afferentMember
	afferentMember = w.open
	w.t.Cleanup(func() { afferentMember = prev })
	for _, k := range []string{brainsrvcfg.EnvBackend, brainsrvcfg.EnvURL, brainsrvcfg.EnvScope, brainsrvcfg.EnvKeyFile} {
		w.t.Setenv(k, "")
	}
}

func TestAfferentBackendTokenAuthAndRefresh(t *testing.T) {
	w := newAfferentWorld(t)
	w.signIn("jwt-old")
	w.use()

	store := OpenConfigured(filepath.Join(t.TempDir(), "memory.db"))
	backend, ok := BrainsrvOf(store)
	if !ok {
		t.Fatal("signed in to afferent, no key file: expected the brainsrv backend")
	}
	if backend.AuthKind() != "afferent" || backend.Config().Scope != "ws.t.people.u.harness" || backend.Config().URL != w.brain.URL {
		t.Fatalf("backend %s %+v", backend.AuthKind(), backend.Config())
	}

	// jwt-old is revoked server-side while it still looks valid: 401, one
	// forced refresh against authsrv, and the request is sent again.
	w.reject("jwt-old")
	h, err := backend.Health(context.Background())
	if err != nil || !h.OK {
		t.Fatalf("health with refreshed token: %v %+v", err, h)
	}
	if w.authsrv.RefreshCalls.Load() != 1 {
		t.Fatalf("refresh calls %d", w.authsrv.RefreshCalls.Load())
	}
	w.mu.Lock()
	first, last := w.seen[len(w.seen)-2], w.seen[len(w.seen)-1]
	w.mu.Unlock()
	if first.Get("Authorization") != "Bearer jwt-old" || last.Get("Authorization") == "Bearer jwt-old" ||
		last.Get("X-Context") != "afferent-poc" || !strings.HasPrefix(last.Get("Authorization"), "Bearer ") {
		t.Fatalf("headers %v then %v", first, last)
	}
	if c, _ := w.store().Load(); c == nil || c.AccessToken == "jwt-old" {
		t.Fatal("the refreshed credentials were not stored")
	}
}

func TestAfferentBackendSelection(t *testing.T) {
	w := newAfferentWorld(t)
	w.use()
	db := filepath.Join(t.TempDir(), "memory.db")

	// Not signed in, nothing set: local only, silently.
	if HasBackend(OpenConfigured(db)) {
		t.Fatal("backend without a sign-in")
	}

	w.signIn("jwt-1")
	if b, ok := BrainsrvOf(OpenConfigured(db)); !ok || b.AuthKind() != "afferent" {
		t.Fatal("signed in: want the afferent backend")
	}
	// The scope came from /v1/whoami once and is cached for the next open.
	before := w.whoamis
	if _, ok := BrainsrvOf(OpenConfigured(db)); !ok || w.whoamis != before {
		t.Fatalf("second open asked whoami again (%d -> %d)", before, w.whoamis)
	}

	// Explicit opt-out.
	t.Setenv(brainsrvcfg.EnvBackend, "local")
	if HasBackend(OpenConfigured(db)) {
		t.Fatal("BEACON_MEMORY_BACKEND=local must stay local")
	}

	// BEACON_BRAINSRV_SCOPE overrides the member scope on the afferent path.
	t.Setenv(brainsrvcfg.EnvBackend, "brainsrv")
	t.Setenv(brainsrvcfg.EnvScope, "ws.t.people.u.harness.sub")
	if b, ok := BrainsrvOf(OpenConfigured(db)); !ok || b.Config().Scope != "ws.t.people.u.harness.sub" {
		t.Fatal("scope override ignored")
	}

	// A key file wins and keeps the spk_ path.
	key := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(key, []byte("spk_test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(brainsrvcfg.EnvKeyFile, key)
	t.Setenv(brainsrvcfg.EnvURL, w.brain.URL)
	b, ok := BrainsrvOf(OpenConfigured(db))
	if !ok || b.AuthKind() != "key" {
		t.Fatal("key file: want the key backend")
	}
}

func TestAfferentBackendNotSignedInWithBackendBrainsrvWarns(t *testing.T) {
	w := newAfferentWorld(t)
	w.use()
	t.Setenv(brainsrvcfg.EnvBackend, "brainsrv")
	if _, err := afferentBackend("x", os.Getenv); !errors.Is(err, member.ErrNotSignedIn) || !strings.Contains(err.Error(), "afferent login") {
		t.Fatalf("got %v", err)
	}
	if HasBackend(OpenConfigured(filepath.Join(t.TempDir(), "m.db"))) {
		t.Fatal("backend without credentials")
	}
}

func TestDefaultMemberIsInertInTests(t *testing.T) {
	if _, err := defaultMember(); !errors.Is(err, member.ErrNotSignedIn) {
		t.Fatalf("defaultMember in a test binary must not read real credentials: %v", err)
	}
}

func TestCachedMember(t *testing.T) {
	n := 0
	get := cachedMember(func() (*member.Session, error) { n++; return nil, member.ErrNotSignedIn }, time.Hour)
	get()
	get()
	if n != 1 {
		t.Fatalf("opened %d times", n)
	}
}
