// Package ui is `afferent ui`: a local, loopback-only web page that shows
// what brainsrv knows for the signed-in member (a treemap of the scope tree,
// the knowledge graph, recall search, sessions and turns) and the forwarder's
// status.
//
// The browser never sees the afferent access token. The page talks only to
// this server's whitelisted /api routes; the server adds the bearer token
// from the refreshing token source and calls brainsrv itself.
//
// Security model (the server runs on the user's machine, next to every other
// local process and every web page the user visits):
//   - It listens on a loopback address only.
//   - Every /api call must carry the per-launch random key (32 bytes, handed
//     to the page in the URL fragment, which browsers never send to servers)
//     in the X-Afferent-UI-Key header. Other local users and web pages do not
//     know it.
//   - The Host header must be exactly 127.0.0.1:<port> or localhost:<port>
//     (or [::1]:<port>), which defeats DNS rebinding.
//   - No CORS headers, a strict CSP with no inline script, frame-ancestors
//     'none', nosniff and no-referrer.
//   - Request bodies are capped at 64 KiB; only the listed routes exist, and
//     no path is proxied as is.
//   - Errors are sanitized: brainsrv bodies are cut short, and the token is
//     redacted from anything written to the browser.
package ui

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/afferent/auth"
	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/afferent/brain"
)

//go:embed static/index.html static/app.js static/mapdata.js static/app.css static/d3.v7.min.js
var staticFS embed.FS

// KeyHeader carries the per-launch key on every /api request.
const KeyHeader = "X-Afferent-UI-Key"

// MaxBody is the largest request body the server reads.
const MaxBody = 64 << 10

// maxBrainBody is the largest brainsrv response relayed to the page.
const maxBrainBody = 16 << 20

// CSP is the Content-Security-Policy on every response.
const CSP = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; " +
	"connect-src 'self'; font-src 'self'; object-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// Config is what the server needs from the CLI.
type Config struct {
	BrainsrvURL string
	Context     string
	HTTP        *http.Client
	// Token returns a valid access token (auth.ErrLoginRequired when signed
	// out).
	Token func(ctx context.Context) (string, error)
	// ForceRefresh returns a new token after brainsrv rejected one with 401;
	// nil disables the retry.
	ForceRefresh func(ctx context.Context, rejected string) (string, error)
	// Scope returns the member scope (X-Scope), resolved like the
	// forwarder's.
	Scope func(ctx context.Context) (string, error)
	// Status returns the /api/status document.
	Status func(ctx context.Context) any
	// Timeout bounds each brainsrv call (default 30s).
	Timeout time.Duration
}

// Server is one `afferent ui` launch.
type Server struct {
	cfg   Config
	key   string
	ln    net.Listener
	host  string // for the URL
	hosts map[string]bool
	port  int

	mu         sync.Mutex
	scope      string
	scopeUntil time.Time
}

// ErrNotLoopback refuses a listen address other than loopback.
var ErrNotLoopback = errors.New("afferent ui listens on loopback only (127.0.0.1, ::1 or localhost)")

// LoopbackHost validates --addr and returns the IP literal to listen on.
func LoopbackHost(addr string) (string, error) {
	a := strings.TrimSpace(addr)
	a = strings.TrimSuffix(strings.TrimPrefix(a, "["), "]")
	if strings.EqualFold(a, "localhost") || a == "" {
		return "127.0.0.1", nil
	}
	ip := net.ParseIP(a)
	if ip == nil || !ip.IsLoopback() {
		return "", fmt.Errorf("%w: %q", ErrNotLoopback, addr)
	}
	return ip.String(), nil
}

// New listens on addr:port (port 0 picks a free one) and makes the key.
func New(cfg Config, addr string, port int) (*Server, error) {
	host, err := LoopbackHost(addr)
	if err != nil {
		return nil, err
	}
	if port < 0 || port > 65535 {
		return nil, fmt.Errorf("invalid port %d", port)
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return nil, err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		ln.Close()
		return nil, err
	}
	p := ln.Addr().(*net.TCPAddr).Port
	s := &Server{cfg: cfg, key: base64.RawURLEncoding.EncodeToString(raw), ln: ln, port: p, host: host}
	ps := strconv.Itoa(p)
	s.hosts = map[string]bool{"127.0.0.1:" + ps: true, "localhost:" + ps: true}
	if host != "127.0.0.1" {
		s.hosts[net.JoinHostPort(host, ps)] = true
	}
	if s.cfg.Timeout <= 0 {
		s.cfg.Timeout = 30 * time.Second
	}
	return s, nil
}

// URL is the page address, with the key in the fragment.
func (s *Server) URL() string {
	return "http://" + net.JoinHostPort(s.host, strconv.Itoa(s.port)) + "/#k=" + s.key
}

// Key is the per-launch key (for tests).
func (s *Server) Key() string { return s.key }

// Addr is host:port as served.
func (s *Server) Addr() string { return net.JoinHostPort(s.host, strconv.Itoa(s.port)) }

// Close stops listening (when Serve is not used).
func (s *Server) Close() error { return s.ln.Close() }

// Serve runs until ctx is done.
func (s *Server) Serve(ctx context.Context) error {
	hs := &http.Server{
		Handler:           s,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
	errc := make(chan error, 1)
	go func() { errc <- hs.Serve(s.ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = hs.Shutdown(sctx)
		return nil
	}
}

func securityHeaders(h http.Header) {
	h.Set("Content-Security-Policy", CSP)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
	h.Set("Cache-Control", "no-store")
}

var static = map[string]struct{ file, ctype string }{
	"/":             {"static/index.html", "text/html; charset=utf-8"},
	"/index.html":   {"static/index.html", "text/html; charset=utf-8"},
	"/app.js":       {"static/app.js", "text/javascript; charset=utf-8"},
	"/mapdata.js":   {"static/mapdata.js", "text/javascript; charset=utf-8"},
	"/app.css":      {"static/app.css", "text/css; charset=utf-8"},
	"/d3.v7.min.js": {"static/d3.v7.min.js", "text/javascript; charset=utf-8"},
}

// ServeHTTP is the whole router.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	securityHeaders(w.Header())
	if !s.hosts[r.Host] {
		writeErr(w, http.StatusForbidden, "bad_host", "forbidden host")
		return
	}
	// A browser marks cross-site requests; nothing cross-site is served.
	if o := r.Header.Get("Origin"); o != "" && !s.hosts[strings.TrimPrefix(o, "http://")] {
		writeErr(w, http.StatusForbidden, "bad_origin", "forbidden origin")
		return
	}
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		writeErr(w, http.StatusForbidden, "bad_origin", "forbidden origin")
		return
	}
	if r.ContentLength > MaxBody {
		writeErr(w, http.StatusRequestEntityTooLarge, "too_large", "request body too large")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, MaxBody)
	if strings.HasPrefix(r.URL.Path, "/api/") {
		s.api(w, r)
		return
	}
	f, ok := static[r.URL.Path]
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found", "not found")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeErr(w, http.StatusMethodNotAllowed, "method", "method not allowed")
		return
	}
	b, err := fs.ReadFile(staticFS, f.file)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "missing asset")
		return
	}
	w.Header().Set("Content-Type", f.ctype)
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(b)
}

var uuidRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// scopeRE is an ltree path of brainsrv labels.
var scopeRE = regexp.MustCompile(`^[A-Za-z0-9_-]+(\.[A-Za-z0-9_-]+)*$`)

func (s *Server) api(w http.ResponseWriter, r *http.Request) {
	got := r.Header.Get(KeyHeader)
	if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(s.key)) != 1 {
		writeErr(w, http.StatusUnauthorized, "bad_key", "missing or wrong UI key; reopen the link `afferent ui` printed")
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/"), "/")
	route := func(method string, h func()) {
		if r.Method != method {
			writeErr(w, http.StatusMethodNotAllowed, "method", "method not allowed")
			return
		}
		h()
	}
	switch {
	case len(parts) == 1 && parts[0] == "status":
		route(http.MethodGet, func() { s.status(w, r) })
	case len(parts) == 1 && parts[0] == "overview":
		route(http.MethodGet, func() { s.overview(w, r) })
	case len(parts) == 1 && parts[0] == "graph":
		route(http.MethodGet, func() { s.graph(w, r) })
	case len(parts) == 1 && parts[0] == "recall":
		route(http.MethodPost, func() { s.recall(w, r) })
	case len(parts) == 2 && parts[0] == "entities":
		route(http.MethodGet, func() { s.entity(w, r, parts[1]) })
	case len(parts) == 1 && parts[0] == "sessions":
		route(http.MethodGet, func() { s.sessions(w, r) })
	case len(parts) == 3 && parts[0] == "sessions" && parts[2] == "turns":
		route(http.MethodGet, func() { s.turns(w, r, parts[1]) })
	default:
		writeErr(w, http.StatusNotFound, "not_found", "not found")
	}
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	var v any = map[string]any{}
	if s.cfg.Status != nil {
		v = s.cfg.Status(r.Context())
	}
	b, err := json.Marshal(v)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "status unavailable")
		return
	}
	writeRaw(w, http.StatusOK, s.redactCurrent(r.Context(), b))
}

// intParam reads an optional integer query parameter within [lo, hi].
func intParam(q url.Values, name string, def, lo, hi int) (int, error) {
	v := q.Get(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < lo || n > hi {
		return 0, fmt.Errorf("%s must be an integer from %d to %d", name, lo, hi)
	}
	return n, nil
}

func validAt(q url.Values) (string, error) {
	v := q.Get("valid_at")
	if v == "" {
		return "", nil
	}
	if _, err := time.Parse(time.RFC3339, v); err != nil {
		return "", errors.New("valid_at must be RFC3339")
	}
	return v, nil
}

func (s *Server) overview(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	depth, err := intParam(q, "depth", 3, 1, 4)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	va, err := validAt(q)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	scope, ok := s.memberScope(w, r)
	if !ok {
		return
	}
	out := url.Values{"depth": {strconv.Itoa(depth)}}
	if va != "" {
		out.Set("valid_at", va)
	}
	s.relay(w, r, http.MethodGet, "/v1/overview", out, scope, nil)
}

func (s *Server) graph(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, err := intParam(q, "limit", 150, 1, 500)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	va, err := validAt(q)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	member, ok := s.memberScope(w, r)
	if !ok {
		return
	}
	out := url.Values{"limit": {strconv.Itoa(limit)}}
	if sc := q.Get("scope"); sc != "" {
		if err := underScope(sc, member); err != nil {
			writeErr(w, http.StatusBadRequest, "bad_scope", err.Error())
			return
		}
		out.Set("scope", sc)
	}
	if va != "" {
		out.Set("valid_at", va)
	}
	s.relay(w, r, http.MethodGet, "/v1/graph", out, member, nil)
}

func (s *Server) recall(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Query string `json:"query"`
		K     int    `json:"k"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeErr(w, http.StatusRequestEntityTooLarge, "too_large", "request body too large")
			return
		}
		writeErr(w, http.StatusBadRequest, "bad_request", "body must be {\"query\": string, \"k\": number}")
		return
	}
	in.Query = strings.TrimSpace(in.Query)
	if in.Query == "" || len(in.Query) > 4000 {
		writeErr(w, http.StatusBadRequest, "bad_request", "query must be 1 to 4000 characters")
		return
	}
	if in.K == 0 {
		in.K = 20
	}
	if in.K < 1 || in.K > 100 {
		writeErr(w, http.StatusBadRequest, "bad_request", "k must be from 1 to 100")
		return
	}
	scope, ok := s.memberScope(w, r)
	if !ok {
		return
	}
	body, _ := json.Marshal(map[string]any{"query": in.Query, "k": in.K, "mode": "memories"})
	s.relay(w, r, http.MethodPost, "/v1/recall", nil, scope, body)
}

func (s *Server) entity(w http.ResponseWriter, r *http.Request, id string) {
	if !uuidRE.MatchString(id) {
		writeErr(w, http.StatusBadRequest, "bad_id", "entity id must be a UUID")
		return
	}
	scope, ok := s.memberScope(w, r)
	if !ok {
		return
	}
	s.relay(w, r, http.MethodGet, "/v1/entities/"+strings.ToLower(id), nil, scope, nil)
}

func (s *Server) sessions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, err := intParam(q, "limit", 50, 1, 500)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	member, ok := s.memberScope(w, r)
	if !ok {
		return
	}
	scope := member
	if sc := q.Get("scope"); sc != "" {
		if err := underScope(sc, member); err != nil {
			writeErr(w, http.StatusBadRequest, "bad_scope", err.Error())
			return
		}
		scope = sc
	}
	s.relay(w, r, http.MethodGet, "/v1/sessions", url.Values{"limit": {strconv.Itoa(limit)}}, scope, nil)
}

func (s *Server) turns(w http.ResponseWriter, r *http.Request, id string) {
	if !uuidRE.MatchString(id) {
		writeErr(w, http.StatusBadRequest, "bad_id", "session id must be a UUID")
		return
	}
	scope, ok := s.memberScope(w, r)
	if !ok {
		return
	}
	s.relay(w, r, http.MethodGet, "/v1/sessions/"+strings.ToLower(id)+"/turns", nil, scope, nil)
}

// underScope checks that scope is a well-formed ltree path at or under
// member.
func underScope(scope, member string) error {
	if len(scope) > 1024 || !scopeRE.MatchString(scope) {
		return errors.New("scope must be a dotted label path")
	}
	if scope != member && !strings.HasPrefix(scope, member+".") {
		return fmt.Errorf("scope must be at or under your member scope %s", member)
	}
	return nil
}

// memberScope resolves X-Scope, caching it for a minute. On failure it
// writes the error and returns false.
func (s *Server) memberScope(w http.ResponseWriter, r *http.Request) (string, bool) {
	s.mu.Lock()
	if s.scope != "" && time.Now().Before(s.scopeUntil) {
		sc := s.scope
		s.mu.Unlock()
		return sc, true
	}
	s.mu.Unlock()
	if s.cfg.Scope == nil {
		writeErr(w, http.StatusBadGateway, "no_scope", "member scope unknown")
		return "", false
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.Timeout)
	defer cancel()
	sc, err := s.cfg.Scope(ctx)
	if err == nil && sc == "" {
		err = errors.New("member scope unknown")
	}
	if err != nil {
		if errors.Is(err, auth.ErrLoginRequired) {
			writeErr(w, http.StatusUnauthorized, "signed_out", "not signed in; run `afferent login`")
			return "", false
		}
		var ne net.Error
		if errors.As(err, &ne) {
			writeErr(w, http.StatusBadGateway, "brainsrv_down", "brainsrv is unreachable at "+s.cfg.BrainsrvURL)
			return "", false
		}
		writeErr(w, http.StatusBadGateway, "no_scope", s.redactCurrentString(r.Context(), clip(err.Error(), 400)))
		return "", false
	}
	s.mu.Lock()
	s.scope, s.scopeUntil = sc, time.Now().Add(time.Minute)
	s.mu.Unlock()
	return sc, true
}

func (s *Server) forgetScope() {
	s.mu.Lock()
	s.scope = ""
	s.mu.Unlock()
}

// relay calls brainsrv and writes its JSON answer, or a sanitized error.
func (s *Server) relay(w http.ResponseWriter, r *http.Request, method, path string, q url.Values, scope string, body []byte) {
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.Timeout)
	defer cancel()
	tok, err := s.cfg.Token(ctx)
	if err != nil {
		s.tokenErr(w, err)
		return
	}
	status, resp, err := s.call(ctx, tok, method, path, q, scope, body)
	if err == nil && status == http.StatusUnauthorized && s.cfg.ForceRefresh != nil {
		if nt, ferr := s.cfg.ForceRefresh(ctx, tok); ferr == nil {
			tok = nt
			status, resp, err = s.call(ctx, tok, method, path, q, scope, body)
		} else if errors.Is(ferr, auth.ErrLoginRequired) {
			s.tokenErr(w, ferr)
			return
		}
	}
	if err != nil {
		if ctx.Err() != nil && r.Context().Err() == nil {
			writeErr(w, http.StatusGatewayTimeout, "brainsrv_down", "brainsrv did not answer in time")
			return
		}
		writeErr(w, http.StatusBadGateway, "brainsrv_down", "brainsrv is unreachable at "+s.cfg.BrainsrvURL)
		return
	}
	resp = redact(resp, tok)
	switch {
	case status >= 200 && status < 300:
		if !json.Valid(resp) {
			writeErr(w, http.StatusBadGateway, "brainsrv_error", "brainsrv answered with something that is not JSON")
			return
		}
		writeRaw(w, http.StatusOK, resp)
	case status >= 300 && status < 400:
		writeErr(w, http.StatusBadGateway, "brainsrv_redirect", fmt.Sprintf("brainsrv answered with a redirect (HTTP %d), which is refused", status))
	case status == http.StatusUnauthorized:
		writeErr(w, http.StatusUnauthorized, "unauthorized", "brainsrv refused the token (HTTP 401); try `afferent login`")
	case status == http.StatusForbidden:
		s.forgetScope()
		writeErr(w, http.StatusForbidden, "forbidden", "brainsrv denied access to this scope: "+brainMsg(resp))
	case status == http.StatusNotFound || status == http.StatusMethodNotAllowed:
		code := "not_found"
		if path == "/v1/overview" || path == "/v1/graph" {
			code = "unsupported"
		}
		writeErr(w, http.StatusNotFound, code, fmt.Sprintf("brainsrv HTTP %d: %s", status, brainMsg(resp)))
	case status >= 400 && status < 500:
		writeErr(w, http.StatusBadRequest, "brainsrv_rejected", fmt.Sprintf("brainsrv HTTP %d: %s", status, brainMsg(resp)))
	default:
		writeErr(w, http.StatusBadGateway, "brainsrv_error", fmt.Sprintf("brainsrv HTTP %d: %s", status, brainMsg(resp)))
	}
}

func (s *Server) tokenErr(w http.ResponseWriter, err error) {
	if errors.Is(err, auth.ErrLoginRequired) {
		writeErr(w, http.StatusUnauthorized, "signed_out", "not signed in; run `afferent login`")
		return
	}
	writeErr(w, http.StatusBadGateway, "token", "could not get an access token: "+clip(err.Error(), 300))
}

func (s *Server) call(ctx context.Context, tok, method, path string, q url.Values, scope string, body []byte) (int, []byte, error) {
	u := strings.TrimRight(s.cfg.BrainsrvURL, "/") + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if s.cfg.Context != "" {
		req.Header.Set("X-Context", s.cfg.Context)
	}
	req.Header.Set("X-Scope", scope)
	resp, err := brain.NoRedirects(s.cfg.HTTP).Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBrainBody+1))
	if err != nil {
		return 0, nil, err
	}
	if len(b) > maxBrainBody {
		return http.StatusBadGateway, []byte(`{"error":"response too large"}`), nil
	}
	return resp.StatusCode, b, nil
}

// brainMsg is a short, printable version of a brainsrv error body.
func brainMsg(b []byte) string {
	var e struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(b, &e) == nil && (e.Error != "" || e.Message != "") {
		return clip(firstNonEmpty(e.Error, e.Message), 300)
	}
	s := strings.Map(func(r rune) rune {
		if r < 0x20 && r != ' ' {
			return ' '
		}
		return r
	}, string(b))
	s = strings.TrimSpace(s)
	if s == "" {
		return "no details"
	}
	return clip(s, 300)
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

var bearerRE = regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._~+/=-]+`)

func redact(b []byte, tok string) []byte {
	if tok != "" && bytes.Contains(b, []byte(tok)) {
		b = bytes.ReplaceAll(b, []byte(tok), []byte("[redacted]"))
	}
	return b
}

// redactCurrent removes the current token (if there is one) from b.
func (s *Server) redactCurrent(ctx context.Context, b []byte) []byte {
	if s.cfg.Token == nil {
		return b
	}
	tok, err := s.cfg.Token(ctx)
	if err != nil {
		return b
	}
	return redact(b, tok)
}

func (s *Server) redactCurrentString(ctx context.Context, m string) string {
	return bearerRE.ReplaceAllString(string(s.redactCurrent(ctx, []byte(m))), "Bearer [redacted]")
}

func writeRaw(w http.ResponseWriter, status int, b []byte) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	msg = bearerRE.ReplaceAllString(msg, "Bearer [redacted]")
	b, _ := json.Marshal(map[string]string{"error": msg, "code": code})
	writeRaw(w, status, b)
}
