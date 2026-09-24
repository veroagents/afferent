package auth

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Device-flow outcomes the caller may want to tell apart.
var (
	ErrAccessDenied = errors.New("sign-in was denied in the browser")
	ErrExpiredToken = errors.New("the sign-in code expired before it was approved; run afferent login again")
)

// DeviceCode is the RFC 8628 §3.2 device authorization response.
type DeviceCode struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete,omitempty"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

// BrowserURL is the page to open for the user: verification_uri_complete
// when the server gave one, else verification_uri with ?user_code= added.
// It returns "" when the URL is not http(s), so the CLI never hands an
// arbitrary scheme from the server to the OS opener.
func (d *DeviceCode) BrowserURL() string {
	raw := d.VerificationURIComplete
	if raw == "" {
		u, err := url.Parse(d.VerificationURI)
		if err != nil {
			return ""
		}
		q := u.Query()
		q.Set("user_code", d.UserCode)
		u.RawQuery = q.Encode()
		raw = u.String()
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return ""
	}
	return raw
}

// TokenResponse is an RFC 6749 §5.1 token response.
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	TokenType    string `json:"token_type,omitempty"`
	ExpiresIn    int64  `json:"expires_in,omitempty"`
	Scope        string `json:"scope,omitempty"`
	IDToken      string `json:"id_token,omitempty"`
}

// RequestDeviceCode starts the device flow (RFC 8628 §3.1).
func (c *Client) RequestDeviceCode(ctx context.Context, ep *Endpoints, scope string) (*DeviceCode, error) {
	form := url.Values{"client_id": {c.ClientID}}
	if scope != "" {
		form.Set("scope", scope)
	}
	var dc DeviceCode
	if err := c.postForm(ctx, ep.DeviceAuthorization, form, &dc); err != nil {
		return nil, fmt.Errorf("request device code: %w", err)
	}
	if dc.DeviceCode == "" || dc.UserCode == "" || dc.VerificationURI == "" {
		return nil, errors.New("request device code: response is missing device_code, user_code or verification_uri")
	}
	return &dc, nil
}

// maxTransientFailures bounds consecutive network/5xx failures while polling.
const maxTransientFailures = 5

// PollToken polls the token endpoint until the user approves, denies, or the
// code expires (RFC 8628 §3.4–3.5). It waits interval seconds (default 5)
// between polls and adds 5 seconds on every slow_down.
func (c *Client) PollToken(ctx context.Context, ep *Endpoints, dc *DeviceCode) (*TokenResponse, error) {
	interval := time.Duration(dc.Interval) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}
	expiresIn := time.Duration(dc.ExpiresIn) * time.Second
	if expiresIn <= 0 {
		expiresIn = 15 * time.Minute
	}
	deadline := c.now().Add(expiresIn)
	form := url.Values{
		"grant_type":  {DeviceGrantType},
		"device_code": {dc.DeviceCode},
		"client_id":   {c.ClientID},
	}
	failures := 0
	for {
		if err := c.sleep(ctx, interval); err != nil {
			return nil, err
		}
		if c.now().After(deadline) {
			return nil, ErrExpiredToken
		}
		var tr TokenResponse
		err := c.postForm(ctx, ep.Token, form, &tr)
		if err == nil {
			if tr.AccessToken == "" {
				return nil, errors.New("token response has no access_token")
			}
			if tt := tr.TokenType; tt != "" && !strings.EqualFold(tt, "bearer") {
				return nil, fmt.Errorf("unsupported token_type %q", tt)
			}
			return &tr, nil
		}
		var oe *OAuthError
		if !errors.As(err, &oe) {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			failures++
			if failures >= maxTransientFailures {
				return nil, fmt.Errorf("polling token endpoint: %w", err)
			}
			continue
		}
		failures = 0
		switch oe.Code {
		case "authorization_pending":
		case "slow_down":
			interval += 5 * time.Second
		case "access_denied":
			return nil, ErrAccessDenied
		case "expired_token":
			return nil, ErrExpiredToken
		default:
			return nil, fmt.Errorf("device login failed: %w", oe)
		}
	}
}
