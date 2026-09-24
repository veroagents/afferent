package forward

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// The forwarder must not follow a redirect with the bearer token and the
// batch (a same-host downgrade to http keeps Authorization in Go).
func TestForwarderRefusesRedirects(t *testing.T) {
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write([]byte(`{"accepted":1}`))
	}))
	defer target.Close()
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusPermanentRedirect)
	}))
	defer redir.Close()

	e := newEnv(t)
	e.primeEmpty()
	e.write(e.log, 2)
	for _, hc := range []*http.Client{nil, {}} {
		o := e.opts()
		o.URL = redir.URL
		o.HTTP = hc
		err := e.run(o)
		if err == nil || !strings.Contains(err.Error(), "redirect") || !strings.Contains(err.Error(), target.URL) {
			t.Fatalf("run: %v", err)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("the redirect target got %d requests", hits.Load())
	}
	st, _ := ReadCheckpoints(e.state)
	for _, cp := range st.Files {
		if cp.Offset != 0 {
			t.Fatal("checkpoint advanced past a redirect")
		}
	}
}
