package forward

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/afferent/auth"
)

// IngestPath is brainsrv's Beacon runtime ingest route.
const IngestPath = "/v1/ingest/beacon/runtime"

// IngestResult is brainsrv's 200 response.
type IngestResult struct {
	Accepted        int `json:"accepted"`
	Duplicate       int `json:"duplicate"`
	Rejected        int `json:"rejected"`
	SessionsTouched int `json:"sessions_touched"`
	Errors          []struct {
		Line  int    `json:"line"`
		Error string `json:"error"`
	} `json:"errors,omitempty"`
}

type errKind int

const (
	kindTransient errKind = iota // 5xx, network: back off and retry
	kindClient                   // other 4xx: back off and retry, never drop
	kindPause                    // login required, scope denied: wait for the user
	kindTooLarge                 // 413: split
)

// sendError is a failed delivery.
type sendError struct {
	kind   errKind
	reason string // paused reason, for kindPause
	status int
	msg    string
	err    error
}

func (e *sendError) Error() string {
	var b strings.Builder
	if e.status != 0 {
		fmt.Fprintf(&b, "brainsrv HTTP %d", e.status)
	}
	if e.reason != "" {
		if b.Len() > 0 {
			b.WriteString(": ")
		}
		b.WriteString(e.reason)
	}
	if e.msg != "" {
		if b.Len() > 0 {
			b.WriteString(": ")
		}
		b.WriteString(e.msg)
	}
	if e.err != nil {
		if b.Len() > 0 {
			b.WriteString(": ")
		}
		b.WriteString(e.err.Error())
	}
	return b.String()
}

func (e *sendError) Unwrap() error { return e.err }

// encode builds the NDJSON body of the entries that carry a line.
func encode(entries []entry) (raw []byte, lines int) {
	var buf bytes.Buffer
	for _, e := range entries {
		if e.line == nil {
			continue
		}
		buf.Write(e.line)
		buf.WriteByte('\n')
		lines++
	}
	return buf.Bytes(), lines
}

func gzipBytes(raw []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// post sends one gzipped NDJSON body. On 401 it forces one token refresh and
// retries once.
func (fw *Forwarder) post(ctx context.Context, raw []byte) (*IngestResult, error) {
	body, err := gzipBytes(raw)
	if err != nil {
		return nil, err
	}
	tok, err := fw.opts.Tokens.Token(ctx)
	if err != nil {
		return nil, tokenError(err)
	}
	status, respBody, err := fw.do(ctx, tok, body)
	if err != nil {
		return nil, err
	}
	if status == http.StatusUnauthorized {
		fw.logf("brainsrv rejected the access token (401); refreshing it")
		tok2, terr := fw.opts.Tokens.ForceRefresh(ctx, tok)
		if terr != nil {
			return nil, tokenError(terr)
		}
		status, respBody, err = fw.do(ctx, tok2, body)
		if err != nil {
			return nil, err
		}
		if status == http.StatusUnauthorized {
			return nil, &sendError{kind: kindPause, reason: ReasonUnauthorized, status: status, msg: "brainsrv rejected a freshly refreshed token: " + snippet(respBody)}
		}
	}
	switch {
	case status == http.StatusOK:
		var res IngestResult
		if err := json.Unmarshal(respBody, &res); err != nil {
			// The batch is committed on brainsrv's side; counts are cosmetic.
			fw.logf("could not decode the ingest response: %v", err)
		}
		return &res, nil
	case status == http.StatusForbidden:
		return nil, &sendError{kind: kindPause, reason: ReasonScopeDenied, status: status, msg: snippet(respBody)}
	case status == http.StatusRequestEntityTooLarge:
		return nil, &sendError{kind: kindTooLarge, status: status, msg: snippet(respBody)}
	case status >= 300 && status < 400:
		// Redirects are refused (brain.NoRedirects): the token and batch
		// only go to the configured brainsrv_url.
		return nil, &sendError{kind: kindClient, status: status, msg: "brainsrv answered with a redirect, which afferent does not follow; set brainsrv_url to the final address (" + snippet(respBody) + ")"}
	case status >= 400 && status < 500:
		return nil, &sendError{kind: kindClient, status: status, msg: snippet(respBody)}
	default:
		return nil, &sendError{kind: kindTransient, status: status, msg: snippet(respBody)}
	}
}

func (fw *Forwarder) do(ctx context.Context, token string, body []byte) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fw.opts.URL+IngestPath, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/x-ndjson")
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set("X-Context", fw.opts.Context)
	req.Header.Set("X-Scope", fw.scope)
	req.Header.Set("User-Agent", "afferent-forwarder")
	resp, err := fw.opts.HTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return 0, nil, ctx.Err()
		}
		return 0, nil, &sendError{kind: kindTransient, err: err}
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil && resp.StatusCode == http.StatusOK {
		return 0, nil, &sendError{kind: kindTransient, err: fmt.Errorf("read ingest response: %w", err)}
	}
	if resp.StatusCode/100 == 3 {
		b = []byte("Location: " + resp.Header.Get("Location"))
	}
	return resp.StatusCode, b, nil
}

func tokenError(err error) error {
	if errors.Is(err, auth.ErrLoginRequired) {
		return &sendError{kind: kindPause, reason: ReasonLoginRequired, err: err}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return &sendError{kind: kindTransient, err: fmt.Errorf("get access token: %w", err)}
}

// snippet trims a response body for logs and the status file.
func snippet(b []byte) string {
	s := strings.Join(strings.Fields(string(b)), " ")
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}
