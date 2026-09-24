// Package afferenttest holds test fakes shared by the afferent packages: an
// in-process authsrv (discovery, device flow, rotating refresh tokens,
// revocation) and a JWT builder. It is imported only from _test files.
package afferenttest

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// MakeJWT returns an unsigned-looking JWT with the given claims.
func MakeJWT(claims map[string]any) string {
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	return enc(map[string]string{"alg": "ES256", "typ": "JWT"}) + "." + enc(claims) + "." +
		base64.RawURLEncoding.EncodeToString([]byte("sig"))
}

// Authsrv is a fake authsrv. Fields may be set before the first request.
type Authsrv struct {
	Server *httptest.Server
	// Issuer is what discovery reports (default: the server URL).
	Issuer string
	// DeviceScript is the sequence of device-grant poll outcomes: an OAuth
	// error code, or "ok" to issue tokens. The last entry repeats.
	DeviceScript []string
	// ExpiresIn is the access-token lifetime in seconds (default 900).
	ExpiresIn int64
	// RefreshDelay is how long a refresh takes (to widen race windows).
	RefreshDelay time.Duration
	// Claims are added to every issued access token.
	Claims map[string]any

	mu            sync.Mutex
	polls         int
	serial        int
	validRefresh  map[string]bool
	Revoked       []string
	DeviceForms   []map[string]string
	RefreshCalls  atomic.Int32
	RevokeHints   []string
	LastDeviceReq map[string]string
}

// NewAuthsrv starts a fake authsrv; it is closed when the test ends.
func NewAuthsrv(t testing.TB) *Authsrv {
	a := &Authsrv{validRefresh: map[string]bool{}, ExpiresIn: 900, DeviceScript: []string{"ok"}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", a.discovery)
	mux.HandleFunc("POST /oauth/device/code", a.deviceCode)
	mux.HandleFunc("POST /oauth/token", a.token)
	mux.HandleFunc("POST /oauth2/token", func(w http.ResponseWriter, r *http.Request) {
		oauthErr(w, 400, "invalid_request", "fosite endpoint: device grant not supported here")
	})
	mux.HandleFunc("POST /oauth2/revoke", a.revoke)
	a.Server = httptest.NewServer(mux)
	t.Cleanup(a.Server.Close)
	return a
}

// URL is the server's base URL.
func (a *Authsrv) URL() string { return a.Server.URL }

func (a *Authsrv) issuer() string {
	if a.Issuer != "" {
		return a.Issuer
	}
	return a.Server.URL
}

func (a *Authsrv) discovery(w http.ResponseWriter, r *http.Request) {
	iss := a.issuer()
	writeJSON(w, 200, map[string]any{
		"issuer":                        iss,
		"device_authorization_endpoint": iss + "/oauth/device/code",
		"token_endpoint":                iss + "/oauth2/token",
		"revocation_endpoint":           iss + "/oauth2/revoke",
	})
}

func (a *Authsrv) deviceCode(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	a.mu.Lock()
	a.LastDeviceReq = map[string]string{"client_id": r.PostForm.Get("client_id"), "scope": r.PostForm.Get("scope")}
	a.mu.Unlock()
	writeJSON(w, 200, map[string]any{
		"device_code":      "dev-code-1",
		"user_code":        "WXPK-HQNM",
		"verification_uri": a.issuer() + "/oauth/device/verify",
		"expires_in":       900,
		"interval":         5,
	})
}

// IssueRefresh registers a refresh token as valid (for seeding tests).
func (a *Authsrv) IssueRefresh(tok string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.validRefresh[tok] = true
}

// RefreshValid reports whether tok is currently redeemable.
func (a *Authsrv) RefreshValid(tok string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.validRefresh[tok]
}

// Polls returns how many device-grant polls happened.
func (a *Authsrv) Polls() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.polls
}

func (a *Authsrv) mint() map[string]any {
	a.serial++
	claims := map[string]any{
		"iss": a.issuer(), "sub": "user-1", "aud": "brainsrv", "email": "drew@vero.localhost",
		"tenant_id": "tenant-1", "account_id": "acct-1",
		"exp": time.Now().Add(time.Duration(a.ExpiresIn) * time.Second).Unix(),
		"n":   a.serial,
	}
	for k, v := range a.Claims {
		claims[k] = v
	}
	rt := fmt.Sprintf("rt-%d", a.serial)
	a.validRefresh[rt] = true
	return map[string]any{
		"access_token":  MakeJWT(claims),
		"refresh_token": rt,
		"token_type":    "Bearer",
		"expires_in":    a.ExpiresIn,
	}
}

func (a *Authsrv) token(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	f := r.PostForm
	if f.Get("client_id") == "" {
		oauthErr(w, 400, "invalid_request", "client_id required")
		return
	}
	switch f.Get("grant_type") {
	case "urn:ietf:params:oauth:grant-type:device_code":
		a.mu.Lock()
		step := a.DeviceScript[min(a.polls, len(a.DeviceScript)-1)]
		a.polls++
		if step == "ok" {
			resp := a.mint()
			a.mu.Unlock()
			writeJSON(w, 200, resp)
			return
		}
		a.mu.Unlock()
		oauthErr(w, 400, step, "")
	case "refresh_token":
		a.RefreshCalls.Add(1)
		if a.RefreshDelay > 0 {
			time.Sleep(a.RefreshDelay)
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		rt := f.Get("refresh_token")
		if !a.validRefresh[rt] {
			oauthErr(w, 400, "invalid_grant", "refresh token revoked, expired or reused")
			return
		}
		delete(a.validRefresh, rt) // rotate: the old one is spent
		writeJSON(w, 200, a.mint())
	default:
		oauthErr(w, 400, "unsupported_grant_type", "")
	}
}

func (a *Authsrv) revoke(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	a.mu.Lock()
	defer a.mu.Unlock()
	tok := r.PostForm.Get("token")
	a.Revoked = append(a.Revoked, tok)
	a.RevokeHints = append(a.RevokeHints, r.PostForm.Get("token_type_hint")+"|"+r.PostForm.Get("client_id"))
	delete(a.validRefresh, tok)
	w.WriteHeader(200)
}

func oauthErr(w http.ResponseWriter, status int, code, desc string) {
	writeJSON(w, status, map[string]string{"error": code, "error_description": desc})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Clock is a fake clock whose Sleep advances time instantly.
type Clock struct {
	mu     sync.Mutex
	T      time.Time
	Sleeps []time.Duration
}

func NewClock() *Clock { return &Clock{T: time.Now()} }

func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.T
}

func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.T = c.T.Add(d)
}

// SleepFunc matches auth.Client.Sleep.
func (c *Clock) Sleep(_ context.Context, d time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Sleeps = append(c.Sleeps, d)
	c.T = c.T.Add(d)
	return nil
}
