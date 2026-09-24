package forward

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/afferent/auth"
	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/afferent/brain"
)

// A cached scope is only used for the account it was learned for.
func TestScopeCacheIsPerAccount(t *testing.T) {
	dir := t.TempDir()
	account := "iss#alice"
	var answer func() (*brain.Whoami, error)
	r := &ScopeResolver{StateDir: dir, URL: "http://b", Context: "c",
		Whoami:  func(context.Context) (*brain.Whoami, error) { return answer() },
		Account: func() string { return account }}
	ctx := context.Background()
	answer = func() (*brain.Whoami, error) {
		return &brain.Whoami{PrincipalID: "pa", Grants: []brain.Grant{{Scope: "ws.alice.harness", Verbs: []string{"write"}}}}, nil
	}
	if s, _, err := r.Resolve(ctx); err != nil || s != "ws.alice.harness" {
		t.Fatalf("%q %v", s, err)
	}
	if r.Cached() != "ws.alice.harness" {
		t.Fatal("not cached")
	}

	// Signed in as bob: alice's scope is neither shown nor used when
	// whoami fails.
	account = "iss#bob"
	if c := r.Cached(); c != "" {
		t.Fatalf("Cached() = %q for another account", c)
	}
	for _, e := range []error{errors.New("connection refused"), &brain.StatusError{Status: 503}, auth.ErrLoginRequired} {
		answer = func() (*brain.Whoami, error) { return nil, e }
		if s, src, err := r.Resolve(ctx); err == nil || s != "" {
			t.Fatalf("%v: got %q (%s) for another account", e, s, src)
		}
	}
	// bob's own answer replaces it.
	answer = func() (*brain.Whoami, error) {
		return &brain.Whoami{PrincipalID: "pb", Grants: []brain.Grant{{Scope: "ws.bob.harness", Verbs: []string{"write"}}}}, nil
	}
	if s, _, _ := r.Resolve(ctx); s != "ws.bob.harness" || r.Cached() != "ws.bob.harness" {
		t.Fatalf("bob %q %q", s, r.Cached())
	}

	if err := ClearScopeCache(dir); err != nil || r.Cached() != "" {
		t.Fatalf("clear: %v %q", err, r.Cached())
	}
	if _, err := os.Stat(filepath.Join(dir, ScopeCacheFile)); !os.IsNotExist(err) {
		t.Fatal("scope.json remains")
	}
	if err := ClearScopeCache(dir); err != nil {
		t.Fatalf("clear twice: %v", err)
	}
}

func TestCredentialsAccount(t *testing.T) {
	jwt := func(payload string) string {
		return "x." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".y"
	}
	c := &auth.Credentials{Issuer: "https://i", AccessToken: jwt(`{"sub":"u1"}`)}
	if c.Account() != "https://i#u1" {
		t.Fatalf("%q", c.Account())
	}
	for _, c := range []*auth.Credentials{nil, {}, {Issuer: "i", AccessToken: "opaque"}, {Issuer: "i", AccessToken: jwt(`{}`)}} {
		if c.Account() != "" {
			t.Fatalf("%+v: %q", c, c.Account())
		}
	}
}
