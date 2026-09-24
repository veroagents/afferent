package cli

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/afferent/afferenttest"
	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/afferent/auth"
	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/afferent/config"
)

type harness struct {
	t       *testing.T
	authsrv *afferenttest.Authsrv
	brain   *httptest.Server
	dir     string
	opened  []string
	envs    map[string]string
	whoami  http.HandlerFunc
}

func newHarness(t *testing.T) *harness {
	h := &harness{t: t, authsrv: afferenttest.NewAuthsrv(t), dir: t.TempDir()}
	h.whoami = func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") || r.Header.Get("X-Context") != "afferent-poc" {
			http.Error(w, "unauthorized", 401)
			return
		}
		w.Write([]byte(`{"principal_id":"p-1","kind":"user","context":"afferent-poc","subject":"user-1",
			"grants":[{"scope":"ws.dev.people.drew.harness","verbs":["read","write","forget"],"template_id":"member"}]}`))
	}
	h.brain = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.whoami(w, r) }))
	t.Cleanup(h.brain.Close)
	h.envs = map[string]string{
		config.EnvConfigDir:   h.dir,
		config.EnvIssuer:      h.authsrv.URL(),
		config.EnvBrainsrvURL: h.brain.URL,
	}
	return h
}

func (h *harness) run(args ...string) (string, string, error) {
	var out, errb bytes.Buffer
	clk := afferenttest.NewClock()
	env := &Env{
		Stdout:      &out,
		Stderr:      &errb,
		Getenv:      func(k string) string { return h.envs[k] },
		OpenBrowser: func(u string) error { h.opened = append(h.opened, u); return nil },
		// The file store only: tests never touch the real Keychain.
		NewStore: func(dir string, _ config.Config, _ func(error)) auth.Store {
			return &auth.FileStore{Path: auth.CredentialsFile(dir)}
		},
		Sleep: clk.Sleep,
	}
	root := NewRootCmd(env)
	root.SetArgs(args)
	err := root.ExecuteContext(context.Background())
	return out.String(), errb.String(), err
}

func (h *harness) store() *auth.FileStore { return &auth.FileStore{Path: auth.CredentialsFile(h.dir)} }

func TestLoginWhoamiLogout(t *testing.T) {
	h := newHarness(t)
	h.authsrv.DeviceScript = []string{"authorization_pending", "ok"}

	out, _, err := h.run("login")
	if err != nil {
		t.Fatalf("login: %v\n%s", err, out)
	}
	for _, want := range []string{"WXPK-HQNM", "/oauth/device/verify", "Signed in", "drew@vero.localhost"} {
		if !strings.Contains(out, want) {
			t.Errorf("login output missing %q:\n%s", want, out)
		}
	}
	if len(h.opened) != 1 || !strings.HasSuffix(h.opened[0], "/oauth/device/verify?user_code=WXPK-HQNM") {
		t.Fatalf("browser opened %v", h.opened)
	}
	creds, err := h.store().Load()
	if err != nil || creds.RefreshToken == "" || creds.Issuer != h.authsrv.URL() || creds.ClientID != "afferent-cli" {
		t.Fatalf("stored creds %v %+v", err, creds)
	}
	// login records the settings it used.
	if cfg, err := config.Load(h.dir); err != nil || cfg.Issuer != h.authsrv.URL() {
		t.Fatalf("config not saved: %v %+v", err, cfg)
	}

	out, _, err = h.run("whoami")
	if err != nil {
		t.Fatalf("whoami: %v\n%s", err, out)
	}
	for _, want := range []string{"user-1", "drew@vero.localhost", "tenant-1", "p-1", "ws.dev.people.drew.harness  [read,write,forget]  via template member"} {
		if !strings.Contains(out, want) {
			t.Errorf("whoami output missing %q:\n%s", want, out)
		}
	}

	refresh := creds.RefreshToken
	out, _, err = h.run("logout")
	if err != nil || !strings.Contains(out, "Signed out") {
		t.Fatalf("logout: %v %s", err, out)
	}
	if len(h.authsrv.Revoked) != 1 || h.authsrv.Revoked[0] != refresh || h.authsrv.RevokeHints[0] != "refresh_token|afferent-cli" {
		t.Fatalf("revoke calls %v %v", h.authsrv.Revoked, h.authsrv.RevokeHints)
	}
	if _, err := h.store().Load(); !errors.Is(err, auth.ErrNotFound) {
		t.Fatalf("credentials remain after logout: %v", err)
	}
	out, _, err = h.run("logout")
	if err != nil || !strings.Contains(out, "Not signed in") {
		t.Fatalf("second logout: %v %s", err, out)
	}
	if _, _, err := h.run("whoami"); !errors.Is(err, auth.ErrLoginRequired) {
		t.Fatalf("whoami after logout: %v", err)
	}
}

func TestLoginNoBrowserAndDenied(t *testing.T) {
	h := newHarness(t)
	h.authsrv.DeviceScript = []string{"access_denied"}
	_, _, err := h.run("login", "--no-browser")
	if !errors.Is(err, auth.ErrAccessDenied) {
		t.Fatalf("got %v", err)
	}
	if len(h.opened) != 0 {
		t.Fatalf("browser opened despite --no-browser: %v", h.opened)
	}
}

func TestWhoamiOlderBrainsrv(t *testing.T) {
	h := newHarness(t)
	if _, _, err := h.run("login", "--no-browser"); err != nil {
		t.Fatal(err)
	}
	h.whoami = func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }
	out, _, err := h.run("whoami")
	if err != nil {
		t.Fatalf("404 should degrade, got %v", err)
	}
	if !strings.Contains(out, "drew@vero.localhost") || !strings.Contains(out, "no /v1/whoami") {
		t.Fatalf("output:\n%s", out)
	}
}

func TestWhoamiRefreshesExpiringToken(t *testing.T) {
	h := newHarness(t)
	if _, _, err := h.run("login", "--no-browser"); err != nil {
		t.Fatal(err)
	}
	c, _ := h.store().Load()
	old := c.RefreshToken
	c.Expiry = time.Now().Add(10 * time.Second)
	if err := h.store().Save(c); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.run("whoami"); err != nil {
		t.Fatal(err)
	}
	c2, _ := h.store().Load()
	if c2.RefreshToken == old || h.authsrv.RefreshCalls.Load() != 1 {
		t.Fatalf("expected one rotation, calls=%d", h.authsrv.RefreshCalls.Load())
	}
}

func TestIssuerFlagRejectsPlainHTTPRemote(t *testing.T) {
	h := newHarness(t)
	_, _, err := h.run("login", "--issuer", "http://auth.example.com")
	if err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("got %v", err)
	}
}

func TestVersionAndHelp(t *testing.T) {
	h := newHarness(t)
	out, _, err := h.run("version")
	if err != nil || !strings.HasPrefix(out, "afferent ") {
		t.Fatalf("%v %q", err, out)
	}
	out, _, err = h.run("--help")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{"login", "logout", "whoami", "version"} {
		if !strings.Contains(out, c) {
			t.Errorf("help missing %s", c)
		}
	}
}
