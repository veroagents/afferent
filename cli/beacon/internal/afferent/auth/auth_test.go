package auth

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/afferent/afferenttest"
)

func newClient(a *afferenttest.Authsrv, clk *afferenttest.Clock) *Client {
	c := &Client{Issuer: a.URL(), ClientID: "afferent-cli"}
	if clk != nil {
		c.Now, c.Sleep = clk.Now, clk.Sleep
	}
	return c
}

func TestDiscoverDerivesDeviceTokenEndpoint(t *testing.T) {
	a := afferenttest.NewAuthsrv(t)
	ep, err := newClient(a, nil).Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ep.Issuer != a.URL() || ep.DeviceAuthorization != a.URL()+"/oauth/device/code" {
		t.Fatalf("endpoints %+v", ep)
	}
	// authsrv's discovery token_endpoint (/oauth2/token) rejects the device
	// grant; the device sibling /oauth/token is used instead.
	if ep.Token != a.URL()+"/oauth/token" {
		t.Fatalf("token endpoint %q", ep.Token)
	}
	if ep.Revocation != a.URL()+"/oauth2/revoke" {
		t.Fatalf("revocation endpoint %q", ep.Revocation)
	}

	c := newClient(a, nil)
	c.TokenEndpoint = a.URL() + "/custom/token"
	ep, err = c.Discover(context.Background())
	if err != nil || ep.Token != a.URL()+"/custom/token" {
		t.Fatalf("override: %v %+v", err, ep)
	}
}

func TestDiscoverLoopbackAliasIsRewritten(t *testing.T) {
	a := afferenttest.NewAuthsrv(t)
	a.Issuer = "http://authsrv.vero.localhost:8801"
	// The user configured the loopback address they can reach.
	ep, err := newClient(a, nil).Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ep.Issuer != "http://authsrv.vero.localhost:8801" {
		t.Fatalf("canonical issuer %q", ep.Issuer)
	}
	if ep.DeviceAuthorization != a.URL()+"/oauth/device/code" || ep.Token != a.URL()+"/oauth/token" {
		t.Fatalf("endpoints not rewritten onto the dial base: %+v", ep)
	}

	// Same via an explicit dial base with the real issuer configured.
	c := &Client{Issuer: "http://authsrv.vero.localhost:8801", DialBase: a.URL(), ClientID: "x"}
	ep, err = c.Discover(context.Background())
	if err != nil || ep.Token != a.URL()+"/oauth/token" {
		t.Fatalf("dial base: %v %+v", err, ep)
	}
}

func TestDiscoverRejectsForeignIssuer(t *testing.T) {
	a := afferenttest.NewAuthsrv(t)
	a.Issuer = "https://evil.example.com"
	if _, err := newClient(a, nil).Discover(context.Background()); err == nil || !strings.Contains(err.Error(), "issuer mismatch") {
		t.Fatalf("want issuer mismatch, got %v", err)
	}
}

func TestDiscoverRejectsPlainHTTPRemote(t *testing.T) {
	c := &Client{Issuer: "http://auth.example.com", ClientID: "x"}
	if _, err := c.Discover(context.Background()); err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("want https error, got %v", err)
	}
}

func deviceLogin(t *testing.T, a *afferenttest.Authsrv, clk *afferenttest.Clock) (*TokenResponse, error) {
	t.Helper()
	c := newClient(a, clk)
	ep, err := c.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	dc, err := c.RequestDeviceCode(context.Background(), ep, DefaultScope)
	if err != nil {
		t.Fatal(err)
	}
	return c.PollToken(context.Background(), ep, dc)
}

func TestDeviceFlowPendingSlowDownSuccess(t *testing.T) {
	a := afferenttest.NewAuthsrv(t)
	a.DeviceScript = []string{"authorization_pending", "slow_down", "authorization_pending", "ok"}
	clk := afferenttest.NewClock()
	tr, err := deviceLogin(t, a, clk)
	if err != nil {
		t.Fatal(err)
	}
	if tr.AccessToken == "" || tr.RefreshToken == "" {
		t.Fatalf("tokens %+v", tr)
	}
	want := []time.Duration{5 * time.Second, 5 * time.Second, 10 * time.Second, 10 * time.Second}
	if len(clk.Sleeps) != len(want) {
		t.Fatalf("sleeps %v, want %v", clk.Sleeps, want)
	}
	for i := range want {
		if clk.Sleeps[i] != want[i] {
			t.Fatalf("sleeps %v, want %v", clk.Sleeps, want)
		}
	}
	if got := a.LastDeviceReq; got["client_id"] != "afferent-cli" || got["scope"] != "openid profile email" {
		t.Fatalf("device request %v", got)
	}
}

func TestDeviceFlowAccessDenied(t *testing.T) {
	a := afferenttest.NewAuthsrv(t)
	a.DeviceScript = []string{"authorization_pending", "access_denied"}
	if _, err := deviceLogin(t, a, afferenttest.NewClock()); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("want ErrAccessDenied, got %v", err)
	}
}

func TestDeviceFlowExpiredToken(t *testing.T) {
	a := afferenttest.NewAuthsrv(t)
	a.DeviceScript = []string{"expired_token"}
	if _, err := deviceLogin(t, a, afferenttest.NewClock()); !errors.Is(err, ErrExpiredToken) {
		t.Fatalf("want ErrExpiredToken, got %v", err)
	}
}

func TestDeviceFlowLocalDeadline(t *testing.T) {
	a := afferenttest.NewAuthsrv(t)
	a.DeviceScript = []string{"authorization_pending"} // forever
	clk := afferenttest.NewClock()
	if _, err := deviceLogin(t, a, clk); !errors.Is(err, ErrExpiredToken) {
		t.Fatalf("want ErrExpiredToken, got %v", err)
	}
	// 900s / 5s = 180 polls, then the local deadline stops it.
	if p := a.Polls(); p < 170 || p > 181 {
		t.Fatalf("polls %d", p)
	}
}

func TestBrowserURL(t *testing.T) {
	dc := &DeviceCode{UserCode: "AB-CD", VerificationURI: "http://authsrv.vero.localhost:8801/oauth/device/verify"}
	if got := dc.BrowserURL(); got != "http://authsrv.vero.localhost:8801/oauth/device/verify?user_code=AB-CD" {
		t.Fatalf("got %q", got)
	}
	dc.VerificationURIComplete = "https://x/verify?code=1"
	if got := dc.BrowserURL(); got != "https://x/verify?code=1" {
		t.Fatalf("got %q", got)
	}
	dc.VerificationURIComplete = "file:///etc/passwd"
	if got := dc.BrowserURL(); got != "" {
		t.Fatalf("non-http scheme passed through: %q", got)
	}
}

func TestNewCredentialsChecksIssuer(t *testing.T) {
	tr := &TokenResponse{AccessToken: afferenttest.MakeJWT(map[string]any{"iss": "https://other"}), ExpiresIn: 60}
	if _, err := NewCredentials(tr, "https://mine", "c", "", time.Now()); err == nil {
		t.Fatal("want issuer mismatch")
	}
	tr.AccessToken = afferenttest.MakeJWT(map[string]any{"iss": "https://mine"})
	c, err := NewCredentials(tr, "https://mine", "c", "old-rt", time.Now())
	if err != nil || c.RefreshToken != "old-rt" {
		t.Fatalf("%v %+v", err, c)
	}
}

// seed stores credentials that expire in 30s (inside the 60s skew) with a
// refresh token the fake server will accept.
func seed(t *testing.T, a *afferenttest.Authsrv, dir string) *FileStore {
	t.Helper()
	a.IssueRefresh("rt-seed")
	fs := &FileStore{Path: CredentialsFile(dir)}
	err := fs.Save(&Credentials{
		AccessToken:  afferenttest.MakeJWT(map[string]any{"iss": a.URL(), "n": 0}),
		RefreshToken: "rt-seed",
		Expiry:       time.Now().Add(30 * time.Second),
		Issuer:       a.URL(),
		ClientID:     "afferent-cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	return fs
}

func TestTokenSourceRefreshesAndPersistsRotation(t *testing.T) {
	a := afferenttest.NewAuthsrv(t)
	dir := t.TempDir()
	fs := seed(t, a, dir)
	ts := &TokenSource{Store: fs, LockPath: LockFile(dir), Client: newClient(a, nil)}

	tok, err := ts.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	stored, err := fs.Load()
	if err != nil {
		t.Fatal(err)
	}
	if stored.AccessToken != tok || stored.RefreshToken == "rt-seed" || stored.RefreshToken == "" {
		t.Fatalf("rotation not persisted: %+v", stored)
	}
	if a.RefreshValid("rt-seed") || !a.RefreshValid(stored.RefreshToken) {
		t.Fatal("server rotation state unexpected")
	}
	if !stored.Expiry.After(time.Now().Add(10 * time.Minute)) {
		t.Fatalf("expiry %v", stored.Expiry)
	}
	// Fresh now: no second refresh.
	if _, err := ts.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := a.RefreshCalls.Load(); n != 1 {
		t.Fatalf("refresh calls %d", n)
	}
}

func TestTokenSourceInvalidGrantClearsCredentials(t *testing.T) {
	a := afferenttest.NewAuthsrv(t)
	dir := t.TempDir()
	fs := &FileStore{Path: CredentialsFile(dir)}
	if err := fs.Save(&Credentials{AccessToken: "x.y.z", RefreshToken: "revoked", Expiry: time.Now().Add(-time.Minute), Issuer: a.URL(), ClientID: "afferent-cli"}); err != nil {
		t.Fatal(err)
	}
	ts := &TokenSource{Store: fs, LockPath: LockFile(dir), Client: newClient(a, nil)}
	_, err := ts.Token(context.Background())
	if !errors.Is(err, ErrLoginRequired) {
		t.Fatalf("want ErrLoginRequired, got %v", err)
	}
	if _, err := fs.Load(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("credentials not cleared: %v", err)
	}
}

func TestTokenSourceNotSignedIn(t *testing.T) {
	dir := t.TempDir()
	ts := &TokenSource{Store: &FileStore{Path: CredentialsFile(dir)}, LockPath: LockFile(dir)}
	if _, err := ts.Token(context.Background()); !errors.Is(err, ErrLoginRequired) {
		t.Fatalf("got %v", err)
	}
}

// Several independent token sources (standing in for the forwarder and CLI
// commands in separate processes) share one credentials file and lock. The
// refresh token must be redeemed exactly once.
func TestConcurrentTokenRequestsRotateOnce(t *testing.T) {
	a := afferenttest.NewAuthsrv(t)
	a.RefreshDelay = 150 * time.Millisecond
	dir := t.TempDir()
	seed(t, a, dir)

	const n = 8
	var wg sync.WaitGroup
	toks := make([]string, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ts := &TokenSource{Store: &FileStore{Path: CredentialsFile(dir)}, LockPath: LockFile(dir), Client: newClient(a, nil)}
			toks[i], errs[i] = ts.Token(context.Background())
		}(i)
	}
	wg.Wait()
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("worker %d: %v", i, errs[i])
		}
		if toks[i] != toks[0] {
			t.Fatalf("workers got different tokens")
		}
	}
	if c := a.RefreshCalls.Load(); c != 1 {
		t.Fatalf("refresh called %d times, want 1", c)
	}
}

func TestFileStorePermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "afferent")
	fs := &FileStore{Path: CredentialsFile(dir)}
	if err := fs.Save(&Credentials{AccessToken: "a", RefreshToken: "r"}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(fs.Path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("file mode %v", fi.Mode().Perm())
	}
	di, _ := os.Stat(dir)
	if di.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v", di.Mode().Perm())
	}
	if err := os.Chmod(fs.Path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.Load(); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("want permission error, got %v", err)
	}
}

func TestFileStoreRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "elsewhere.json")
	if err := os.WriteFile(target, []byte(`{"access_token":"a"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	fs := &FileStore{Path: filepath.Join(dir, "credentials.json")}
	if err := os.Symlink(target, fs.Path); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.Load(); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("load: want symlink error, got %v", err)
	}
	if err := fs.Save(&Credentials{AccessToken: "b"}); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("save: want symlink error, got %v", err)
	}
	if b, _ := os.ReadFile(target); string(b) != `{"access_token":"a"}` {
		t.Fatalf("symlink target was modified: %s", b)
	}
}
