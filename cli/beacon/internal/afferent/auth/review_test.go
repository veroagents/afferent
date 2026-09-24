package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/afferent/afferenttest"
)

// unsupported_grant_type (an authsrv without refresh for device tokens) must
// ask for a login but keep the stored credentials; only invalid_grant clears.
func TestTokenSourceUnsupportedGrantKeepsCredentials(t *testing.T) {
	for _, code := range []string{"unsupported_grant_type", "unauthorized_client"} {
		t.Run(code, func(t *testing.T) {
			a := afferenttest.NewAuthsrv(t)
			a.RefreshError = code
			dir := t.TempDir()
			fs := seed(t, a, dir)
			ts := &TokenSource{Store: fs, LockPath: LockFile(dir), Client: newClient(a, nil)}
			if _, err := ts.Token(context.Background()); !errors.Is(err, ErrLoginRequired) {
				t.Fatalf("want ErrLoginRequired, got %v", err)
			}
			c, err := fs.Load()
			if err != nil || c.RefreshToken != "rt-seed" {
				t.Fatalf("credentials not kept: %v %+v", err, c)
			}
		})
	}
}

// Credentials issued by another issuer or to another client must never be
// refreshed against (or returned for) the configured one, and must be kept.
func TestTokenSourceRefusesCredentialsFromOtherIssuerOrClient(t *testing.T) {
	for _, tc := range []struct {
		name, issuer, client string
		expiry               time.Duration
	}{
		{"other issuer expiring", "https://a.example", "afferent-cli", 30 * time.Second},
		{"other client expiring", "", "other-cli", 30 * time.Second},
		{"other issuer still valid", "https://a.example", "afferent-cli", time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := afferenttest.NewAuthsrv(t)
			a.IssueRefresh("rt-a")
			dir := t.TempDir()
			fs := &FileStore{Path: CredentialsFile(dir)}
			iss := tc.issuer
			if iss == "" {
				iss = a.URL()
			}
			if err := fs.Save(&Credentials{AccessToken: "x.y.z", RefreshToken: "rt-a", Expiry: time.Now().Add(tc.expiry), Issuer: iss, ClientID: tc.client}); err != nil {
				t.Fatal(err)
			}
			ts := &TokenSource{Store: fs, LockPath: LockFile(dir), Client: newClient(a, nil)}
			_, err := ts.Token(context.Background())
			if !errors.Is(err, ErrLoginRequired) || !errors.Is(err, ErrOtherIssuer) {
				t.Fatalf("want ErrOtherIssuer/ErrLoginRequired, got %v", err)
			}
			if n := a.RefreshCalls.Load(); n != 0 {
				t.Fatalf("refresh token sent to the wrong issuer/client (%d calls)", n)
			}
			if c, err := fs.Load(); err != nil || c.RefreshToken != "rt-a" {
				t.Fatalf("credentials not kept: %v %+v", err, c)
			}
		})
	}
}

// A redirect from the token endpoint must not re-POST the refresh token.
func TestPostFormRefusesRedirect(t *testing.T) {
	var hit atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit.Add(1)
		w.Write([]byte(`{"access_token":"stolen"}`))
	}))
	defer target.Close()
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/token", http.StatusPermanentRedirect)
	}))
	defer redir.Close()
	c := &Client{ClientID: "afferent-cli"}
	ep := &Endpoints{Token: redir.URL + "/token", Revocation: redir.URL + "/revoke"}
	if _, err := c.Refresh(context.Background(), ep, "rt-secret"); err == nil || !strings.Contains(err.Error(), "redirect") {
		t.Fatalf("refresh: want redirect refusal, got %v", err)
	}
	if err := c.Revoke(context.Background(), ep, "rt-secret", "refresh_token"); err == nil {
		t.Fatal("revoke: want redirect refusal")
	}
	if n := hit.Load(); n != 0 {
		t.Fatalf("form re-sent to the redirect target %d times", n)
	}
}

// Discovery may follow redirects, but only to https or loopback URLs.
func TestDiscoveryRedirectMustPassCheckURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://auth.example.com/.well-known/openid-configuration", http.StatusFound)
	}))
	defer srv.Close()
	c := &Client{Issuer: srv.URL, ClientID: "x"}
	if _, err := c.Discover(context.Background()); err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("want https refusal, got %v", err)
	}
}

// lockProbeStore records whether the refresh lock was held during Load.
type lockProbeStore struct {
	Store
	lockPath   string
	heldOnLoad bool
}

func (s *lockProbeStore) Load() (*Credentials, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	if unlock, err := lockFile(ctx, s.lockPath); err == nil {
		unlock()
	} else {
		s.heldOnLoad = true
	}
	return s.Store.Load()
}

// Login and logout must read the previous refresh token under the lock that
// refresh uses, or they can revoke a token another process already rotated.
func TestSwapAndRemoveReadUnderLock(t *testing.T) {
	dir := t.TempDir()
	lock := LockFile(dir)
	fs := &FileStore{Path: CredentialsFile(dir)}
	if err := fs.Save(&Credentials{AccessToken: "a1", RefreshToken: "rt-1"}); err != nil {
		t.Fatal(err)
	}
	probe := &lockProbeStore{Store: fs, lockPath: lock}
	old, err := SwapLocked(context.Background(), probe, lock, &Credentials{AccessToken: "a2", RefreshToken: "rt-2"})
	if err != nil || old == nil || old.RefreshToken != "rt-1" || !probe.heldOnLoad {
		t.Fatalf("swap: %v %+v heldOnLoad=%v", err, old, probe.heldOnLoad)
	}
	probe.heldOnLoad = false
	old, lerr, derr := RemoveLocked(context.Background(), probe, lock)
	if lerr != nil || derr != nil || old == nil || old.RefreshToken != "rt-2" || !probe.heldOnLoad {
		t.Fatalf("remove: %v %v %+v heldOnLoad=%v", lerr, derr, old, probe.heldOnLoad)
	}
	if _, err := fs.Load(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("not deleted: %v", err)
	}
}
