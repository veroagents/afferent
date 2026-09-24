package auth

import (
	"context"
	"testing"
	"time"

	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/afferent/afferenttest"
)

// A server rejected a token that still looks valid: ForceRefresh refreshes
// even though the expiry is far away, and a second ForceRefresh with the same
// (now superseded) token reuses the new one instead of refreshing again.
func TestForceRefreshReplacesRejectedToken(t *testing.T) {
	a := afferenttest.NewAuthsrv(t)
	dir := t.TempDir()
	a.IssueRefresh("rt-seed")
	fs := &FileStore{Path: CredentialsFile(dir)}
	old := afferenttest.MakeJWT(map[string]any{"iss": a.URL(), "n": 0})
	if err := fs.Save(&Credentials{AccessToken: old, RefreshToken: "rt-seed", Expiry: time.Now().Add(time.Hour), Issuer: a.URL(), ClientID: "afferent-cli"}); err != nil {
		t.Fatal(err)
	}
	ts := &TokenSource{Store: fs, LockPath: LockFile(dir), Client: newClient(a, nil)}
	ctx := context.Background()

	if tok, err := ts.Token(ctx); err != nil || tok != old {
		t.Fatalf("Token = %v, %v; want the stored token", tok, err)
	}
	if n := a.RefreshCalls.Load(); n != 0 {
		t.Fatalf("refresh calls %d before force", n)
	}
	fresh, err := ts.ForceRefresh(ctx, old)
	if err != nil {
		t.Fatal(err)
	}
	if fresh == old || a.RefreshCalls.Load() != 1 {
		t.Fatalf("force refresh did not refresh: same=%v calls=%d", fresh == old, a.RefreshCalls.Load())
	}
	// A second caller that also saw the old token rejected gets the new one.
	again, err := (&TokenSource{Store: fs, LockPath: LockFile(dir), Client: newClient(a, nil)}).ForceRefresh(ctx, old)
	if err != nil || again != fresh || a.RefreshCalls.Load() != 1 {
		t.Fatalf("second force: %v same=%v calls=%d", err, again == fresh, a.RefreshCalls.Load())
	}
}
