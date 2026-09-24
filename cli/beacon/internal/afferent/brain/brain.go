// Package brain is the afferent CLI's brainsrv client.
package brain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ErrWhoamiUnsupported means brainsrv has no /v1/whoami (an older build).
var ErrWhoamiUnsupported = errors.New("brainsrv does not support /v1/whoami")

// Client calls brainsrv as the signed-in user.
type Client struct {
	BaseURL string
	Context string // sent as X-Context
	HTTP    *http.Client
	// Token returns a valid bearer token.
	Token func(ctx context.Context) (string, error)
}

// Grant is one scope the principal holds.
type Grant struct {
	Scope      string   `json:"scope"`
	Verbs      []string `json:"verbs"`
	TemplateID string   `json:"template_id,omitempty"`
}

// Whoami is brainsrv's GET /v1/whoami response.
type Whoami struct {
	PrincipalID string  `json:"principal_id"`
	Kind        string  `json:"kind"`
	Context     string  `json:"context"`
	Subject     string  `json:"subject"`
	Grants      []Grant `json:"grants"`
}

// StatusError is a non-2xx brainsrv response.
type StatusError struct {
	Status int
	Body   string
}

func (e *StatusError) Error() string {
	if e.Body != "" {
		return fmt.Sprintf("brainsrv HTTP %d: %s", e.Status, e.Body)
	}
	return fmt.Sprintf("brainsrv HTTP %d", e.Status)
}

// Whoami asks brainsrv who the token's principal is in the Context.
func (c *Client) Whoami(ctx context.Context) (*Whoami, error) {
	tok, err := c.Token(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.BaseURL, "/")+"/v1/whoami", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json")
	if c.Context != "" {
		req.Header.Set("X-Context", c.Context)
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := NoRedirects(hc).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed:
		return nil, ErrWhoamiUnsupported
	case resp.StatusCode != http.StatusOK:
		s := strings.TrimSpace(string(body))
		if len(s) > 300 {
			s = s[:300] + "…"
		}
		return nil, &StatusError{Status: resp.StatusCode, Body: s}
	}
	var w Whoami
	if err := json.Unmarshal(body, &w); err != nil {
		return nil, fmt.Errorf("decode /v1/whoami: %w", err)
	}
	return &w, nil
}
