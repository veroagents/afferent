package brain

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWhoami(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/whoami" || r.Header.Get("Authorization") != "Bearer tok" || r.Header.Get("X-Context") != "afferent-poc" {
			http.Error(w, "bad request", 400)
			return
		}
		w.Write([]byte(`{"principal_id":"p1","kind":"user","context":"afferent-poc","subject":"user-1",
			"grants":[{"scope":"ws.dev.people.drew.harness","verbs":["read","write"],"template_id":"t1"}]}`))
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL + "/", Context: "afferent-poc", Token: func(context.Context) (string, error) { return "tok", nil }}
	w, err := c.Whoami(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if w.PrincipalID != "p1" || len(w.Grants) != 1 || w.Grants[0].Scope != "ws.dev.people.drew.harness" || w.Grants[0].TemplateID != "t1" {
		t.Fatalf("%+v", w)
	}
}

func TestWhoamiNotFoundIsUnsupported(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Token: func(context.Context) (string, error) { return "tok", nil }}
	if _, err := c.Whoami(context.Background()); !errors.Is(err, ErrWhoamiUnsupported) {
		t.Fatalf("got %v", err)
	}
}

func TestWhoamiForbidden(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"no grant"}`, 403)
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Token: func(context.Context) (string, error) { return "tok", nil }}
	_, err := c.Whoami(context.Background())
	var se *StatusError
	if !errors.As(err, &se) || se.Status != 403 {
		t.Fatalf("got %v", err)
	}
}
