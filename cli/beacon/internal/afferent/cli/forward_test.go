package cli

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/afferent/config"
	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/afferent/forward"
	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/afferent/service"
)

// fakeLoader records service manager calls; nothing reaches launchctl.
type fakeLoader struct {
	mu    sync.Mutex
	calls []string
	state service.State
}

func (f *fakeLoader) Load(p string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "load "+p)
	f.state = service.State{Loaded: true, Running: true, PID: 4242}
	return nil
}

func (f *fakeLoader) Unload(p string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "unload "+p)
	f.state = service.State{}
	return nil
}

func (f *fakeLoader) Status() (service.State, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state, nil
}

// ingestRecorder is a brainsrv ingest that dedupes on event id.
type ingestRecorder struct {
	mu      sync.Mutex
	ids     []string
	seen    map[string]bool
	scopes  []string
	status  int
	message string
}

func (h *harness) recordIngest() *ingestRecorder {
	rec := &ingestRecorder{seen: map[string]bool{}}
	h.ingest = func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Context") != "afferent-poc" || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			http.Error(w, "unauthorized", 401)
			return
		}
		rec.mu.Lock()
		defer rec.mu.Unlock()
		if rec.status != 0 {
			http.Error(w, rec.message, rec.status)
			return
		}
		rec.scopes = append(rec.scopes, r.Header.Get("X-Scope"))
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			http.Error(w, "bad gzip", 400)
			return
		}
		acc, dup := 0, 0
		sc := bufio.NewScanner(zr)
		for sc.Scan() {
			var ev struct{ ID string }
			_ = json.Unmarshal(sc.Bytes(), &ev)
			if rec.seen[ev.ID] {
				dup++
				continue
			}
			rec.seen[ev.ID] = true
			rec.ids = append(rec.ids, ev.ID)
			acc++
		}
		fmt.Fprintf(w, `{"accepted":%d,"duplicate":%d,"rejected":0,"sessions_touched":1}`, acc, dup)
	}
	return rec
}

func (h *harness) writeEvents(ids ...string) {
	h.t.Helper()
	f, err := os.OpenFile(h.logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		h.t.Fatal(err)
	}
	defer f.Close()
	for _, id := range ids {
		fmt.Fprintf(f, `{"id":%q,"kind":"prompt"}`+"\n", id)
	}
}

func TestForwardOnceLearnsScopeAndResumes(t *testing.T) {
	h := newHarness(t)
	rec := h.recordIngest()
	if _, _, err := h.run("login", "--no-browser"); err != nil {
		t.Fatal(err)
	}
	h.writeEvents("a", "b")
	// Backfill the history that predates the first run.
	out, errOut, err := h.run("forward", "--once", "--backfill")
	if err != nil {
		t.Fatalf("forward: %v\n%s", err, errOut)
	}
	if !strings.Contains(out, "2 lines sent") || !strings.Contains(errOut, "scope ws.dev.people.drew.harness") {
		t.Fatalf("stdout %q stderr %q", out, errOut)
	}
	h.writeEvents("c")
	if _, errOut, err := h.run("forward", "--once"); err != nil {
		t.Fatalf("forward: %v\n%s", err, errOut)
	}
	if strings.Join(rec.ids, ",") != "a,b,c" {
		t.Fatalf("ingested %v", rec.ids)
	}
	for _, s := range rec.scopes {
		if s != "ws.dev.people.drew.harness" {
			t.Fatalf("X-Scope %q", s)
		}
	}
	// State lives in <config dir>/state, 0700, and the scope is cached.
	stateDir := filepath.Join(h.dir, "state")
	if fi, err := os.Stat(stateDir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("state dir %v %v", err, fi)
	}
	if _, err := os.Stat(filepath.Join(stateDir, forward.ScopeCacheFile)); err != nil {
		t.Fatal(err)
	}

	out, _, err = h.run("status")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Signed in:  drew@vero.localhost",
		"Scope:      ws.dev.people.drew.harness",
		"Service:    not installed",
		"Forwarder:  stopped",
		"3 lines in 2 batches",
		"Lag:        0 bytes behind",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status missing %q:\n%s", want, out)
		}
	}
}

func TestForwardScopeOverrideAndErrors(t *testing.T) {
	h := newHarness(t)
	rec := h.recordIngest()
	if _, _, err := h.run("login", "--no-browser"); err != nil {
		t.Fatal(err)
	}
	h.writeEvents("x")
	h.envs[config.EnvScope] = "ws.dev.people.other.harness"
	if _, errOut, err := h.run("forward", "--once", "--backfill"); err != nil {
		t.Fatalf("%v %s", err, errOut)
	}
	if len(rec.scopes) != 1 || rec.scopes[0] != "ws.dev.people.other.harness" {
		t.Fatalf("scopes %v", rec.scopes)
	}
	delete(h.envs, config.EnvScope)

	// Two writable member grants: refuse to guess.
	h.whoami = func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"principal_id":"p","grants":[{"scope":"ws.a.harness","verbs":["write"]},{"scope":"ws.b.harness","verbs":["write"]}]}`))
	}
	h.writeEvents("y")
	_, _, err := h.run("forward", "--once")
	if err == nil || !strings.Contains(err.Error(), "ws.a.harness, ws.b.harness") || !strings.Contains(err.Error(), "--scope") {
		t.Fatalf("ambiguous: %v", err)
	}
	// brainsrv refuses the batch: --once reports it and nothing advances.
	rec.status, rec.message = 403, "denied: write not granted"
	_, _, err = h.run("forward", "--once", "--scope", "ws.a.harness")
	if err == nil || !strings.Contains(err.Error(), "scope denied") {
		t.Fatalf("403: %v", err)
	}
	out, _, _ := h.run("status")
	if !strings.Contains(out, "denied: write not granted") || !strings.Contains(out, "bytes behind") {
		t.Fatalf("status after 403:\n%s", out)
	}
}

func TestForwardSignedOut(t *testing.T) {
	h := newHarness(t)
	h.recordIngest()
	h.writeEvents("a")
	_, _, err := h.run("forward", "--once")
	if err == nil || !strings.Contains(err.Error(), "afferent login") {
		t.Fatalf("got %v", err)
	}
	out, _, _ := h.run("status")
	if !strings.Contains(out, "Signed in:  no") || !strings.Contains(out, "unknown until you sign in") {
		t.Fatalf("status:\n%s", out)
	}
}

func TestServiceInstallStatusUninstall(t *testing.T) {
	h := newHarness(t)
	h.envs[config.EnvBrainsrvURL] = h.brain.URL
	out, errOut, err := h.run("service", "install")
	if err != nil {
		t.Fatalf("%v %s", err, errOut)
	}
	if !strings.Contains(errOut, "not signed in") {
		t.Errorf("no signed-out note: %q", errOut)
	}
	plist := filepath.Join(h.home, "Library", "LaunchAgents", service.Label+".plist")
	b, err := os.ReadFile(plist)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"<string>/usr/local/bin/afferent</string>",
		"<string>forward</string>",
		"<string>--config-dir</string>",
		"<string>" + h.dir + "</string>",
		"<string>" + filepath.Join(h.dir, "state", forward.ServiceLogFile) + "</string>",
	} {
		if !strings.Contains(string(b), want) {
			t.Errorf("plist missing %q:\n%s", want, b)
		}
	}
	if !strings.Contains(out, "Installed "+service.Label) {
		t.Errorf("install output %q", out)
	}
	// The service reads config.json, so install saved the brainsrv URL it
	// was given through the environment.
	if cfg, err := config.Load(h.dir); err != nil || cfg.BrainsrvURL != h.brain.URL {
		t.Fatalf("config %+v %v", cfg, err)
	}
	if fi, err := os.Stat(filepath.Join(h.dir, "state")); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("log dir %v %v", err, fi)
	}

	out, _, _ = h.run("service", "status")
	if !strings.Contains(out, "running (launchd "+service.Label+", pid 4242)") {
		t.Fatalf("service status %q", out)
	}
	if _, _, err := h.run("service", "uninstall"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(plist); !os.IsNotExist(err) {
		t.Fatal("plist left behind")
	}
	out, _, _ = h.run("service", "status")
	if !strings.Contains(out, "not installed") {
		t.Fatalf("after uninstall %q", out)
	}
	if len(h.svc.calls) != 2 || !strings.HasPrefix(h.svc.calls[0], "load ") || !strings.HasPrefix(h.svc.calls[1], "unload ") {
		t.Fatalf("loader calls %v", h.svc.calls)
	}
}

func TestServiceInstallPassesLogPath(t *testing.T) {
	h := newHarness(t)
	custom := filepath.Join(t.TempDir(), "beacon.jsonl")
	if _, _, err := h.run("service", "install", "--log-path", custom, "--program", "/opt/afferent"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(h.home, "Library", "LaunchAgents", service.Label+".plist"))
	if !strings.Contains(string(b), "<string>--log-path</string>\n    <string>"+custom+"</string>") || !strings.Contains(string(b), "<string>/opt/afferent</string>") {
		t.Fatalf("plist:\n%s", b)
	}
}
