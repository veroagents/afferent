// Package mcpproxy is `afferent mcp proxy`: a stdio MCP server that relays
// every JSON-RPC message to brainsrv's streamable-HTTP MCP endpoint (/mcp) as
// the signed-in user.
//
// Agents (Claude Code, Cursor, Codex) speak MCP over stdio: one JSON-RPC
// message per line on stdin and stdout. brainsrv speaks MCP over HTTP: each
// message is a POST, answered with application/json or a text/event-stream,
// and a session id (Mcp-Session-Id) assigned on initialize.
//
// brainsrv binds a session to the sha256 of the bearer token it was opened
// with, so a session dies whenever the access token is refreshed (every ~15
// minutes). The proxy hides that: it remembers the client's initialize
// params and, when the token changes or brainsrv rejects the session (401
// after a refresh, 403 or 404), it opens a new session with the same params
// (initialize + notifications/initialized, whose answers are discarded) and
// sends the original message again, once.
//
// stdout carries only protocol messages; diagnostics go to Logf (stderr).
package mcpproxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/afferent/auth"
)

// Path is brainsrv's MCP endpoint.
const Path = "/mcp"

// Header names of the streamable-HTTP transport.
const (
	HeaderSession  = "Mcp-Session-Id"
	HeaderProtocol = "Mcp-Protocol-Version"
)

// JSON-RPC error codes the proxy produces itself.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeInternal       = -32603
	// CodeUnavailable is brainsrv unreachable or failing (a server error
	// in the implementation-defined range).
	CodeUnavailable = -32000
	// CodeAuth is "not signed in" or a rejected credential or scope.
	CodeAuth = -32001
)

// DefaultMaxMessage bounds one stdin line and one brainsrv answer.
const DefaultMaxMessage = 32 << 20

// Tokens hands out access tokens. auth.TokenSource implements it.
type Tokens interface {
	Token(ctx context.Context) (string, error)
	ForceRefresh(ctx context.Context, rejected string) (string, error)
}

// Options configure a Proxy.
type Options struct {
	URL     string // brainsrv base URL
	Context string // X-Context
	// Scope is X-Scope, the member base scope. When empty, Resolve is asked
	// on the first message (and again until it answers).
	Scope string
	// Resolve looks the scope up (brainsrv /v1/whoami). It is also asked
	// after a 403, in case the member's grant moved. Nil when the scope was
	// configured.
	Resolve    func(ctx context.Context) (string, error)
	Tokens     Tokens
	HTTP       *http.Client
	Logf       func(format string, args ...any)
	MaxMessage int
	// UserAgent is sent on every request.
	UserAgent string
}

// Proxy relays one stdio MCP client to brainsrv.
type Proxy struct {
	opts Options

	outMu sync.Mutex
	out   io.Writer

	scopeMu sync.Mutex
	scope   string

	// mu guards the session. It is held across a re-initialize, so
	// concurrent requests that all see a dead session open one new one.
	mu           sync.Mutex
	initParams   json.RawMessage // the client's initialize params
	sessionID    string
	sessionTok   string // the token the session was opened with
	sessionScope string
	protocol     string // negotiated protocolVersion
	gen          int    // bumped on every new session
	reinits      int

	wg sync.WaitGroup
}

// New validates opts.
func New(opts Options) (*Proxy, error) {
	if opts.URL == "" || opts.Context == "" {
		return nil, errors.New("mcp proxy: brainsrv URL and Context are required")
	}
	if opts.Scope == "" && opts.Resolve == nil {
		return nil, errors.New("mcp proxy: a Scope or a way to look it up (Resolve) is required")
	}
	if opts.Tokens == nil {
		return nil, errors.New("mcp proxy: Tokens is required")
	}
	opts.URL = strings.TrimRight(opts.URL, "/")
	if opts.HTTP == nil {
		// No overall timeout: a tool call answered over SSE may take a
		// while. Requests end with the context.
		opts.HTTP = &http.Client{}
	}
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}
	if opts.MaxMessage <= 0 {
		opts.MaxMessage = DefaultMaxMessage
	}
	if opts.UserAgent == "" {
		opts.UserAgent = "afferent-mcp-proxy"
	}
	return &Proxy{opts: opts, scope: strings.TrimSpace(opts.Scope)}, nil
}

func (p *Proxy) logf(format string, args ...any) { p.opts.Logf(format, args...) }

// message is the part of a JSON-RPC message the proxy looks at.
type message struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  json.RawMessage `json:"error,omitempty"`
}

func (m *message) isRequest() bool      { return m.Method != "" && len(m.ID) > 0 }
func (m *message) isNotification() bool { return m.Method != "" && len(m.ID) == 0 }
func (m *message) isResponse() bool {
	return m.Method == "" && len(m.ID) > 0 && (len(m.Result) > 0 || len(m.Error) > 0)
}

// idKey normalizes an id for matching ("1" and 1 stay different).
func idKey(id json.RawMessage) string {
	var b bytes.Buffer
	if json.Compact(&b, id) != nil {
		return string(id)
	}
	return b.String()
}

// Serve reads messages from in until EOF or ctx ends, relaying each to
// brainsrv and writing every answer to out as one line. It returns nil at
// EOF, after in-flight requests finish.
func (p *Proxy) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	p.out = out
	// A read from stdin does not end with ctx; closing it does (for a
	// signal while the agent is idle).
	if c, ok := in.(io.Closer); ok {
		done := make(chan struct{})
		defer close(done)
		go func() {
			select {
			case <-ctx.Done():
				c.Close()
			case <-done:
			}
		}()
	}
	br := bufio.NewReaderSize(in, 64<<10)
	var rerr error
	for {
		line, tooLong, err := readLine(br, p.opts.MaxMessage)
		if tooLong {
			p.logf("dropped a message over %d bytes", p.opts.MaxMessage)
			p.writeError(nil, CodeParseError, fmt.Sprintf("message larger than %d bytes", p.opts.MaxMessage))
		} else if len(bytes.TrimSpace(line)) > 0 {
			p.handle(ctx, line)
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				rerr = err
			}
			break
		}
		if ctx.Err() != nil {
			break
		}
	}
	p.wg.Wait()
	p.closeSession()
	if ctx.Err() != nil {
		return nil
	}
	return rerr
}

// readLine reads one '\n'-terminated line. A line over max is consumed and
// reported as tooLong without keeping it.
func readLine(br *bufio.Reader, max int) (line []byte, tooLong bool, err error) {
	for {
		chunk, err := br.ReadSlice('\n')
		if !tooLong {
			line = append(line, chunk...)
			if len(line) > max {
				tooLong, line = true, nil
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return line, tooLong, err
	}
}

// handle dispatches one line from the client. Requests other than
// initialize run concurrently; initialize, notifications and responses run
// in order, so brainsrv sees them in the order the client sent them.
func (p *Proxy) handle(ctx context.Context, line []byte) {
	line = bytes.TrimSpace(line)
	if !json.Valid(line) {
		p.writeError(nil, CodeParseError, "parse error: not valid JSON")
		return
	}
	switch line[0] {
	case '[':
		p.handleBatch(ctx, line)
		return
	case '{':
	default:
		p.writeError(nil, CodeInvalidRequest, "invalid request: a JSON-RPC message is an object")
		return
	}
	var m message
	if err := json.Unmarshal(line, &m); err != nil {
		p.writeError(nil, CodeInvalidRequest, "invalid request: "+err.Error())
		return
	}
	switch {
	case m.isRequest() && m.Method == "initialize":
		p.mu.Lock()
		p.initParams = append(json.RawMessage(nil), m.Params...)
		if len(p.initParams) == 0 {
			p.initParams = json.RawMessage(`{}`)
		}
		p.mu.Unlock()
		p.relay(ctx, line, []json.RawMessage{m.ID}, true)
	case m.isRequest():
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			p.relay(ctx, line, []json.RawMessage{m.ID}, false)
		}()
	case m.isNotification(), m.isResponse():
		p.relay(ctx, line, nil, false)
	default:
		var id json.RawMessage
		if len(m.ID) > 0 {
			id = m.ID
		}
		p.writeError(id, CodeInvalidRequest, "invalid request: no method, result or error")
	}
}

func (p *Proxy) handleBatch(ctx context.Context, line []byte) {
	var items []json.RawMessage
	if err := json.Unmarshal(line, &items); err != nil || len(items) == 0 {
		p.writeError(nil, CodeInvalidRequest, "invalid request: empty or malformed batch")
		return
	}
	var ids []json.RawMessage
	for _, it := range items {
		var m message
		if json.Unmarshal(it, &m) == nil && m.isRequest() {
			ids = append(ids, m.ID)
		}
	}
	if len(ids) == 0 {
		p.relay(ctx, line, nil, false)
		return
	}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.relay(ctx, line, ids, false)
	}()
}

// relay sends body and writes the answers. Any id left unanswered gets a
// JSON-RPC error, so the client never waits forever.
func (p *Proxy) relay(ctx context.Context, body []byte, ids []json.RawMessage, isInit bool) {
	answered, err := p.roundTrip(ctx, body, ids, isInit)
	if err == nil {
		return
	}
	if len(ids) == 0 {
		p.logf("could not deliver a notification to brainsrv: %v", err)
		return
	}
	code, msg := errorCode(err)
	for _, id := range ids {
		if !answered[idKey(id)] {
			p.writeError(id, code, msg)
		}
	}
}

// statusError is a non-2xx brainsrv answer.
type statusError struct {
	status int
	body   string
}

func (e *statusError) Error() string {
	if e.body != "" {
		return fmt.Sprintf("brainsrv HTTP %d: %s", e.status, e.body)
	}
	return fmt.Sprintf("brainsrv HTTP %d", e.status)
}

func errorCode(err error) (int, string) {
	var se *statusError
	switch {
	case errors.Is(err, auth.ErrLoginRequired):
		return CodeAuth, "afferent is not signed in; run `afferent login` (" + err.Error() + ")"
	case errors.As(err, &se) && (se.status == http.StatusUnauthorized || se.status == http.StatusForbidden):
		return CodeAuth, "brainsrv refused the request: " + se.Error()
	case errors.As(err, &se) && se.status >= 500:
		return CodeUnavailable, se.Error()
	case errors.As(err, &se):
		return CodeInternal, se.Error()
	default:
		return CodeUnavailable, "brainsrv unreachable: " + err.Error()
	}
}

// roundTrip posts body, recovering once from a dead session or token.
func (p *Proxy) roundTrip(ctx context.Context, body []byte, ids []json.RawMessage, isInit bool) (map[string]bool, error) {
	retried := false
	for {
		tok, err := p.opts.Tokens.Token(ctx)
		if err != nil {
			return nil, err
		}
		scope, err := p.currentScope(ctx)
		if err != nil {
			return nil, err
		}
		sid, gen, pv := "", 0, ""
		if !isInit {
			sid, gen, pv, err = p.ensureSession(ctx, tok, scope)
			if err != nil {
				var se *statusError
				if !retried && errors.As(err, &se) && p.recoverable(ctx, se.status, tok, scope, "") {
					retried = true
					continue
				}
				return nil, fmt.Errorf("re-open the brainsrv MCP session: %w", err)
			}
		}
		resp, err := p.post(ctx, tok, scope, sid, pv, body)
		if err != nil {
			return nil, err
		}
		switch resp.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
			se := &statusError{status: resp.StatusCode, body: readSnippet(resp)}
			if retried || !p.recoverable(ctx, resp.StatusCode, tok, scope, sid) {
				return nil, se
			}
			retried = true
			p.logf("brainsrv answered %d (%s); refreshing the session and retrying once", se.status, se.body)
			if !isInit {
				p.invalidate(gen)
			}
			continue
		}
		answered, err := p.readAnswer(resp, ids, isInit)
		if isInit && resp.StatusCode/100 == 2 {
			p.adoptSession(resp.Header.Get(HeaderSession), tok, scope)
		}
		return answered, err
	}
}

// recoverable prepares a retry after status and reports whether one makes
// sense: a 401 forces a token refresh; a 403 asks brainsrv for the scope
// again; a 404 is only a dead session when one was sent.
func (p *Proxy) recoverable(ctx context.Context, status int, tok, scope, sid string) bool {
	switch status {
	case http.StatusUnauthorized:
		if _, err := p.opts.Tokens.ForceRefresh(ctx, tok); err != nil {
			p.logf("token refresh after a 401 failed: %v", err)
			return false
		}
		return true
	case http.StatusForbidden:
		changed := p.rescope(ctx, scope)
		// A session opened with an older token is refused with 403.
		return changed || sid != ""
	case http.StatusNotFound:
		return sid != ""
	}
	return false
}

// currentScope returns X-Scope, resolving it on first use.
func (p *Proxy) currentScope(ctx context.Context) (string, error) {
	p.scopeMu.Lock()
	defer p.scopeMu.Unlock()
	if p.scope != "" {
		return p.scope, nil
	}
	s, err := p.opts.Resolve(ctx)
	if err != nil {
		return "", err
	}
	if s = strings.TrimSpace(s); s == "" {
		return "", errors.New("brainsrv returned an empty member scope")
	}
	p.scope = s
	return s, nil
}

// rescope asks for the scope again after a 403 and reports whether it
// changed.
func (p *Proxy) rescope(ctx context.Context, old string) bool {
	if p.opts.Resolve == nil {
		return false
	}
	s, err := p.opts.Resolve(ctx)
	if err != nil || strings.TrimSpace(s) == "" || s == old {
		return false
	}
	p.scopeMu.Lock()
	p.scope = s
	p.scopeMu.Unlock()
	p.logf("scope %s was refused; brainsrv now grants %s, switching", old, s)
	return true
}

// adoptSession records the session brainsrv opened for the client's own
// initialize.
func (p *Proxy) adoptSession(sid, tok, scope string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sessionID, p.sessionTok, p.sessionScope = sid, tok, scope
	p.gen++
}

// invalidate forgets session generation gen, unless another request has
// already replaced it.
func (p *Proxy) invalidate(gen int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.gen == gen {
		p.sessionID, p.sessionTok = "", ""
	}
}

// ensureSession returns the session to send with a message, opening a new
// one first when the token or scope changed since it was opened (brainsrv
// would refuse the old one) or it was invalidated. Before the client's own
// initialize there is no session to send.
func (p *Proxy) ensureSession(ctx context.Context, tok, scope string) (sid string, gen int, pv string, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.initParams == nil || (p.sessionID != "" && p.sessionTok == tok && p.sessionScope == scope) {
		return p.sessionID, p.gen, p.protocol, nil
	}
	if err := p.reinitLocked(ctx, tok, scope); err != nil {
		return "", p.gen, "", err
	}
	return p.sessionID, p.gen, p.protocol, nil
}

// reinitLocked opens a new session with the client's initialize params and
// sends notifications/initialized. Both answers are discarded: the client
// already has its own. p.mu is held.
func (p *Proxy) reinitLocked(ctx context.Context, tok, scope string) error {
	p.reinits++
	id := fmt.Sprintf("%q", fmt.Sprintf("afferent-reinit-%d", p.reinits))
	init := []byte(`{"jsonrpc":"2.0","id":` + id + `,"method":"initialize","params":` + string(p.initParams) + `}`)
	resp, err := p.post(ctx, tok, scope, "", "", init)
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return &statusError{status: resp.StatusCode, body: readSnippet(resp)}
	}
	sid := resp.Header.Get(HeaderSession)
	msgs, err := readMessages(resp, []json.RawMessage{json.RawMessage(id)}, p.opts.MaxMessage)
	if err != nil {
		return err
	}
	for _, m := range msgs {
		var r message
		if json.Unmarshal(m, &r) == nil && idKey(r.ID) == idKey(json.RawMessage(id)) && len(r.Error) > 0 {
			return fmt.Errorf("brainsrv refused initialize: %s", string(r.Error))
		}
		if pv := protocolVersion(m, id); pv != "" {
			p.protocol = pv
		}
	}
	p.sessionID, p.sessionTok, p.sessionScope = sid, tok, scope
	p.gen++
	resp, err = p.post(ctx, tok, scope, sid, p.protocol, []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	if err != nil {
		return err
	}
	drain(resp)
	if resp.StatusCode/100 != 2 {
		return &statusError{status: resp.StatusCode}
	}
	p.logf("opened a new brainsrv MCP session")
	return nil
}

// protocolVersion returns result.protocolVersion of the answer to id.
func protocolVersion(msg json.RawMessage, id string) string {
	var r struct {
		ID     json.RawMessage `json:"id"`
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"result"`
	}
	if json.Unmarshal(msg, &r) != nil || idKey(r.ID) != idKey(json.RawMessage(id)) {
		return ""
	}
	return r.Result.ProtocolVersion
}

func (p *Proxy) post(ctx context.Context, tok, scope, sid, pv string, body []byte) (*http.Response, error) {
	return p.request(ctx, http.MethodPost, tok, scope, sid, pv, body)
}

// request sends one message. pv is the negotiated protocol version, sent
// with the session id.
func (p *Proxy) request(ctx context.Context, method, tok, scope, sid, pv string, body []byte) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.opts.URL+Path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("X-Context", p.opts.Context)
	req.Header.Set("X-Scope", scope)
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("User-Agent", p.opts.UserAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if sid != "" {
		req.Header.Set(HeaderSession, sid)
		if pv != "" {
			req.Header.Set(HeaderProtocol, pv)
		}
	}
	return p.opts.HTTP.Do(req)
}

// readAnswer writes brainsrv's answer to the client and reports which ids
// it answered.
func (p *Proxy) readAnswer(resp *http.Response, ids []json.RawMessage, isInit bool) (map[string]bool, error) {
	answered := map[string]bool{}
	if resp.StatusCode/100 != 2 {
		// brainsrv (mcp-go) may put a JSON-RPC error in the body; pass it
		// through when it answers one of ours.
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		for _, m := range splitMessages(b) {
			var r message
			if json.Unmarshal(m, &r) == nil && r.isResponse() && wanted(ids, r.ID) {
				answered[idKey(r.ID)] = true
				p.writeLine(m)
			}
		}
		if len(answered) == len(ids) && len(ids) > 0 {
			return answered, nil
		}
		return answered, &statusError{status: resp.StatusCode, body: snippet(b)}
	}
	msgs, err := readMessages(resp, ids, p.opts.MaxMessage)
	for _, m := range msgs {
		var r message
		if json.Unmarshal(m, &r) == nil && r.isResponse() {
			answered[idKey(r.ID)] = true
			if isInit && len(ids) == 1 {
				if pv := protocolVersion(m, string(ids[0])); pv != "" {
					p.mu.Lock()
					p.protocol = pv
					p.mu.Unlock()
				}
			}
		}
		p.writeLine(m)
	}
	if err != nil {
		return answered, err
	}
	for _, id := range ids {
		if !answered[idKey(id)] {
			return answered, errors.New("brainsrv ended the answer without a response to this request")
		}
	}
	return answered, nil
}

func wanted(ids []json.RawMessage, id json.RawMessage) bool {
	k := idKey(id)
	for _, i := range ids {
		if idKey(i) == k {
			return true
		}
	}
	return false
}

// readMessages returns the JSON-RPC messages in a 2xx answer: a JSON body,
// or the data of each SSE event. An SSE stream is read until every id in
// ids is answered (brainsrv may keep it open), then closed.
func readMessages(resp *http.Response, ids []json.RawMessage, max int) ([]json.RawMessage, error) {
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusAccepted || resp.StatusCode == http.StatusNoContent {
		drain(resp)
		return nil, nil
	}
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if strings.HasPrefix(ct, "text/event-stream") {
		return readSSE(resp.Body, ids, max)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, int64(max)+1))
	if err != nil {
		return nil, err
	}
	if len(b) > max {
		return nil, fmt.Errorf("brainsrv answer larger than %d bytes", max)
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return nil, nil
	}
	if !json.Valid(b) {
		return nil, fmt.Errorf("brainsrv answered with invalid JSON (%s)", snippet(b))
	}
	return splitMessages(b), nil
}

// splitMessages turns a JSON object or array into messages.
func splitMessages(b []byte) []json.RawMessage {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || !json.Valid(b) {
		return nil
	}
	if b[0] == '[' {
		var items []json.RawMessage
		if json.Unmarshal(b, &items) == nil {
			return items
		}
		return nil
	}
	if b[0] == '{' {
		return []json.RawMessage{json.RawMessage(b)}
	}
	return nil
}

// readSSE parses a text/event-stream (data lines joined by "\n" per event;
// comments, id and retry fields ignored).
func readSSE(r io.Reader, ids []json.RawMessage, max int) ([]json.RawMessage, error) {
	pending := map[string]bool{}
	for _, id := range ids {
		pending[idKey(id)] = true
	}
	br := bufio.NewReaderSize(r, 64<<10)
	var (
		out   []json.RawMessage
		data  []byte
		event string
		has   bool
	)
	dispatch := func() {
		defer func() { data, event, has = nil, "", false }()
		if !has || (event != "" && event != "message") {
			return
		}
		for _, m := range splitMessages(data) {
			out = append(out, m)
			var r message
			if json.Unmarshal(m, &r) == nil && r.isResponse() {
				delete(pending, idKey(r.ID))
			}
		}
	}
	for {
		line, tooLong, err := readLine(br, max)
		if tooLong {
			return out, fmt.Errorf("brainsrv sent an event line over %d bytes", max)
		}
		s := strings.TrimRight(string(line), "\r\n")
		switch {
		case s == "":
			if len(line) > 0 { // a blank line ends the event
				dispatch()
				if len(ids) > 0 && len(pending) == 0 {
					return out, nil
				}
			}
		case strings.HasPrefix(s, ":"):
		default:
			field, value, _ := strings.Cut(s, ":")
			value = strings.TrimPrefix(value, " ")
			switch field {
			case "data":
				if has {
					data = append(data, '\n')
				}
				data = append(data, value...)
				has = true
				if len(data) > max {
					return out, fmt.Errorf("brainsrv sent an event over %d bytes", max)
				}
			case "event":
				event = value
			}
		}
		if err != nil {
			dispatch()
			if errors.Is(err, io.EOF) {
				return out, nil
			}
			return out, err
		}
	}
}

// closeSession ends the session with DELETE, best effort.
func (p *Proxy) closeSession() {
	p.mu.Lock()
	sid, tok, scope, pv := p.sessionID, p.sessionTok, p.sessionScope, p.protocol
	p.mu.Unlock()
	if sid == "" || tok == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if resp, err := p.request(ctx, http.MethodDelete, tok, scope, sid, pv, nil); err == nil {
		drain(resp)
	}
}

// writeLine writes one compact JSON message and a newline to the client.
func (p *Proxy) writeLine(m json.RawMessage) {
	var b bytes.Buffer
	if err := json.Compact(&b, m); err != nil {
		p.logf("dropped an invalid message from brainsrv: %v", err)
		return
	}
	b.WriteByte('\n')
	p.outMu.Lock()
	defer p.outMu.Unlock()
	if _, err := p.out.Write(b.Bytes()); err != nil {
		p.logf("write to the client: %v", err)
	}
}

// writeError sends a JSON-RPC error response (id null when unknown).
func (p *Proxy) writeError(id json.RawMessage, code int, msg string) {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	b, _ := json.Marshal(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}{JSONRPC: "2.0", ID: id, Error: struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}{code, msg}})
	p.writeLine(b)
}

func readSnippet(resp *http.Response) string {
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	return snippet(b)
}

func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
}

func snippet(b []byte) string {
	s := strings.Join(strings.Fields(string(b)), " ")
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}
