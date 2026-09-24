package brainsrv

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// HealthTimeout bounds each call to brainsrv; status must stay fast offline.
const HealthTimeout = 5 * time.Second

// Endpoint is one row of GET /v1/ingest/beacon/health?scope=…: a hostname that posted under
// the scope, when brainsrv last heard from it, and how many lines its last batch accepted.
type Endpoint struct {
	Hostname          string    `json:"hostname"`
	LastSeen          time.Time `json:"last_seen"`
	LastBatchAccepted *int      `json:"last_batch_accepted"`
}

// HealthResponse is the body of a 200 from the health route.
type HealthResponse struct {
	OK        bool       `json:"ok"`
	Endpoints []Endpoint `json:"endpoints"`
}

// ErrUnauthorized and ErrForbidden classify the two answers that mean "this key cannot be used
// for this scope"; connect refuses on either.
var (
	ErrUnauthorized = errors.New("brainsrv rejected the key (HTTP 401): it is invalid, expired or revoked")
	ErrForbidden    = errors.New("brainsrv denied the key on this scope (HTTP 403)")
)

// Client talks to brainsrv's Beacon ingest routes with one member key.
type Client struct {
	BaseURL string
	Key     string
	HTTP    *http.Client
}

func (c Client) httpClient() *http.Client {
	base := c.HTTP
	if base == nil {
		base = &http.Client{Timeout: HealthTimeout}
	}
	// Never follow a redirect with the key attached: a moved brainsrv is a configuration
	// error to surface, not something to chase with a credential.
	clone := *base
	clone.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &clone
}

func (c Client) do(ctx context.Context, req *http.Request) (*http.Response, []byte, error) {
	req.Header.Set("Authorization", "Bearer "+c.Key)
	req.Header.Set("User-Agent", "afferent-beacon-cli")
	resp, err := c.httpClient().Do(req.WithContext(ctx))
	if err != nil {
		return nil, nil, fmt.Errorf("brainsrv unreachable: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp, body, nil
}

// Health calls GET <base>/v1/ingest/beacon/health?scope=<scope>. With a scope, brainsrv checks
// the key can read it (403 otherwise) and lists the endpoints that posted under it.
func (c Client) Health(ctx context.Context, scope string) (*HealthResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, HealthTimeout)
	defer cancel()
	req, err := http.NewRequest(http.MethodGet, HealthURL(c.BaseURL, scope), nil)
	if err != nil {
		return nil, err
	}
	resp, body, err := c.do(ctx, req)
	if err != nil {
		return nil, err
	}
	if err := classify(resp.StatusCode, body, scope, "read"); err != nil {
		return nil, err
	}
	var out HealthResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("brainsrv health answered 200 with an unreadable body: %w", err)
	}
	return &out, nil
}

// ProbeWrite POSTs an empty NDJSON batch to the runtime route with X-Scope: scope. brainsrv
// authorizes write on X-Scope before reading the body, so a key without write there gets a 403
// while an authorized one gets 200 with nothing accepted. Nothing is stored.
func (c Client) ProbeWrite(ctx context.Context, scope string) error {
	ctx, cancel := context.WithTimeout(ctx, HealthTimeout)
	defer cancel()
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(c.BaseURL, "/")+RuntimeIngestPath, bytes.NewReader(nil))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-ndjson")
	req.Header.Set("X-Scope", scope)
	resp, body, err := c.do(ctx, req)
	if err != nil {
		return err
	}
	return classify(resp.StatusCode, body, scope, "write")
}

func classify(status int, body []byte, scope, verb string) error {
	switch {
	case status >= 200 && status < 300:
		return nil
	case status == http.StatusUnauthorized:
		return ErrUnauthorized
	case status == http.StatusForbidden:
		return fmt.Errorf("%w: it cannot %s %s; mint a member key with narrow_scope=%s and read+write grants there%s", ErrForbidden, verb, scope, scope, serverMessage(body))
	default:
		return fmt.Errorf("brainsrv answered HTTP %d%s", status, serverMessage(body))
	}
}

// serverMessage extracts brainsrv's {"error": "..."} text, if any, for an error suffix.
func serverMessage(body []byte) string {
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil && e.Error != "" {
		return " (" + e.Error + ")"
	}
	return ""
}
