package auth

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Claims are the access-token claims the CLI displays. They are decoded
// without verifying the signature: the CLI only reads its own token for
// display and sanity checks; brainsrv does the verification that matters.
type Claims struct {
	Issuer        string   `json:"iss"`
	Subject       string   `json:"sub"`
	Audience      Audience `json:"aud"`
	Email         string   `json:"email"`
	TenantID      string   `json:"tenant_id"`
	AccountID     string   `json:"account_id"`
	PrincipalType string   `json:"principal_type"`
	Roles         []string `json:"roles"`
	Scopes        []string `json:"scopes"`
	ExpiresAt     int64    `json:"exp"`
	IssuedAt      int64    `json:"iat"`
}

// Expiry returns exp as a time, or the zero time when absent.
func (c *Claims) Expiry() time.Time {
	if c.ExpiresAt == 0 {
		return time.Time{}
	}
	return time.Unix(c.ExpiresAt, 0)
}

// Audience accepts aud as a string or an array of strings.
type Audience []string

func (a *Audience) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*a = Audience{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return err
	}
	*a = many
	return nil
}

// DecodeClaims decodes a JWT's payload without verifying it.
func DecodeClaims(jwt string) (*Claims, error) {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return nil, errors.New("access token is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return nil, errors.New("access token payload is not base64url")
	}
	var c Claims
	if err := json.Unmarshal(payload, &c); err != nil {
		return nil, errors.New("access token payload is not JSON")
	}
	return &c, nil
}
