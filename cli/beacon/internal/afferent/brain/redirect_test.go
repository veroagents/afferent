package brain

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestWhoamiDoesNotFollowRedirects(t *testing.T) {
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write([]byte(`{"principal_id":"p"}`))
	}))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	for _, hc := range []*http.Client{nil, {}} {
		c := &Client{BaseURL: srv.URL, Context: "c", HTTP: hc, Token: func(context.Context) (string, error) { return "tok", nil }}
		if _, err := c.Whoami(context.Background()); err == nil {
			t.Fatal("a redirect was followed to a success")
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("the redirect target got %d requests", hits.Load())
	}
}

func TestNoRedirectsKeepsCallerPolicy(t *testing.T) {
	own := func(*http.Request, []*http.Request) error { return nil }
	hc := &http.Client{CheckRedirect: own}
	if NoRedirects(hc) != hc {
		t.Fatal("a client with its own policy was replaced")
	}
	plain := &http.Client{}
	if got := NoRedirects(plain); got == plain || got.CheckRedirect == nil || plain.CheckRedirect != nil {
		t.Fatal("NoRedirects must return a copy with the policy set")
	}
}
