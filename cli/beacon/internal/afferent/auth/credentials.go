package auth

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"time"
)

// Credentials are what the CLI keeps after login.
type Credentials struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	TokenType    string    `json:"token_type,omitempty"`
	Expiry       time.Time `json:"expiry"`
	// Issuer is the canonical issuer (from discovery), which iss matches.
	Issuer   string `json:"issuer"`
	ClientID string `json:"client_id"`
}

// ValidFor reports whether the access token is still valid skew from now.
func (c *Credentials) ValidFor(now time.Time, skew time.Duration) bool {
	return c != nil && c.AccessToken != "" && !c.Expiry.IsZero() && now.Add(skew).Before(c.Expiry)
}

// Matches reports whether these credentials were issued by issuer (the
// canonical issuer from discovery) to clientID. Refresh and revocation send
// the refresh token to the current issuer, so they must only use credentials
// that came from it; otherwise the token would leak to another server (an
// OAuth mix-up) or a mismatched invalid_grant would wipe a valid session.
func (c *Credentials) Matches(issuer, clientID string) bool {
	return c != nil && c.Issuer != "" && c.Issuer == issuer && c.ClientID == clientID
}

// ErrOtherIssuer means the stored credentials belong to a different issuer
// or client than the one configured. They are left in place.
var ErrOtherIssuer = errors.New("stored credentials belong to a different issuer or client")

func mismatchError(c *Credentials, issuer, clientID string) error {
	return fmt.Errorf("%w (stored: %s, client %s; configured: %s, client %s): %w",
		ErrOtherIssuer, c.Issuer, c.ClientID, issuer, clientID, ErrLoginRequired)
}

// NewCredentials turns a token response into Credentials. It checks that the
// access token's iss (when it is a JWT carrying one) is the expected issuer,
// and keeps prevRefresh when the server did not rotate the refresh token.
func NewCredentials(tr *TokenResponse, issuer, clientID, prevRefresh string, now time.Time) (*Credentials, error) {
	creds := &Credentials{
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		TokenType:    tr.TokenType,
		Issuer:       issuer,
		ClientID:     clientID,
	}
	if creds.RefreshToken == "" {
		creds.RefreshToken = prevRefresh
	}
	claims, cerr := DecodeClaims(tr.AccessToken)
	if cerr == nil && claims.Issuer != "" && claims.Issuer != issuer {
		return nil, fmt.Errorf("token issuer %q does not match %q", claims.Issuer, issuer)
	}
	switch {
	case tr.ExpiresIn > 0:
		creds.Expiry = now.Add(time.Duration(tr.ExpiresIn) * time.Second)
	case cerr == nil && claims.ExpiresAt > 0:
		creds.Expiry = claims.Expiry()
	default:
		// No lifetime given at all: assume a short one so it is refreshed.
		creds.Expiry = now.Add(5 * time.Minute)
	}
	return creds, nil
}

// Refresh redeems a refresh token (RFC 6749 §6) at the token endpoint.
func (c *Client) Refresh(ctx context.Context, ep *Endpoints, refreshToken string) (*TokenResponse, error) {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {c.ClientID},
	}
	var tr TokenResponse
	if err := c.postForm(ctx, ep.Token, form, &tr); err != nil {
		return nil, err
	}
	if tr.AccessToken == "" {
		return nil, errors.New("refresh response has no access_token")
	}
	return &tr, nil
}

// Revoke revokes a token (RFC 7009). The server answers 200 even for
// unknown tokens, so an error means the request itself failed.
func (c *Client) Revoke(ctx context.Context, ep *Endpoints, token, hint string) error {
	if ep.Revocation == "" {
		return errors.New("issuer does not advertise revocation_endpoint")
	}
	form := url.Values{"token": {token}, "client_id": {c.ClientID}}
	if hint != "" {
		form.Set("token_type_hint", hint)
	}
	return c.postForm(ctx, ep.Revocation, form, nil)
}
