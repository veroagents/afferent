// Package auth signs the afferent CLI in to authsrv with the OAuth 2.0 device
// authorization grant (RFC 8628), stores the resulting tokens, and hands out
// fresh access tokens, refreshing them under a cross-process lock.
package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/afferent/config"
)

// DeviceGrantType is the RFC 8628 grant type URN.
const DeviceGrantType = "urn:ietf:params:oauth:grant-type:device_code"

// DefaultScope is what the CLI asks for at login.
const DefaultScope = "openid profile email"

// maxBody caps every response body the CLI reads from authsrv or brainsrv.
const maxBody = 1 << 20

// Client talks to one authsrv issuer as one public OAuth client.
type Client struct {
	// HTTP is the client used for every request. nil means a client with a
	// 30s timeout.
	HTTP *http.Client
	// Issuer is the configured issuer URL (no trailing slash).
	Issuer string
	// DialBase optionally replaces the issuer's origin for network requests
	// (see config.Config.IssuerDial).
	DialBase string
	// ClientID is the public client id.
	ClientID string
	// TokenEndpoint overrides the derived device/refresh token endpoint.
	TokenEndpoint string

	// Now and Sleep are hooks for tests; nil means the real clock.
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error
}

// NewClient builds a Client from the effective configuration.
func NewClient(cfg config.Config) *Client {
	return &Client{
		Issuer:        strings.TrimRight(cfg.Issuer, "/"),
		DialBase:      strings.TrimRight(cfg.IssuerDial, "/"),
		ClientID:      cfg.ClientID,
		TokenEndpoint: cfg.TokenEndpoint,
	}
}

// Endpoints are the issuer's endpoints as the CLI will dial them.
type Endpoints struct {
	// Issuer is the canonical issuer identifier reported by discovery. Token
	// iss claims must equal it.
	Issuer              string
	DeviceAuthorization string
	// Token is used for both device-code polling and refresh.
	Token      string
	Revocation string
}

type discoveryDoc struct {
	Issuer                      string `json:"issuer"`
	DeviceAuthorizationEndpoint string `json:"device_authorization_endpoint"`
	TokenEndpoint               string `json:"token_endpoint"`
	RevocationEndpoint          string `json:"revocation_endpoint"`
}

// Discover fetches /.well-known/openid-configuration and resolves the
// endpoints the CLI uses.
//
// Issuer vs. the address the CLI dials. In vero-local the issuer is
// http://authsrv.vero.localhost:8801, which many resolvers map to loopback
// (RFC 6761) but some do not. The CLI accepts either:
//   - Issuer set to the real issuer, optionally with DialBase (for example
//     http://localhost:8801) to reach it; or
//   - Issuer set to a loopback alias such as http://localhost:8801. Discovery
//     then reports a different issuer. That is accepted only when both hosts
//     are loopback names (so nothing off-machine can claim the identity), the
//     reported issuer becomes the canonical one that token iss must match,
//     and the configured URL becomes the dial base.
//
// Any other mismatch between the configured and reported issuer is refused
// (an OAuth mix-up guard). Endpoints under the canonical issuer are rewritten
// onto the dial base.
//
// Token endpoint. authsrv serves the device grant (and, with D2, refresh for
// device-flow tokens) at /oauth/token next to /oauth/device/code, while its
// discovery token_endpoint is fosite's /oauth2/token, which rejects the device
// grant. So when the device endpoint ends in /oauth/device/code the token
// endpoint is its sibling /oauth/token; otherwise discovery's token_endpoint
// is used. Client.TokenEndpoint overrides both.
func (c *Client) Discover(ctx context.Context) (*Endpoints, error) {
	base := c.Issuer
	if c.DialBase != "" {
		base = c.DialBase
	}
	if err := config.CheckURL("issuer", c.Issuer); err != nil {
		return nil, err
	}
	var doc discoveryDoc
	if err := c.getJSON(ctx, base+"/.well-known/openid-configuration", &doc); err != nil {
		return nil, fmt.Errorf("OIDC discovery at %s: %w", base, err)
	}
	canonical := strings.TrimRight(doc.Issuer, "/")
	if canonical == "" {
		return nil, errors.New("OIDC discovery: document has no issuer")
	}
	dial := c.DialBase
	if canonical != c.Issuer {
		if !loopbackURL(c.Issuer) || !loopbackURL(canonical) {
			return nil, fmt.Errorf("issuer mismatch: configured %s but discovery reports %s", c.Issuer, canonical)
		}
		if dial == "" {
			dial = c.Issuer
		}
	}
	rewrite := func(name, u string) (string, error) {
		if u == "" {
			return "", nil
		}
		if dial != "" && dial != canonical && (u == canonical || strings.HasPrefix(u, canonical+"/")) {
			u = dial + strings.TrimPrefix(u, canonical)
		}
		if err := config.CheckURL(name, u); err != nil {
			return "", err
		}
		return u, nil
	}
	ep := &Endpoints{Issuer: canonical}
	var err error
	if doc.DeviceAuthorizationEndpoint == "" {
		return nil, fmt.Errorf("issuer %s does not advertise device_authorization_endpoint", canonical)
	}
	if ep.DeviceAuthorization, err = rewrite("device_authorization_endpoint", doc.DeviceAuthorizationEndpoint); err != nil {
		return nil, err
	}
	tokenURL := doc.TokenEndpoint
	if strings.HasSuffix(doc.DeviceAuthorizationEndpoint, "/oauth/device/code") {
		tokenURL = strings.TrimSuffix(doc.DeviceAuthorizationEndpoint, "/device/code") + "/token"
	}
	if c.TokenEndpoint != "" {
		tokenURL = c.TokenEndpoint
	}
	if tokenURL == "" {
		return nil, fmt.Errorf("issuer %s does not advertise token_endpoint", canonical)
	}
	if ep.Token, err = rewrite("token_endpoint", tokenURL); err != nil {
		return nil, err
	}
	if ep.Revocation, err = rewrite("revocation_endpoint", doc.RevocationEndpoint); err != nil {
		return nil, err
	}
	return ep, nil
}

func loopbackURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && config.IsLoopbackHost(u.Hostname())
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// noRedirectClient is httpClient with redirects refused. Token, device and
// revocation POSTs carry a refresh token or device code in the body; a 307/308
// would re-send it to the Location URL, bypassing the https-or-loopback check
// the configured endpoints passed.
func (c *Client) noRedirectClient() *http.Client {
	hc := *c.httpClient()
	hc.CheckRedirect = func(req *http.Request, _ []*http.Request) error {
		return fmt.Errorf("refusing to follow redirect to %s", req.URL.Redacted())
	}
	return &hc
}

// checkedRedirectClient is httpClient with every redirect target held to the
// same https-or-loopback rule as the configured URLs.
func (c *Client) checkedRedirectClient() *http.Client {
	hc := *c.httpClient()
	hc.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return config.CheckURL("redirect", req.URL.String())
	}
	return &hc
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Client) sleep(ctx context.Context, d time.Duration) error {
	if c.Sleep != nil {
		return c.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (c *Client) getJSON(ctx context.Context, u string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.checkedRedirectClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, snippet(body))
	}
	return json.Unmarshal(body, out)
}

// OAuthError is an RFC 6749 §5.2 error response.
type OAuthError struct {
	Status      int
	Code        string `json:"error"`
	Description string `json:"error_description"`
}

func (e *OAuthError) Error() string {
	if e.Description != "" {
		return fmt.Sprintf("%s: %s", e.Code, e.Description)
	}
	return e.Code
}

// postForm POSTs form to u. A 2xx body is decoded into out. A 4xx with an
// OAuth error body returns *OAuthError; anything else returns a plain error
// (transient: network, 5xx, garbage).
func (c *Client) postForm(ctx context.Context, u string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := c.noRedirectClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if out == nil || len(body) == 0 {
			return nil
		}
		if err := json.Unmarshal(body, out); err != nil {
			return fmt.Errorf("decode response from %s: %w", u, err)
		}
		return nil
	}
	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		oe := &OAuthError{Status: resp.StatusCode}
		if json.Unmarshal(body, oe) == nil && oe.Code != "" {
			return oe
		}
	}
	return fmt.Errorf("%s: HTTP %d: %s", u, resp.StatusCode, snippet(body))
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}
