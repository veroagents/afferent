package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/afferent/afferenttest"
	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/afferent/config"
	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/afferent/forward"
	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/afferent/service"
	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/afferent/ui"
)

const uiMember = "ws.dev.people.drew.harness"

// startUI signs in against the fake authsrv, points brainsrv at a fake that
// implements the ui contract, and runs `afferent ui` until the test ends. It
// returns the printed URL and a stop function (also run at cleanup).
func startUI(t *testing.T, h *harness, args ...string) (string, *afferenttest.Brain, func()) {
	t.Helper()
	brain := afferenttest.NewBrain(t, uiMember)
	h.envs[config.EnvBrainsrvURL] = brain.URL()
	if out, _, err := h.run("login", "--no-browser"); err != nil {
		t.Fatalf("login: %v\n%s", err, out)
	}
	pr, pw := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	h.ctx, h.out = ctx, pw
	done := make(chan error, 1)
	go func() {
		_, _, err := h.run(append([]string{"ui"}, args...)...)
		pw.Close()
		done <- err
	}()
	urls := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(pr)
		for sc.Scan() {
			if m := regexp.MustCompile(`http://\S+`).FindString(sc.Text()); m != "" {
				select {
				case urls <- m:
				default:
				}
			}
		}
	}()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("ui: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Error("ui did not stop")
			}
			h.ctx, h.out = nil, nil
		})
	}
	t.Cleanup(stop)
	select {
	case u := <-urls:
		return u, brain, stop
	case <-time.After(10 * time.Second):
		t.Fatal("ui printed no URL")
	}
	return "", nil, stop
}

func uiGet(t *testing.T, base, key, path string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", base+path, nil)
	req.Header.Set(ui.KeyHeader, key)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

func splitURL(t *testing.T, u string) (base, key string) {
	t.Helper()
	i := strings.Index(u, "/#k=")
	if i < 0 {
		t.Fatalf("no key in %q", u)
	}
	return u[:i], u[i+4:]
}

func writeForwarderStatus(t *testing.T, h *harness) {
	t.Helper()
	dir := filepath.Join(h.dir, "state")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	s := forward.Status{
		PID: 4242, StartedAt: now.Add(-3 * time.Hour), UpdatedAt: now.Add(-4 * time.Second), State: forward.StateRunning,
		LogPath: h.logPath, BrainsrvURL: h.envs[config.EnvBrainsrvURL], Context: "afferent-poc", Scope: uiMember,
		LastSuccessAt: now.Add(-38 * time.Second), Batches: 812, LinesSent: 20431, BytesSent: 48 << 20,
		Accepted: 18302, Duplicate: 2011, Rejected: 3, LinesSkipped: 115, LagBytes: 12 << 10, LogFound: true,
	}
	b, _ := json.Marshal(s)
	if err := os.WriteFile(filepath.Join(dir, forward.StatusFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestUICommand(t *testing.T) {
	h := newHarness(t)
	h.svc.state = service.State{Loaded: true, Running: true, PID: 4242}
	writeForwarderStatus(t, h)
	u, brain, stop := startUI(t, h)
	base, key := splitURL(t, u)
	if !strings.HasPrefix(base, "http://127.0.0.1:") {
		t.Fatalf("not loopback: %s", base)
	}

	code, body := uiGet(t, base, key, "/api/status")
	if code != 200 {
		t.Fatalf("status: %d %s", code, body)
	}
	var st map[string]any
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		t.Fatal(err)
	}
	if st["signed_in"] != true || st["identity"] != "drew@vero.localhost" || st["scope"] != uiMember || st["scope_source"] != "brainsrv" {
		t.Fatalf("status %s", body)
	}
	if svc := st["service"].(map[string]any); svc["running"] != true {
		t.Fatalf("service %v", svc)
	}
	if fw := st["forwarder"].(map[string]any); fw["state"] != "running" || fw["lines_sent"] != float64(20431) {
		t.Fatalf("forwarder %v", fw)
	}
	if st["token_valid_seconds"].(float64) <= 0 {
		t.Fatalf("token validity %v", st["token_valid_seconds"])
	}

	// The token is added server-side and never returned.
	creds, err := h.store().Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/api/status", "/api/overview", "/api/graph", "/api/sessions"} {
		code, body := uiGet(t, base, key, p)
		if code != 200 {
			t.Fatalf("%s: %d %s", p, code, body)
		}
		if strings.Contains(body, creds.AccessToken) || strings.Contains(body, creds.RefreshToken) {
			t.Fatalf("%s leaks a token", p)
		}
	}
	last := brain.Last("/v1/overview")
	if last.Auth != "Bearer "+creds.AccessToken || last.Scope != uiMember || last.Context != "afferent-poc" {
		t.Fatalf("overview request %+v", last)
	}
	if code, _ := uiGet(t, base, "", "/api/status"); code != 401 {
		t.Fatalf("no key: %d", code)
	}
	if code, _ := uiGet(t, base, key, "/api/sessions?scope=ws.dev.people.someone.harness"); code != 400 {
		t.Fatalf("scope escape: %d", code)
	}
	stop()
	if len(h.opened) != 1 || h.opened[0] != u {
		t.Fatalf("browser opened %v, printed %s", h.opened, u)
	}
}

func TestUICommandFlags(t *testing.T) {
	h := newHarness(t)
	for _, a := range []string{"0.0.0.0", "192.168.0.2", "example.com"} {
		if _, _, err := h.run("ui", "--no-browser", "--addr", a); err == nil || !strings.Contains(err.Error(), "loopback") {
			t.Errorf("--addr %s: %v", a, err)
		}
	}
	u, _, stop := startUI(t, h, "--no-browser")
	base, key := splitURL(t, u)
	if code, _ := uiGet(t, base, key, "/api/status"); code != 200 {
		t.Fatalf("status %d", code)
	}
	stop()
	if len(h.opened) != 0 {
		t.Fatalf("--no-browser opened %v", h.opened)
	}
}

func TestUICommandSignedOut(t *testing.T) {
	h := newHarness(t)
	brain := afferenttest.NewBrain(t, uiMember)
	h.envs[config.EnvBrainsrvURL] = brain.URL()
	pr, pw := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	h.ctx, h.out = ctx, pw
	done := make(chan error, 1)
	go func() { _, _, err := h.run("ui", "--no-browser"); pw.Close(); done <- err }()
	line, _ := bufio.NewReader(pr).ReadString('\n')
	go io.Copy(io.Discard, pr)
	base, key := splitURL(t, strings.TrimSpace(strings.TrimPrefix(line, "afferent ui: ")))
	code, body := uiGet(t, base, key, "/api/status")
	if code != 200 || !strings.Contains(body, `"signed_in":false`) {
		t.Fatalf("status: %d %s", code, body)
	}
	code, body = uiGet(t, base, key, "/api/overview")
	if code != 401 || !strings.Contains(body, "signed_out") {
		t.Fatalf("overview: %d %s", code, body)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// TestUIDemo serves the page with fake data for a manual look or a headless
// screenshot. It runs only with AFFERENT_UI_DEMO=<duration>, and writes the
// URL to AFFERENT_UI_DEMO_URLFILE when set. AFFERENT_UI_DEMO_MODE=empty, truncated,
// signedout or down shows those states. It uses temp dirs and fakes only.
func TestUIDemo(t *testing.T) {
	d := os.Getenv("AFFERENT_UI_DEMO")
	if d == "" {
		t.Skip("set AFFERENT_UI_DEMO=2m to serve the demo page")
	}
	wait, err := time.ParseDuration(d)
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t)
	h.svc.state = service.State{Loaded: true, Running: true, PID: 4242}
	writeForwarderStatus(t, h)
	u, brain, _ := startUI(t, h, "--no-browser")
	switch os.Getenv("AFFERENT_UI_DEMO_MODE") {
	case "empty":
		brain.Clear()
	case "signedout":
		if err := h.store().Delete(); err != nil {
			t.Fatal(err)
		}
	case "down":
		brain.Server.Close()
	case "truncated":
		brain.MaxChildren = 2
	}
	fmt.Println("DEMO URL:", u)
	if f := os.Getenv("AFFERENT_UI_DEMO_URLFILE"); f != "" {
		if err := os.WriteFile(f, []byte(u+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(wait)
}
