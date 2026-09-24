package mcpproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/afferent/auth"
)

// fakeMCP is a brainsrv /mcp: Bearer + X-Context + X-Scope, sessions bound
// to the token they were opened with (403 otherwise, 404 when unknown).
type fakeMCP struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	valid    map[string]bool
	scope    string
	sessions map[string]string // sid -> token
	nextSID  int
	sse      bool // answer requests (other than initialize) over SSE
	holdOpen bool // keep SSE streams open after the answer
	reqs     []seen
}

type seen struct {
	Method, ID, SID, Token, Scope, Protocol string
	Params                                  json.RawMessage
	Status                                  int
	HTTPMethod                              string
}

func newFakeMCP(t *testing.T) *fakeMCP {
	f := &fakeMCP{t: t, valid: map[string]bool{"t1": true}, scope: "ws.t.people.u.harness", sessions: map[string]string{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeMCP) record(s seen) {
	f.mu.Lock()
	f.reqs = append(f.reqs, s)
	f.mu.Unlock()
}

func (f *fakeMCP) requests() []seen {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]seen(nil), f.reqs...)
}

func (f *fakeMCP) methods() []string {
	var out []string
	for _, r := range f.requests() {
		if r.Status/100 == 2 {
			out = append(out, r.Method)
		} else {
			out = append(out, fmt.Sprintf("%s=%d", r.Method, r.Status))
		}
	}
	return out
}

func (f *fakeMCP) handle(w http.ResponseWriter, r *http.Request) {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	body, _ := io.ReadAll(r.Body)
	var m struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	_ = json.Unmarshal(body, &m)
	s := seen{Method: m.Method, ID: string(m.ID), SID: r.Header.Get(HeaderSession), Token: tok,
		Scope: r.Header.Get("X-Scope"), Protocol: r.Header.Get(HeaderProtocol), Params: m.Params, HTTPMethod: r.Method}
	fail := func(code int, msg string) {
		s.Status = code
		f.record(s)
		http.Error(w, msg, code)
	}
	f.mu.Lock()
	valid, scope := f.valid[tok], f.scope
	bound, known := f.sessions[s.SID]
	f.mu.Unlock()
	if r.URL.Path != "/mcp" {
		fail(404, "no route")
		return
	}
	if !valid {
		fail(401, "invalid credentials")
		return
	}
	if r.Header.Get("X-Context") != "afferent-poc" || !strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		fail(400, "bad headers")
		return
	}
	if s.Scope != scope {
		fail(403, "scope not granted")
		return
	}
	if s.SID != "" {
		if !known {
			fail(404, "unknown MCP session; re-initialize")
			return
		}
		if bound != tok {
			fail(403, "MCP session is bound to a different scope or credential")
			return
		}
	}
	if r.Method == http.MethodDelete {
		f.mu.Lock()
		delete(f.sessions, s.SID)
		f.mu.Unlock()
		s.Status = 200
		f.record(s)
		return
	}
	switch {
	case m.Method == "initialize":
		f.mu.Lock()
		f.nextSID++
		sid := fmt.Sprintf("sid-%d", f.nextSID)
		f.sessions[sid] = tok
		f.mu.Unlock()
		w.Header().Set(HeaderSession, sid)
		w.Header().Set("Content-Type", "application/json")
		s.Status = 200
		f.record(s)
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2025-06-18","serverInfo":{"name":"brainsrv","version":"x"},"capabilities":{"tools":{}}}}`, m.ID)
	case s.SID == "":
		fail(400, "Mcp-Session-Id required")
	case len(m.ID) == 0: // notification or client response
		s.Status = 202
		f.record(s)
		w.WriteHeader(http.StatusAccepted)
	default:
		s.Status = 200
		f.record(s)
		answer := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"method":%q,"token":%q,"sid":%q}}`, m.ID, m.Method, tok, s.SID)
		f.mu.Lock()
		sse, hold := f.sse, f.holdOpen
		f.mu.Unlock()
		if !sse {
			w.Header().Set("Content-Type", "application/json")
			// Pretty-printed on purpose: the proxy must emit one line.
			var b bytes.Buffer
			_ = json.Indent(&b, []byte(answer), "", "  ")
			w.Write(b.Bytes())
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		fmt.Fprintf(w, ": comment\n\n")
		fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\n")
		fmt.Fprintf(w, "data: \"params\":{\"progress\":1}}\n\n")
		fmt.Fprintf(w, "id: 7\nevent: message\ndata: %s\n\n", answer)
		fl.Flush()
		if hold {
			<-r.Context().Done()
		}
	}
}

func (f *fakeMCP) set(fn func(f *fakeMCP)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

type fakeTokens struct {
	mu     sync.Mutex
	cur    string
	next   string
	err    error
	forced int
}

func (t *fakeTokens) Token(context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.cur, t.err
}

func (t *fakeTokens) ForceRefresh(_ context.Context, rejected string) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.forced++
	if t.err != nil {
		return "", t.err
	}
	if t.cur == rejected && t.next != "" {
		t.cur, t.next = t.next, ""
	}
	return t.cur, nil
}

// syncBuf collects stdout lines.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) lines() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	txt := strings.TrimRight(s.b.String(), "\n")
	if txt == "" {
		return nil
	}
	return strings.Split(txt, "\n")
}

// session drives one Proxy.Serve over a pipe.
type session struct {
	t    *testing.T
	in   *io.PipeWriter
	out  *syncBuf
	done chan error
	logs *syncBuf
}

func start(t *testing.T, opts Options) *session {
	t.Helper()
	logs := &syncBuf{}
	opts.Logf = func(format string, args ...any) { fmt.Fprintf(logs, format+"\n", args...) }
	p, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	pr, pw := io.Pipe()
	s := &session{t: t, in: pw, out: &syncBuf{}, done: make(chan error, 1), logs: logs}
	go func() { s.done <- p.Serve(context.Background(), pr, s.out) }()
	t.Cleanup(func() { pw.Close() })
	return s
}

func (s *session) send(line string) {
	s.t.Helper()
	if _, err := io.WriteString(s.in, line+"\n"); err != nil {
		s.t.Fatal(err)
	}
}

// wait returns once n lines are out.
func (s *session) wait(n int) []string {
	s.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if l := s.out.lines(); len(l) >= n {
			return l
		}
		time.Sleep(5 * time.Millisecond)
	}
	s.t.Fatalf("want %d output lines, have %q\nlogs:\n%s", n, s.out.lines(), s.logs.b.String())
	return nil
}

func (s *session) close() {
	s.t.Helper()
	s.in.Close()
	select {
	case err := <-s.done:
		if err != nil {
			s.t.Fatalf("Serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		s.t.Fatal("Serve did not return after EOF")
	}
}

type reply struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Result map[string]any  `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func parse(t *testing.T, line string) reply {
	t.Helper()
	var r reply
	if err := json.Unmarshal([]byte(line), &r); err != nil {
		t.Fatalf("output line is not JSON: %q", line)
	}
	return r
}

func byID(t *testing.T, lines []string, id string) reply {
	t.Helper()
	for _, l := range lines {
		if r := parse(t, l); string(r.ID) == id {
			return r
		}
	}
	t.Fatalf("no answer with id %s in %q", id, lines)
	return reply{}
}

const initMsg = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","clientInfo":{"name":"claude-code","version":"9"},"capabilities":{}}}`
const initializedMsg = `{"jsonrpc":"2.0","method":"notifications/initialized"}`

func opts(f *fakeMCP, tok *fakeTokens) Options {
	return Options{URL: f.srv.URL + "/", Context: "afferent-poc", Scope: f.scope, Tokens: tok}
}

func TestProxyJSONAndNotification(t *testing.T) {
	f := newFakeMCP(t)
	tok := &fakeTokens{cur: "t1"}
	s := start(t, opts(f, tok))
	s.send(initMsg)
	init := parse(t, s.wait(1)[0])
	if init.Result["protocolVersion"] != "2025-06-18" {
		t.Fatalf("initialize answer %+v", init)
	}
	s.send(initializedMsg)
	s.send(`{"jsonrpc":"2.0","id":"a","method":"tools/list"}`)
	lines := s.wait(2)
	r := byID(t, lines, `"a"`)
	if r.Result["sid"] != "sid-1" || r.Result["token"] != "t1" {
		t.Fatalf("tools/list answer %+v", r)
	}
	for _, l := range lines {
		if strings.Contains(l, "\n") || !json.Valid([]byte(l)) {
			t.Fatalf("not one JSON line: %q", l)
		}
	}
	s.close()
	got := f.methods()
	want := []string{"initialize", "notifications/initialized", "tools/list", ""}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("brainsrv saw %v, want %v (the last is the closing DELETE)", got, want)
	}
	reqs := f.requests()
	if reqs[1].Status != 202 || reqs[1].SID != "sid-1" || reqs[2].Protocol != "2025-06-18" || reqs[3].HTTPMethod != "DELETE" {
		t.Fatalf("requests %+v", reqs)
	}
	if len(s.out.lines()) != 2 {
		t.Fatalf("a notification must not produce output: %q", s.out.lines())
	}
}

func TestProxySSE(t *testing.T) {
	f := newFakeMCP(t)
	f.sse, f.holdOpen = true, true
	tok := &fakeTokens{cur: "t1"}
	s := start(t, opts(f, tok))
	s.send(initMsg)
	s.wait(1)
	s.send(initializedMsg)
	s.send(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"recall"}}`)
	lines := s.wait(3)
	if p := parse(t, lines[1]); p.Method != "notifications/progress" {
		t.Fatalf("SSE notification (split over two data lines) not relayed first: %q", lines)
	}
	if r := byID(t, lines, "2"); r.Result["method"] != "tools/call" {
		t.Fatalf("SSE answer %+v", r)
	}
	// The stream is held open by brainsrv; the proxy stops at the answer.
	s.close()
}

func TestProxyReinitializesWhenTheTokenRotates(t *testing.T) {
	f := newFakeMCP(t)
	tok := &fakeTokens{cur: "t1"}
	s := start(t, opts(f, tok))
	s.send(initMsg)
	s.wait(1)
	s.send(initializedMsg)
	s.send(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	s.wait(2)

	// A normal refresh: the source now hands out t2, and brainsrv would
	// refuse sid-1 with it.
	f.set(func(f *fakeMCP) { f.valid["t2"] = true })
	tok.mu.Lock()
	tok.cur = "t2"
	tok.mu.Unlock()
	s.send(`{"jsonrpc":"2.0","id":3,"method":"tools/list"}`)
	lines := s.wait(3)
	if r := byID(t, lines, "3"); r.Result["sid"] != "sid-2" || r.Result["token"] != "t2" {
		t.Fatalf("answer %+v", r)
	}
	if len(lines) != 3 {
		t.Fatalf("the re-initialize answer must be discarded: %q", lines)
	}
	reqs := f.requests()
	var reinit *seen
	for i := range reqs {
		if reqs[i].Method == "initialize" && reqs[i].Token == "t2" {
			reinit = &reqs[i]
		}
	}
	if reinit == nil || !strings.Contains(string(reinit.Params), `"claude-code"`) || !strings.HasPrefix(reinit.ID, `"afferent-reinit-`) {
		t.Fatalf("re-initialize with the client's params missing: %+v", reqs)
	}
	got := strings.Join(f.methods(), ",")
	if !strings.HasSuffix(got, "initialize,notifications/initialized,tools/list") {
		t.Fatalf("sequence %s", got)
	}
	s.close()
}

func TestProxyRecoversFromLostSessionAnd401(t *testing.T) {
	f := newFakeMCP(t)
	tok := &fakeTokens{cur: "t1", next: "t2"}
	s := start(t, opts(f, tok))
	s.send(initMsg)
	s.wait(1)
	s.send(initializedMsg)

	// brainsrv restarted: every session is unknown (404).
	f.set(func(f *fakeMCP) { f.sessions = map[string]string{} })
	s.send(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if r := byID(t, s.wait(2), "2"); r.Result["sid"] != "sid-2" {
		t.Fatalf("after 404: %+v", r)
	}

	// The token was revoked server-side while it still looked valid: 401,
	// forced refresh, new session, one retry.
	f.set(func(f *fakeMCP) { f.valid = map[string]bool{"t2": true} })
	s.send(`{"jsonrpc":"2.0","id":3,"method":"tools/list"}`)
	if r := byID(t, s.wait(3), "3"); r.Result["token"] != "t2" || r.Result["sid"] != "sid-3" {
		t.Fatalf("after 401: %+v", r)
	}
	if tok.forced != 1 {
		t.Fatalf("ForceRefresh calls %d", tok.forced)
	}

	// Still refused after the one retry: a JSON-RPC error, not a hang.
	f.set(func(f *fakeMCP) { f.valid = map[string]bool{} })
	s.send(`{"jsonrpc":"2.0","id":4,"method":"tools/list"}`)
	r := byID(t, s.wait(4), "4")
	if r.Error == nil || r.Error.Code != CodeAuth {
		t.Fatalf("want an auth error, got %+v", r)
	}
	s.close()
}

func TestProxyScopeResolvedAndRescopedOn403(t *testing.T) {
	f := newFakeMCP(t)
	tok := &fakeTokens{cur: "t1"}
	scopes := []string{"ws.old.harness", f.scope}
	calls := 0
	o := opts(f, tok)
	o.Scope = ""
	o.Resolve = func(context.Context) (string, error) {
		s := scopes[min(calls, len(scopes)-1)]
		calls++
		return s, nil
	}
	s := start(t, o)
	s.send(initMsg)
	if r := parse(t, s.wait(1)[0]); r.Result["protocolVersion"] == nil {
		t.Fatalf("initialize after rescope: %+v", r)
	}
	if calls != 2 {
		t.Fatalf("Resolve calls %d", calls)
	}
	s.close()
}

func TestProxyBadInputAndSignedOut(t *testing.T) {
	f := newFakeMCP(t)
	tok := &fakeTokens{cur: "t1"}
	s := start(t, opts(f, tok))
	s.send(`this is not json`)
	s.send(`[]`)
	s.send(`42`)
	s.send(`{"jsonrpc":"2.0","id":9}`)
	s.send(`   `)
	s.send(`{"jsonrpc":"2.0","id":5,"method":"tools/list"`) // truncated
	lines := s.wait(5)
	codes := []int{}
	for _, l := range lines {
		r := parse(t, l)
		if r.Error == nil {
			t.Fatalf("expected errors only: %q", lines)
		}
		codes = append(codes, r.Error.Code)
	}
	want := []int{CodeParseError, CodeInvalidRequest, CodeInvalidRequest, CodeInvalidRequest, CodeParseError}
	if fmt.Sprint(codes) != fmt.Sprint(want) {
		t.Fatalf("codes %v want %v", codes, want)
	}
	if r := parse(t, lines[3]); string(r.ID) != "9" {
		t.Fatalf("invalid request should echo its id: %q", lines[3])
	}

	// Signed out: a request gets an error that says what to do.
	tok.mu.Lock()
	tok.err = fmt.Errorf("session ended: %w", auth.ErrLoginRequired)
	tok.mu.Unlock()
	s.send(initMsg)
	r := parse(t, s.wait(6)[5])
	if r.Error == nil || r.Error.Code != CodeAuth || !strings.Contains(r.Error.Message, "afferent login") {
		t.Fatalf("signed out: %+v", r)
	}
	// Still alive: signed back in, it works.
	tok.mu.Lock()
	tok.err = nil
	tok.mu.Unlock()
	s.send(initMsg)
	if r := parse(t, s.wait(7)[6]); r.Result["protocolVersion"] == nil {
		t.Fatalf("after sign-in: %+v", r)
	}
	s.close()
}

func TestProxyOversizedLineAndBrainsrvDown(t *testing.T) {
	f := newFakeMCP(t)
	tok := &fakeTokens{cur: "t1"}
	o := opts(f, tok)
	o.MaxMessage = 64
	s := start(t, o)
	s.send(`{"jsonrpc":"2.0","id":1,"method":"x","params":"` + strings.Repeat("a", 200) + `"}`)
	if r := parse(t, s.wait(1)[0]); r.Error == nil || r.Error.Code != CodeParseError {
		t.Fatalf("oversized: %q", s.out.lines())
	}
	f.srv.Close()
	s.send(`{"jsonrpc":"2.0","id":2,"method":"ping"}`)
	if r := parse(t, s.wait(2)[1]); r.Error == nil || r.Error.Code != CodeUnavailable {
		t.Fatalf("down: %q", s.out.lines())
	}
	s.close()
}

func TestReadSSEStopsAtAnswer(t *testing.T) {
	pr, pw := io.Pipe()
	go func() {
		io.WriteString(pw, "data: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}\r\n\r\n")
		// never closed: readSSE must return on its own
	}()
	done := make(chan struct{})
	go func() {
		msgs, err := readSSE(pr, []json.RawMessage{json.RawMessage("1")}, 1<<20)
		if err != nil || len(msgs) != 1 {
			t.Errorf("readSSE %v %q", err, msgs)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("readSSE waited for the stream to end")
	}
	pw.Close()
}

func TestNewValidates(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("empty options accepted")
	}
	if _, err := New(Options{URL: "http://x", Context: "c", Tokens: &fakeTokens{}}); err == nil {
		t.Fatal("no scope and no resolver accepted")
	}
	var se *statusError
	if code, _ := errorCode(&statusError{status: 502}); code != CodeUnavailable || errors.As(errors.New("x"), &se) {
		t.Fatal("errorCode")
	}
}

func TestServeReturnsWhenCancelledWhileIdle(t *testing.T) {
	f := newFakeMCP(t)
	p, err := New(opts(f, &fakeTokens{cur: "t1"}))
	if err != nil {
		t.Fatal(err)
	}
	pr, pw := io.Pipe()
	defer pw.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Serve(ctx, pr, io.Discard) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve after cancel: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve kept waiting on stdin after cancel")
	}
}
