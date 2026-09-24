package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/afferent/afferenttest"
	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/afferent/auth"
)

const (
	member = "ws.dev.people.drew.harness"
	secret = "tok-SECRET-abc123.def456"
)

type fixture struct {
	t     *testing.T
	brain *afferenttest.Brain
	srv   *Server
	base  string
	token atomic.Value // string
	err   atomic.Value // error
}

func newFixture(t *testing.T, tweak func(*fixture, *Config)) *fixture {
	t.Helper()
	f := &fixture{t: t, brain: afferenttest.NewBrain(t, member)}
	f.token.Store(secret)
	f.err.Store(errors.New(""))
	cfg := Config{
		BrainsrvURL: f.brain.URL(),
		Context:     "afferent-poc",
		Token: func(context.Context) (string, error) {
			if e := f.err.Load().(error); e.Error() != "" {
				return "", e
			}
			return f.token.Load().(string), nil
		},
		Scope:  func(context.Context) (string, error) { return member, nil },
		Status: func(context.Context) any { return map[string]any{"signed_in": true, "scope": member} },
	}
	if tweak != nil {
		tweak(f, &cfg)
	}
	s, err := New(cfg, "127.0.0.1", 0)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = s.Serve(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	f.srv = s
	f.base = "http://" + s.Addr()
	return f
}

type resp struct {
	status int
	header http.Header
	body   string
}

func (f *fixture) do(method, path string, body []byte, hdr map[string]string) resp {
	f.t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, f.base+path, rd)
	if err != nil {
		f.t.Fatal(err)
	}
	req.Header.Set(KeyHeader, f.srv.Key())
	for k, v := range hdr {
		if k == "Host" {
			req.Host = v
			continue
		}
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return resp{res.StatusCode, res.Header, string(b)}
}

func (f *fixture) get(path string) resp { return f.do("GET", path, nil, nil) }

func noToken(t *testing.T, r resp) {
	t.Helper()
	if strings.Contains(r.body, secret) {
		t.Fatalf("token leaked in body: %s", r.body)
	}
	for k, vs := range r.header {
		for _, v := range vs {
			if strings.Contains(v, secret) {
				t.Fatalf("token leaked in header %s", k)
			}
		}
	}
}

func TestLoopbackOnly(t *testing.T) {
	for _, a := range []string{"0.0.0.0", "192.168.1.10", "::", "example.com", "10.0.0.1"} {
		if _, err := LoopbackHost(a); !errors.Is(err, ErrNotLoopback) {
			t.Errorf("%q: want ErrNotLoopback, got %v", a, err)
		}
		if _, err := New(Config{}, a, 0); err == nil {
			t.Errorf("New(%q) listened", a)
		}
	}
	for a, want := range map[string]string{"127.0.0.1": "127.0.0.1", "localhost": "127.0.0.1", "::1": "::1", "[::1]": "::1", "127.0.0.2": "127.0.0.2"} {
		got, err := LoopbackHost(a)
		if err != nil || got != want {
			t.Errorf("%q: got %q, %v", a, got, err)
		}
	}
}

func TestURLCarriesKeyInFragment(t *testing.T) {
	f := newFixture(t, nil)
	u := f.srv.URL()
	if !strings.HasPrefix(u, "http://127.0.0.1:") || !strings.Contains(u, "/#k="+f.srv.Key()) {
		t.Fatalf("URL %q", u)
	}
	if len(f.srv.Key()) != 43 { // 32 bytes, base64url without padding
		t.Fatalf("key length %d", len(f.srv.Key()))
	}
	g := newFixture(t, nil)
	if g.srv.Key() == f.srv.Key() {
		t.Fatal("key is not per launch")
	}
}

func TestSecurityHeadersAndStatic(t *testing.T) {
	f := newFixture(t, nil)
	for _, p := range []string{"/", "/app.js", "/app.css", "/d3.v7.min.js", "/api/status"} {
		r := f.get(p)
		if r.status != 200 {
			t.Fatalf("%s: %d %s", p, r.status, r.body)
		}
		h := r.header
		if h.Get("Content-Security-Policy") != CSP || !strings.Contains(CSP, "frame-ancestors 'none'") || !strings.Contains(CSP, "default-src 'self'") {
			t.Errorf("%s: CSP %q", p, h.Get("Content-Security-Policy"))
		}
		if strings.Contains(CSP, "unsafe-inline") || strings.Contains(CSP, "unsafe-eval") {
			t.Error("CSP allows inline or eval")
		}
		if h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Referrer-Policy") != "no-referrer" || h.Get("X-Frame-Options") != "DENY" {
			t.Errorf("%s: headers %v", p, h)
		}
		for k := range h {
			if strings.HasPrefix(strings.ToLower(k), "access-control-") {
				t.Errorf("%s: CORS header %s", p, k)
			}
		}
	}
	index := f.get("/").body
	if strings.Contains(index, "<script>") || strings.Contains(index, "onclick=") || strings.Contains(index, "style=") {
		t.Error("index.html has inline script or style")
	}
	if strings.Contains(index, "http://") || strings.Contains(index, "https://") {
		t.Error("index.html loads something from the network")
	}
	if ct := f.get("/app.js").header.Get("Content-Type"); !strings.HasPrefix(ct, "text/javascript") {
		t.Errorf("app.js content type %q", ct)
	}
}

func TestKeyRequired(t *testing.T) {
	f := newFixture(t, nil)
	for _, key := range []string{"", "wrong", f.srv.Key() + "x", strings.ToUpper(f.srv.Key())} {
		r := f.do("GET", "/api/status", nil, map[string]string{KeyHeader: key})
		if key == strings.ToUpper(f.srv.Key()) && key == f.srv.Key() {
			continue
		}
		if r.status != 401 || !strings.Contains(r.body, "bad_key") {
			t.Errorf("key %q: %d %s", key, r.status, r.body)
		}
	}
	// A key in the query string or a cookie is not accepted.
	r := f.do("GET", "/api/status?k="+f.srv.Key(), nil, map[string]string{KeyHeader: ""})
	if r.status != 401 {
		t.Errorf("query key accepted: %d", r.status)
	}
	if len(f.brain.Recorded()) != 0 {
		t.Error("brainsrv was called without a key")
	}
}

func TestHostHeaderChecked(t *testing.T) {
	f := newFixture(t, nil)
	port := f.srv.Addr()[strings.LastIndex(f.srv.Addr(), ":")+1:]
	for _, host := range []string{"evil.example:" + port, "127.0.0.1", "localhost", "127.0.0.1:1", "attacker.localhost:" + port, "127.0.0.1.nip.io:" + port} {
		for _, p := range []string{"/api/status", "/", "/app.js"} {
			r := f.do("GET", p, nil, map[string]string{"Host": host})
			if r.status != 403 {
				t.Errorf("Host %q %s: %d", host, p, r.status)
			}
		}
	}
	for _, host := range []string{"127.0.0.1:" + port, "localhost:" + port} {
		if r := f.do("GET", "/api/status", nil, map[string]string{"Host": host}); r.status != 200 {
			t.Errorf("Host %q: %d", host, r.status)
		}
	}
	// Cross-site requests are refused even with the key.
	if r := f.do("GET", "/api/status", nil, map[string]string{"Origin": "https://evil.example"}); r.status != 403 {
		t.Errorf("foreign Origin: %d", r.status)
	}
	if r := f.do("GET", "/api/status", nil, map[string]string{"Sec-Fetch-Site": "cross-site"}); r.status != 403 {
		t.Errorf("cross-site fetch: %d", r.status)
	}
	if r := f.do("GET", "/api/status", nil, map[string]string{"Origin": "http://127.0.0.1:" + port}); r.status != 200 {
		t.Errorf("own Origin: %d", r.status)
	}
}

func TestUnknownPathsAndMethods(t *testing.T) {
	f := newFixture(t, nil)
	for _, p := range []string{"/v1/overview", "/api/", "/api/v1/whoami", "/api/whoami", "/api/../v1/overview", "/api/entities", "/api/sessions/x/y/z", "/static/app.js", "/.env", "/api/proxy?u=http://x"} {
		r := f.get(p)
		if r.status != 404 {
			t.Errorf("%s: %d", p, r.status)
		}
	}
	if r := f.do("POST", "/api/status", []byte("{}"), nil); r.status != 405 {
		t.Errorf("POST status: %d", r.status)
	}
	if r := f.get("/api/recall"); r.status != 405 {
		t.Errorf("GET recall: %d", r.status)
	}
	if r := f.do("POST", "/", nil, nil); r.status != 405 {
		t.Errorf("POST /: %d", r.status)
	}
	for _, r := range f.brain.Recorded() {
		if r.Path != "/v1/whoami" {
			t.Errorf("unexpected brainsrv call %s", r.Path)
		}
	}
}

func TestBodyLimit(t *testing.T) {
	f := newFixture(t, nil)
	big := []byte(`{"query":"` + strings.Repeat("a", MaxBody) + `","k":5}`)
	r := f.do("POST", "/api/recall", big, map[string]string{"Content-Type": "application/json"})
	if r.status != 413 {
		t.Fatalf("big body: %d %s", r.status, r.body)
	}
	// Chunked (no Content-Length) is capped too.
	req, _ := http.NewRequest("POST", f.base+"/api/recall", io.MultiReader(bytes.NewReader(big)))
	req.ContentLength = -1
	req.Header.Set(KeyHeader, f.srv.Key())
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 413 {
		t.Fatalf("chunked big body: %d", res.StatusCode)
	}
	if f.brain.Last("/v1/recall") != nil {
		t.Fatal("oversized recall reached brainsrv")
	}
}

func TestProxyingAgainstFakeBrainsrv(t *testing.T) {
	f := newFixture(t, nil)

	r := f.get("/api/overview?depth=2")
	if r.status != 200 {
		t.Fatalf("overview: %d %s", r.status, r.body)
	}
	var ov struct {
		Scope  string         `json:"scope"`
		Totals map[string]int `json:"totals"`
		Kids   []struct {
			Scope, Label string
			Turns        int
			Children     []json.RawMessage
		} `json:"children"`
	}
	if err := json.Unmarshal([]byte(r.body), &ov); err != nil {
		t.Fatal(err)
	}
	if ov.Scope != member || len(ov.Kids) < 3 || ov.Totals["turns"] == 0 || len(ov.Kids[0].Children) == 0 {
		t.Fatalf("overview %+v", ov)
	}
	last := f.brain.Last("/v1/overview")
	if last.Scope != member || last.Context != "afferent-poc" || last.Auth != "Bearer "+secret || last.Query != "depth=2" {
		t.Fatalf("overview request %+v", last)
	}
	noToken(t, r)
	if r := f.get("/api/overview"); r.status != 200 || f.brain.Last("/v1/overview").Query != "depth=3" {
		t.Fatalf("default depth: %d %+v", r.status, f.brain.Last("/v1/overview"))
	}
	for _, q := range []string{"depth=0", "depth=5", "depth=x", "valid_at=yesterday"} {
		if r := f.get("/api/overview?" + q); r.status != 400 {
			t.Errorf("overview %s: %d", q, r.status)
		}
	}

	r = f.get("/api/graph?limit=10")
	var gr struct {
		Nodes []struct{ ID string }
		Edges []struct{ Src, Dst string }
	}
	if r.status != 200 || json.Unmarshal([]byte(r.body), &gr) != nil || len(gr.Nodes) != 10 {
		t.Fatalf("graph: %d %s", r.status, r.body)
	}
	if r := f.get("/api/graph"); r.status != 200 || f.brain.Last("/v1/graph").Query != "limit=150" {
		t.Fatalf("graph default: %+v", f.brain.Last("/v1/graph"))
	}
	sub := member + ".afferent"
	if r := f.get("/api/graph?scope=" + sub); r.status != 200 || f.brain.Last("/v1/graph").Query != "limit=150&scope="+sub || f.brain.Last("/v1/graph").Scope != member {
		t.Fatalf("graph scope: %d %+v", r.status, f.brain.Last("/v1/graph"))
	}
	for _, q := range []string{"limit=0", "limit=501"} {
		if r := f.get("/api/graph?" + q); r.status != 400 {
			t.Errorf("graph %s: %d", q, r.status)
		}
	}

	r = f.do("POST", "/api/recall", []byte(`{"query":"Go","k":5}`), nil)
	if r.status != 200 || !strings.Contains(r.body, `"results"`) {
		t.Fatalf("recall: %d %s", r.status, r.body)
	}
	var rb map[string]any
	_ = json.Unmarshal([]byte(f.brain.Last("/v1/recall").Body), &rb)
	if rb["query"] != "Go" || rb["k"] != float64(5) || rb["mode"] != "memories" {
		t.Fatalf("recall body %v", rb)
	}
	for _, b := range []string{`{}`, `{"query":""}`, `{"query":"x","k":1000}`, `not json`, `{"query":"x","scope":"ws"}`} {
		if r := f.do("POST", "/api/recall", []byte(b), nil); r.status != 400 {
			t.Errorf("recall %s: %d", b, r.status)
		}
	}

	eid := afferenttest.FakeID('e', 1)
	r = f.get("/api/entities/" + eid)
	if r.status != 200 || !strings.Contains(r.body, `"attributes"`) {
		t.Fatalf("entity: %d %s", r.status, r.body)
	}
	if r := f.get("/api/entities/" + afferenttest.FakeID('e', 999)); r.status != 404 {
		t.Fatalf("missing entity: %d", r.status)
	}

	r = f.get("/api/sessions?scope=" + sub + ".claude&limit=3")
	var ss []map[string]any
	if r.status != 200 || json.Unmarshal([]byte(r.body), &ss) != nil || len(ss) == 0 || len(ss) > 3 {
		t.Fatalf("sessions: %d %s", r.status, r.body)
	}
	if l := f.brain.Last("/v1/sessions"); l.Scope != sub+".claude" || l.Query != "limit=3" {
		t.Fatalf("sessions request %+v", l)
	}
	if r := f.get("/api/sessions"); r.status != 200 || f.brain.Last("/v1/sessions").Scope != member {
		t.Fatalf("sessions default scope: %+v", f.brain.Last("/v1/sessions"))
	}
	r = f.get("/api/sessions/" + ss[0]["id"].(string) + "/turns")
	if r.status != 200 || !strings.Contains(r.body, `"turns"`) {
		t.Fatalf("turns: %d %s", r.status, r.body)
	}
	noToken(t, r)
}

func TestScopeEscape(t *testing.T) {
	f := newFixture(t, nil)
	for _, sc := range []string{
		"ws", "ws.dev", "ws.dev.people.other.harness", member + "x", member + "_evil.a",
		member + "..a", member + ".a b", member + ".a;drop", "*", member + ".*", "." + member, member + ".",
	} {
		for _, p := range []string{"/api/sessions?scope=", "/api/graph?scope="} {
			r := f.get(p + urlEscape(sc))
			if r.status != 400 || !strings.Contains(r.body, "bad_scope") {
				t.Errorf("%s%s: %d %s", p, sc, r.status, r.body)
			}
		}
	}
	for _, r := range f.brain.Recorded() {
		if r.Path != "/v1/whoami" {
			t.Errorf("brainsrv called for an escaped scope: %+v", r)
		}
	}
}

func urlEscape(s string) string {
	return strings.NewReplacer(" ", "%20", ";", "%3B", "*", "%2A").Replace(s)
}

func TestUUIDValidation(t *testing.T) {
	f := newFixture(t, nil)
	for _, id := range []string{"x", "123", "../../v1/whoami", "00000000-0000-0000-0000-00000000000g", "%2e%2e", "00000000000000000000000000000000"} {
		for _, p := range []string{"/api/entities/" + id, "/api/sessions/" + id + "/turns"} {
			r := f.get(p)
			if r.status != 400 && r.status != 404 {
				t.Errorf("%s: %d", p, r.status)
			}
		}
	}
	if r := f.get("/api/entities/not-a-uuid"); r.status != 400 || !strings.Contains(r.body, "bad_id") {
		t.Errorf("entity id: %d %s", r.status, r.body)
	}
	if r := f.get("/api/sessions/not-a-uuid/turns"); r.status != 400 {
		t.Errorf("session id: %d", r.status)
	}
	for _, r := range f.brain.Recorded() {
		if r.Path != "/v1/whoami" {
			t.Errorf("brainsrv called for a bad id: %+v", r)
		}
	}
}

func TestTokenNeverReachesTheBrowser(t *testing.T) {
	f := newFixture(t, func(f *fixture, cfg *Config) {
		f.brain.EchoAuth = true
		cfg.Status = func(context.Context) any {
			return map[string]any{"note": "leak " + secret, "err": "Bearer " + secret}
		}
	})
	paths := []string{"/api/status", "/api/overview", "/api/graph", "/api/sessions", "/api/entities/" + afferenttest.FakeID('e', 1),
		"/api/sessions/" + afferenttest.FakeID('s', 1) + "/turns", "/", "/app.js"}
	for _, p := range paths {
		r := f.get(p)
		if r.status != 200 {
			t.Errorf("%s: %d %s", p, r.status, r.body)
		}
		noToken(t, r)
	}
	r := f.do("POST", "/api/recall", []byte(`{"query":"Go"}`), nil)
	noToken(t, r)
	if !strings.Contains(f.get("/api/overview").body, "[redacted]") {
		t.Error("echoed token was not redacted")
	}
	// Errors do not carry it either.
	f.brain.Reject(secret)
	r = f.get("/api/overview")
	if r.status != 401 {
		t.Errorf("rejected token: %d", r.status)
	}
	noToken(t, r)
	f.err.Store(errors.New("refresh failed for Bearer " + secret))
	r = f.get("/api/graph")
	noToken(t, r)
}

func TestRefreshOn401(t *testing.T) {
	var refreshed atomic.Int32
	f := newFixture(t, func(f *fixture, cfg *Config) {
		f.brain.Reject(secret)
		cfg.ForceRefresh = func(_ context.Context, rejected string) (string, error) {
			if rejected != secret {
				t.Errorf("rejected %q", rejected)
			}
			refreshed.Add(1)
			f.token.Store("tok-new")
			return "tok-new", nil
		}
	})
	if r := f.get("/api/overview"); r.status != 200 {
		t.Fatalf("after refresh: %d %s", r.status, r.body)
	}
	if refreshed.Load() != 1 || f.brain.Last("/v1/overview").Auth != "Bearer tok-new" {
		t.Fatalf("refresh %d, %+v", refreshed.Load(), f.brain.Last("/v1/overview"))
	}
}

func TestRedirectRefused(t *testing.T) {
	target := afferenttest.NewBrain(t, member)
	f := newFixture(t, func(f *fixture, _ *Config) { f.brain.RedirectTo = target.URL() })
	r := f.get("/api/overview")
	if r.status != 502 || !strings.Contains(r.body, "brainsrv_redirect") {
		t.Fatalf("redirect: %d %s", r.status, r.body)
	}
	r = f.do("POST", "/api/recall", []byte(`{"query":"Go"}`), nil)
	if r.status != 502 {
		t.Fatalf("recall redirect: %d", r.status)
	}
	if len(target.Recorded()) != 0 {
		t.Fatalf("redirect was followed: %+v", target.Recorded())
	}
}

func TestSignedOutAndBrainsrvDown(t *testing.T) {
	f := newFixture(t, nil)
	f.err.Store(auth.ErrLoginRequired)
	r := f.get("/api/overview")
	if r.status != 401 || !strings.Contains(r.body, "signed_out") {
		t.Fatalf("signed out: %d %s", r.status, r.body)
	}
	f.err.Store(errors.New(""))

	g := newFixture(t, func(_ *fixture, cfg *Config) {
		cfg.Scope = func(context.Context) (string, error) { return "", auth.ErrLoginRequired }
	})
	if r := g.get("/api/graph"); r.status != 401 || !strings.Contains(r.body, "signed_out") {
		t.Fatalf("signed out scope: %d %s", r.status, r.body)
	}

	d := newFixture(t, func(_ *fixture, cfg *Config) {
		cfg.Scope = func(context.Context) (string, error) {
			return "", fmt.Errorf("learn the member scope: %w", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")})
		}
	})
	if r := d.get("/api/overview"); r.status != 502 || !strings.Contains(r.body, "brainsrv_down") || strings.Contains(r.body, "dial") {
		t.Fatalf("scope unreachable: %d %s", r.status, r.body)
	}

	h := newFixture(t, nil)
	h.brain.Server.Close()
	r = h.get("/api/overview")
	if r.status != 502 || !strings.Contains(r.body, "brainsrv_down") {
		t.Fatalf("down: %d %s", r.status, r.body)
	}
}

func TestOlderBrainsrvWithoutOverview(t *testing.T) {
	f := newFixture(t, func(f *fixture, cfg *Config) { cfg.BrainsrvURL = f.brain.URL() + "/old" }) // every path 404s
	r := f.get("/api/overview")
	if r.status != 404 || !strings.Contains(r.body, "unsupported") {
		t.Fatalf("older brainsrv: %d %s", r.status, r.body)
	}
}

func TestEmptyBrain(t *testing.T) {
	f := newFixture(t, func(f *fixture, _ *Config) { f.brain.Clear() })
	r := f.get("/api/overview")
	if r.status != 200 || !strings.Contains(r.body, `"children":[]`) || !strings.Contains(r.body, `"turns":0`) {
		t.Fatalf("empty overview: %d %s", r.status, r.body)
	}
	r = f.get("/api/graph")
	if r.status != 200 || !strings.Contains(r.body, `"nodes":[]`) {
		t.Fatalf("empty graph: %d %s", r.status, r.body)
	}
}

func TestServeStopsOnCancel(t *testing.T) {
	s, err := New(Config{}, "localhost", 0)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not stop")
	}
}
