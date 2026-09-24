package brainsrv

// Env-gated live test against a real brainsrv and a real Vector (PLAN §4 Phase 5 E2E, SPEC §6
// step 7). It is skipped unless AFFERENT_LIVE_BRAINSRV_URL is set and a Vector >=
// MinVectorVersion is found (see realVector). It never installs a service: Connect runs with the
// fake service manager, and the test runs the Vector binary Connect would have put in the unit
// directly, on the config Connect rendered, under a temp HOME.
//
//	AFFERENT_LIVE_BRAINSRV_URL          e.g. http://localhost:18077
//	AFFERENT_LIVE_BRAINSRV_KEY_FILE     a 0600 spk_ key file for the scope, or
//	AFFERENT_LIVE_BRAINSRV_ADMIN_TOKEN  a bootstrap token to provision one (context below)
//	AFFERENT_LIVE_BRAINSRV_SCOPE        default ws.dev.people.m.harness
//	AFFERENT_LIVE_BRAINSRV_CONTEXT      default afferent-forwarder-e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/brainsrvcfg"
	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/testenv"
)

const (
	envLiveURL        = "AFFERENT_LIVE_BRAINSRV_URL"
	envLiveKeyFile    = "AFFERENT_LIVE_BRAINSRV_KEY_FILE"
	envLiveAdminToken = "AFFERENT_LIVE_BRAINSRV_ADMIN_TOKEN"
	envLiveScope      = "AFFERENT_LIVE_BRAINSRV_SCOPE"
	envLiveContext    = "AFFERENT_LIVE_BRAINSRV_CONTEXT"
)

func liveEnv(name, def string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return def
}

func liveJSON(t *testing.T, method, url, bearer string, in, out any) int {
	t.Helper()
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(method, url, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		t.Logf("%s %s -> %d %s", method, url, resp.StatusCode, body)
	} else if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			t.Fatalf("%s %s: decode %q: %v", method, url, body, err)
		}
	}
	return resp.StatusCode
}

// liveKey returns the member key for scope: from the key file, or minted with the admin token
// (principal kind user, read/write/forget on scope, narrow_scope = scope).
func liveKey(t *testing.T, base, scope string) string {
	t.Helper()
	if path := liveEnv(envLiveKeyFile, ""); path != "" {
		key, err := brainsrvcfg.ReadKeyFile(path)
		if err != nil {
			t.Fatalf("%s: %v", envLiveKeyFile, err)
		}
		return key
	}
	admin := liveEnv(envLiveAdminToken, "")
	if admin == "" {
		t.Skipf("%s is set but neither %s nor %s is", envLiveURL, envLiveKeyFile, envLiveAdminToken)
	}
	slug := liveEnv(envLiveContext, "afferent-forwarder-e2e")
	if code := liveJSON(t, http.MethodPost, base+"/v1/admin/contexts", admin,
		map[string]string{"slug": slug, "isolation": "schema"}, nil); code != http.StatusCreated && code != http.StatusBadRequest && code != http.StatusConflict {
		t.Fatalf("create context: HTTP %d", code)
	}
	var principal struct {
		ID string `json:"id"`
	}
	if code := liveJSON(t, http.MethodPost, base+"/v1/admin/principals", admin,
		map[string]string{"context_slug": slug, "kind": "user", "display": "afferent-forwarder-live-test"}, &principal); code != http.StatusCreated {
		t.Fatalf("create principal: HTTP %d", code)
	}
	if code := liveJSON(t, http.MethodPost, base+"/v1/admin/grants", admin, map[string]any{
		"context_slug": slug, "principal_id": principal.ID, "scope_path": scope, "verbs": []string{"read", "write", "forget"},
	}, nil); code != http.StatusCreated {
		t.Fatalf("create grant: HTTP %d", code)
	}
	var key struct {
		Secret string `json:"secret"`
	}
	if code := liveJSON(t, http.MethodPost, base+"/v1/admin/api-keys", admin,
		map[string]string{"principal_id": principal.ID, "narrow_scope": scope}, &key); code != http.StatusCreated {
		t.Fatalf("mint key: HTTP %d", code)
	}
	return key.Secret
}

// liveEvents returns a Beacon-shaped prompt/response pair from host, with ids unique to run.
func liveEvents(host, run string, at time.Time) string {
	common := func(action, category, id string, seq int, ts time.Time) map[string]any {
		return map[string]any{
			"timestamp": ts.UTC().Format(time.RFC3339Nano), "vendor": "beacon", "product": "endpoint-agent", "schema_version": "1.0",
			"event":    map[string]any{"kind": "agent_runtime", "action": action, "category": category, "id": id, "fidelity": "observed"},
			"severity": "info",
			"endpoint": map[string]any{"hostname": host, "os": "darwin", "agent_version": "1.2.7"},
			"user":     map[string]any{"name": "dev"},
			"harness":  map[string]any{"name": "codex", "version": "0.40.0", "collection_method": "hook"},
			"sequence": seq, "session": map[string]any{"id": "afferent-live-" + run},
			"repository": "git@github.com:acme/afferent_live.git", "branch": "main",
		}
	}
	prompt := common("prompt.submitted", "prompt", "afferent-live-"+run+"-1", 1, at)
	prompt["prompt"] = map[string]any{"text": "Which forwarder ships beacon runtime to brainsrv?"}
	prompt["message"] = "Prompt submitted"
	reply := common("agent.message", "session", "afferent-live-"+run+"-2", 2, at.Add(time.Second))
	reply["gen_ai"] = map[string]any{"output": map[string]any{"messages": []any{map[string]any{
		"role": "assistant", "parts": []any{map[string]any{"type": "text", "content": "The afferent brainsrv Vector forwarder."}}}}}}
	reply["message"] = "Agent message"
	var b strings.Builder
	for _, e := range []map[string]any{prompt, reply} {
		line, _ := json.Marshal(e)
		b.Write(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// runVector runs the forwarder Connect rendered, as the unit would (vector --config <path>),
// with the batch timeout shortened in a copy so the test does not wait a minute per batch. It
// returns a stop function that SIGTERMs Vector and waits for it to flush its checkpoints.
func runVector(t *testing.T, vectorBin, configPath string) func() {
	t.Helper()
	rendered, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	fast := strings.Replace(string(rendered), "timeout_secs = 60", "timeout_secs = 1", 1)
	if fast == string(rendered) {
		t.Fatal("rendered config has no 60s batch timeout to shorten")
	}
	path := filepath.Join(t.TempDir(), "vector-fast.toml")
	if err := os.WriteFile(path, []byte(fast), 0o600); err != nil {
		t.Fatal(err)
	}
	logFile, err := os.Create(filepath.Join(t.TempDir(), "vector.log"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(vectorBin, "--config", path)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(70 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
		_ = logFile.Close()
		if t.Failed() {
			out, _ := os.ReadFile(logFile.Name())
			t.Logf("vector log:\n%s", out)
		}
	}
	t.Cleanup(stop)
	return stop
}

func liveEndpoint(t *testing.T, client Client, scope, host string) *Endpoint {
	t.Helper()
	health, err := client.Health(context.Background(), scope)
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	for i := range health.Endpoints {
		if health.Endpoints[i].Hostname == host {
			return &health.Endpoints[i]
		}
	}
	return nil
}

// waitEndpoint polls health?scope= until host's row satisfies ok.
func waitEndpoint(t *testing.T, client Client, scope, host, what string, ok func(*Endpoint) bool) *Endpoint {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for {
		if ep := liveEndpoint(t, client, scope, host); ep != nil && ok(ep) {
			return ep
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s (host %s under %s)", what, host, scope)
		}
		time.Sleep(time.Second)
	}
}

func accepted(ep *Endpoint) int {
	if ep == nil || ep.LastBatchAccepted == nil {
		return -1
	}
	return *ep.LastBatchAccepted
}

func TestLiveConnectForwardsResumesAndBackfillsWithoutDuplicates(t *testing.T) {
	rawURL := liveEnv(envLiveURL, "")
	if rawURL == "" {
		t.Skip(envLiveURL + " not set; skipping the live brainsrv forwarder test")
	}
	vector := realVector(t)
	base, err := brainsrvcfg.ValidateURL(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	scope := liveEnv(envLiveScope, "ws.dev.people.m.harness")
	key := liveKey(t, base, scope)
	client := Client{BaseURL: base, Key: key}

	home := t.TempDir()
	testenv.SetHome(t, home)
	t.Setenv("BEACON_VECTOR_BIN", "")
	oldPoll := stopPollInterval
	stopPollInterval = 10 * time.Millisecond
	t.Cleanup(func() { stopPollInterval = oldPoll })

	keyDir := t.TempDir()
	keyFile := filepath.Join(keyDir, "beacon.key")
	if err := os.WriteFile(keyFile, []byte(key+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run := fmt.Sprintf("%d", time.Now().UnixNano())
	host := "afferent-live-" + run
	logPath := filepath.Join(home, "logs", "runtime.jsonl")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte(liveEvents(host, run, time.Now().Add(-time.Minute))), 0o600); err != nil {
		t.Fatal(err)
	}
	fwd := &fakeForwarder{unitPath: filepath.Join(home, "unit.plist")}
	opts := ConnectOptions{
		UserMode: true, URL: base, Scope: scope, KeyFile: keyFile, LogPath: logPath,
		VectorBin: vector.Path, Forwarder: fwd,
	}
	nothingWritten := func(t *testing.T) {
		t.Helper()
		for _, path := range []string{SecretsPath(true), VectorConfigPath(true), ConnectionPath(true)} {
			if _, err := os.Stat(path); err == nil {
				t.Fatalf("%s was written by a refused connect", path)
			}
		}
		if fwd.touched() {
			t.Fatalf("refused connect touched the service manager: %v", fwd.calls)
		}
	}

	// A group-readable key file is refused before brainsrv is asked.
	if testenv.HasPOSIXFileModes() {
		loose := filepath.Join(keyDir, "loose.key")
		if err := os.WriteFile(loose, []byte(key+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(loose, 0o644); err != nil {
			t.Fatal(err)
		}
		bad := opts
		bad.KeyFile = loose
		if _, err := Connect(context.Background(), bad); err == nil || !strings.Contains(err.Error(), "--key-file") {
			t.Fatalf("connect with a 0644 key file: err = %v, want a --key-file refusal", err)
		}
		nothingWritten(t)
	}

	// A scope outside the key's grant (its parent) is refused by the live health check.
	outside := scope[:strings.LastIndex(scope, ".")]
	bad := opts
	bad.Scope = outside
	if _, err := Connect(context.Background(), bad); err == nil || !(errors.Is(err, ErrForbidden) || errors.Is(err, ErrUnauthorized)) {
		t.Fatalf("connect with out-of-grant scope %s: err = %v, want a 403/401 refusal", outside, err)
	}
	nothingWritten(t)

	// connect --backfill: the existing log is sent.
	opts.Backfill = true
	if _, err := Connect(context.Background(), opts); err != nil {
		t.Fatalf("connect --backfill: %v", err)
	}
	wantUnit := "write " + vector.Path + " --config " + VectorConfigPath(true)
	if len(fwd.calls) == 0 || fwd.calls[0] != wantUnit {
		t.Fatalf("service manager calls = %v, want first %q", fwd.calls, wantUnit)
	}
	stop := runVector(t, vector.Path, VectorConfigPath(true))
	first := waitEndpoint(t, client, scope, host, "the backfilled batch", func(ep *Endpoint) bool { return accepted(ep) == 2 })
	stop()
	checkpoints, err := ReadCheckpoints(true)
	if err != nil || len(checkpoints) == 0 {
		t.Fatalf("no checkpoint after delivery: %v %v", checkpoints, err)
	}

	// connect without --backfill resumes from the checkpoint: a restart re-sends nothing.
	opts.Backfill = false
	if _, err := Connect(context.Background(), opts); err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	stop = runVector(t, vector.Path, VectorConfigPath(true))
	time.Sleep(8 * time.Second)
	if ep := liveEndpoint(t, client, scope, host); ep == nil || !ep.LastSeen.Equal(first.LastSeen) {
		t.Fatalf("restart without --backfill re-sent lines: before %+v after %+v", first, ep)
	}
	stop()

	// connect --backfill again clears the checkpoints: every line is re-sent and brainsrv
	// reports it as a duplicate (last_batch_accepted = 0), SPEC §6 step 7.
	opts.Backfill = true
	if _, err := Connect(context.Background(), opts); err != nil {
		t.Fatalf("connect --backfill again: %v", err)
	}
	stop = runVector(t, vector.Path, VectorConfigPath(true))
	waitEndpoint(t, client, scope, host, "the duplicate backfill batch", func(ep *Endpoint) bool {
		return ep.LastSeen.After(first.LastSeen) && accepted(ep) == 0
	})
	stop()
}
