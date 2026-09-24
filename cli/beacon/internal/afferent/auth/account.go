package auth

// Account names the signed-in identity locally, without a network call:
// the issuer and the access token's subject. It is "" for an opaque token
// or one without a subject. Refreshing keeps it; signing in as someone else
// changes it, so it keys caches that belong to one member (the scope).
func (c *Credentials) Account() string {
	if c == nil || c.AccessToken == "" {
		return ""
	}
	cl, err := DecodeClaims(c.AccessToken)
	if err != nil || cl.Subject == "" {
		return ""
	}
	return c.Issuer + "#" + cl.Subject
}
