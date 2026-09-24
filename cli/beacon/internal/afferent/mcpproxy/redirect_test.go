package mcpproxy

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// The proxy must not re-POST a JSON-RPC body (and, same host, the bearer
// token) to wherever a 307/308 points.
func TestProxyRefusesRedirects(t *testing.T) {
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	defer target.Close()
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer redir.Close()

	s := start(t, Options{URL: redir.URL, Context: "afferent-poc", Scope: "s", Tokens: &fakeTokens{cur: "t1"}})
	s.send(initMsg)
	r := parse(t, s.wait(1)[0])
	if r.Error == nil {
		t.Fatalf("initialize through a redirect succeeded: %+v", r)
	}
	s.close()
	if hits.Load() != 0 {
		t.Fatalf("the redirect target got %d requests", hits.Load())
	}
}
