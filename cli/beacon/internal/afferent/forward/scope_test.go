package forward

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/afferent/auth"
	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/afferent/brain"
)

func TestMemberScope(t *testing.T) {
	g := func(scope string, verbs ...string) brain.Grant { return brain.Grant{Scope: scope, Verbs: verbs} }
	cases := []struct {
		name   string
		grants []brain.Grant
		want   string
		err    error
	}{
		{"one", []brain.Grant{g("ws.t.people.u.harness", "read", "write"), g("ws.t.projects.x", "read", "write")}, "ws.t.people.u.harness", nil},
		{"read only is not enough", []brain.Grant{g("ws.t.people.u.harness", "read")}, "", ErrNoScope},
		{"none", nil, "", ErrNoScope},
		{"suffix must be a label", []brain.Grant{g("ws.t.people.u.notharness", "write")}, "", ErrNoScope},
		{"ambiguous", []brain.Grant{g("ws.t.people.u.harness", "write"), g("ws.t.people.v.harness", "write")}, "", ErrAmbiguousScope},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := MemberScope(&brain.Whoami{Context: "afferent-poc", Subject: "u", Grants: c.grants})
			if !errors.Is(err, c.err) || got != c.want {
				t.Fatalf("got %q, %v; want %q, %v", got, err, c.want, c.err)
			}
			if c.err == ErrAmbiguousScope && !strings.Contains(err.Error(), "ws.t.people.v.harness") {
				t.Fatalf("ambiguous error should list the scopes: %v", err)
			}
		})
	}
}

func TestScopeResolverCachesAndFallsBack(t *testing.T) {
	dir := t.TempDir()
	var answer func() (*brain.Whoami, error)
	r := &ScopeResolver{StateDir: dir, URL: "http://b", Context: "c", Whoami: func(context.Context) (*brain.Whoami, error) { return answer() }}
	ctx := context.Background()

	answer = func() (*brain.Whoami, error) {
		return &brain.Whoami{PrincipalID: "p", Grants: []brain.Grant{{Scope: "ws.a.harness", Verbs: []string{"write"}}}}, nil
	}
	if s, src, err := r.Resolve(ctx); err != nil || s != "ws.a.harness" || src != "brainsrv" {
		t.Fatalf("%q %q %v", s, src, err)
	}
	// brainsrv down, or the user signed out: the cached scope holds.
	for _, e := range []error{errors.New("dial tcp: connection refused"), &brain.StatusError{Status: 503}, auth.ErrLoginRequired} {
		answer = func() (*brain.Whoami, error) { return nil, e }
		if s, src, err := r.Resolve(ctx); err != nil || s != "ws.a.harness" || src != "cached" {
			t.Fatalf("%v: %q %q %v", e, s, src, err)
		}
	}
	// A definite answer is not papered over.
	answer = func() (*brain.Whoami, error) { return nil, &brain.StatusError{Status: 403, Body: "no"} }
	if _, _, err := r.Resolve(ctx); err == nil {
		t.Fatal("403 fell back to the cache")
	}
	answer = func() (*brain.Whoami, error) { return &brain.Whoami{}, nil }
	if _, _, err := r.Resolve(ctx); !errors.Is(err, ErrNoScope) {
		t.Fatalf("no grant: %v", err)
	}
	// The cache belongs to one brainsrv and Context.
	other := *r
	other.Context = "other"
	answer = func() (*brain.Whoami, error) { return nil, errors.New("down") }
	if _, _, err := other.Resolve(ctx); err == nil {
		t.Fatal("used another Context's cached scope")
	}
	// A configured scope wins without asking.
	other.Override = "ws.cfg.harness"
	if s, src, err := other.Resolve(ctx); err != nil || s != "ws.cfg.harness" || src != "configured" {
		t.Fatalf("%q %q %v", s, src, err)
	}
}
