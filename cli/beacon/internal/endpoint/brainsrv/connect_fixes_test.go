package brainsrv

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/endpoint/service"
	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/endpoint/writer"
)

func connectOK(t *testing.T, fx *connectFixture) {
	t.Helper()
	if _, err := Connect(context.Background(), fx.opts); err != nil {
		t.Fatalf("Connect: %v", err)
	}
}

func writeBuffer(t *testing.T, content string) string {
	t.Helper()
	dir := filepath.Join(DataDir(true), "buffer", "v2", "brainsrv_runtime")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "buffer-data-1.dat")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// The exiting Vector writes its checkpoint back while it drains after bootout. Connect must
// wait for it to be gone before clearing checkpoints, or the backfill silently does nothing.
func TestBackfillWaitsForTheDrainingForwarderBeforeClearingCheckpoints(t *testing.T) {
	fx := newConnectFixture(t)
	connectOK(t, fx)
	writeCheckpoint(t, 1234)
	fx.fwd.drainPolls = 3
	fx.fwd.onDrain = func() { writeCheckpoint(t, 1234) }
	fx.opts.Backfill = true
	connectOK(t, fx)
	if fx.fwd.drainPolls != 0 {
		t.Fatalf("connect did not wait for the forwarder to exit (%d polls left)", fx.fwd.drainPolls)
	}
	if _, err := os.Stat(CheckpointDir(true)); !os.IsNotExist(err) {
		t.Fatalf("a checkpoint written during the drain survived --backfill: %v", err)
	}
}

func TestBackfillFailsAndRestoresWhenTheForwarderWillNotStop(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*fakeForwarder)
	}{
		{"still running", func(f *fakeForwarder) { f.stuck = true }},
		{"unload error", func(f *fakeForwarder) { f.unloadErr = errors.New("bootout failed") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newConnectFixture(t)
			connectOK(t, fx)
			writeCheckpoint(t, 1234)
			config := readFile(t, VectorConfigPath(true))
			tc.setup(fx.fwd)
			fx.fwd.calls = nil
			fx.opts.Backfill = true
			fx.opts.StopTimeout = 20 * time.Millisecond
			_, err := Connect(context.Background(), fx.opts)
			if err == nil {
				t.Fatal("--backfill must fail when the forwarder cannot be stopped")
			}
			if cps, cpErr := ReadCheckpoints(true); cpErr != nil || len(cps) != 1 || cps[0].Position != 1234 {
				t.Fatalf("checkpoints must be left alone: %+v %v", cps, cpErr)
			}
			if got := readFile(t, VectorConfigPath(true)); got != config {
				t.Fatal("the previous config must be restored")
			}
			if last := fx.fwd.calls[len(fx.fwd.calls)-1]; last != "load" {
				t.Fatalf("the previous forwarder must be restarted: %v", fx.fwd.calls)
			}
		})
	}
}

// A reconnect that fails after writing must not leave the new key next to the old URL, and a
// backfill reconnect must not leave the old forwarder stopped with its checkpoints gone.
func TestFailedReconnectRestoresKeyConfigCheckpointsAndRestartsTheForwarder(t *testing.T) {
	fx := newConnectFixture(t)
	connectOK(t, fx)
	writeCheckpoint(t, 555)
	bufferPath := writeBuffer(t, "old lines")
	oldSecrets := readFile(t, SecretsPath(true))
	oldConfig := readFile(t, VectorConfigPath(true))

	other := newFakeBrainsrv(t)
	const newKey = "spk_other_member_key_987"
	other.key = newKey
	keyFile := filepath.Join(t.TempDir(), "new.key")
	if err := os.WriteFile(keyFile, []byte(newKey), 0o600); err != nil {
		t.Fatal(err)
	}
	fx.opts.URL, fx.opts.KeyFile, fx.opts.HTTPClient = other.server.URL, keyFile, other.server.Client()
	fx.opts.Scope = "ws.w2.people.m.harness"
	fx.opts.Backfill = true
	fx.fwd.loadErr = errors.New("not started within 10s")
	fx.fwd.calls = nil

	_, err := Connect(context.Background(), fx.opts)
	if err == nil || !strings.Contains(err.Error(), "could not be started") {
		t.Fatalf("err = %v", err)
	}
	if got := readFile(t, SecretsPath(true)); got != oldSecrets || strings.Contains(got, newKey) {
		t.Fatal("the previous key must be restored")
	}
	if got := readFile(t, VectorConfigPath(true)); got != oldConfig {
		t.Fatal("the previous config must be restored")
	}
	conn, _ := LoadConnection(true)
	if conn.URL != fx.brainsrv.server.URL {
		t.Fatalf("connection = %+v", conn)
	}
	if cps, cpErr := ReadCheckpoints(true); cpErr != nil || len(cps) != 1 || cps[0].Position != 555 {
		t.Fatalf("checkpoints must be restored: %+v %v", cps, cpErr)
	}
	if got := readFile(t, bufferPath); got != "old lines" {
		t.Fatal("the buffer must be restored")
	}
	if got := strings.Join(fx.fwd.calls, ","); !strings.HasSuffix(got, "unload,load,load") {
		t.Fatalf("the previous forwarder must be restarted after the failed load: %s", got)
	}
	entries, _ := os.ReadDir(Dir(true))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".rollback") {
			t.Errorf("leftover backup %s", e.Name())
		}
	}
	// A first connect that fails leaves no key behind either.
	fx2 := newConnectFixture(t)
	fx2.fwd.loadErr = errors.New("no")
	if _, err := Connect(context.Background(), fx2.opts); err == nil {
		t.Fatal("expected failure")
	}
	for _, path := range []string{SecretsPath(true), VectorConfigPath(true), ConnectionPath(true)} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s left by a failed first connect", path)
		}
	}
}

// Lines buffered for one (url, scope) must never be posted to another.
func TestConnectToAnotherDestinationEmptiesTheDataDir(t *testing.T) {
	fx := newConnectFixture(t)
	connectOK(t, fx)
	bufferPath := writeBuffer(t, "workspace one")

	// Same destination: buffer kept, forwarder not stopped separately.
	fx.fwd.calls = nil
	connectOK(t, fx)
	if _, err := os.Stat(bufferPath); err != nil {
		t.Fatal("a reconnect to the same destination must keep the buffer")
	}

	// Different scope, after a plain disconnect that keeps the data dir.
	if err := Disconnect(DisconnectOptions{UserMode: true, Forwarder: fx.fwd}); err != nil {
		t.Fatal(err)
	}
	fx.opts.Scope = "ws.w2.people.m.harness"
	fx.fwd.calls = nil
	connectOK(t, fx)
	if _, err := os.Stat(bufferPath); !os.IsNotExist(err) {
		t.Fatal("the buffer of the previous scope must be cleared")
	}
	if got := strings.Join(fx.fwd.calls, ","); !strings.HasSuffix(got, "unload,load") {
		t.Fatalf("the forwarder must be stopped before its data dir is cleared: %s", got)
	}
	if d := readFile(t, DestinationPath(true)); !strings.Contains(d, "ws.w2.people.m.harness") {
		t.Fatalf("destination = %s", d)
	}

	// Unknown destination (no record) with a buffer present: cleared too.
	bufferPath = writeBuffer(t, "unknown")
	if err := os.Remove(DestinationPath(true)); err != nil {
		t.Fatal(err)
	}
	connectOK(t, fx)
	if _, err := os.Stat(bufferPath); !os.IsNotExist(err) {
		t.Fatal("a buffer of unknown destination must be cleared")
	}
}

func TestRenderEscapesDollarQuoteAndIncludesArchivesOnBackfill(t *testing.T) {
	opts := RenderOptions{
		LogPath:     `/data/$team/"x"/runtime.jsonl`,
		URL:         "https://brain.example.com/brain$x",
		Scope:       "ws.w.people.m.harness",
		SecretsFile: "/state/$HOME/secrets.json",
		DataDir:     "/state/${X}/data",
	}
	out, err := RenderVectorConfig(opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`include = ["/data/$$team/\"x\"/runtime.jsonl"]`,
		`uri = "https://brain.example.com/brain$$x/v1/ingest/beacon/runtime"`,
		`path = "/state/$$HOME/secrets.json"`,
		`data_dir = "/state/$${X}/data"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %s", want)
		}
	}
	opts.LogPath = "/logs/runtime.jsonl"
	opts.Backfill = true
	out, err = RenderVectorConfig(opts)
	if err != nil {
		t.Fatal(err)
	}
	archives := writer.RetainedLogPaths(opts.LogPath)
	if len(archives) < 2 {
		t.Fatalf("archives = %v", archives)
	}
	for _, path := range archives {
		if !strings.Contains(out, `"`+path+`"`) {
			t.Errorf("--backfill include lacks %s", path)
		}
	}
	opts.LogPath = "/logs/*.jsonl"
	if _, err := RenderVectorConfig(opts); !errors.Is(err, ErrGlobLogPath) {
		t.Fatalf("glob log path: %v", err)
	}
	if !hasUnescapedReference(`a "${X}"`) || hasUnescapedReference(`a "$${X}"`) || !hasUnescapedReference(`a "$$${X}"`) {
		t.Fatal("hasUnescapedReference")
	}
}

func TestLaunchdPlistLogsIntoTheStateDirNotTmp(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("launchd plist writer only runs on macOS")
	}
	newConnectFixture(t) // temp HOME
	m := NewForwarderManager(true)
	m.Kind = service.KindLaunchd
	path, err := m.WriteUnit("/usr/local/bin/vector", VectorConfigPath(true))
	if err != nil {
		t.Fatal(err)
	}
	plist := readFile(t, path)
	if strings.Contains(plist, "/tmp/") {
		t.Fatalf("plist still logs to /tmp:\n%s", plist)
	}
	if !strings.Contains(plist, "<string>"+VectorLogPath(true)+"</string>") || !strings.Contains(plist, "<string>"+LaunchdLabel+"</string>") {
		t.Fatalf("plist:\n%s", plist)
	}
	if info, err := os.Stat(Dir(true)); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("log dir: %v %v", info, err)
	}
}

func TestStatusFlagsRefusedWritesAndUndeliveredLines(t *testing.T) {
	fx := newConnectFixture(t)
	connectOK(t, fx)
	connectedAt := fx.opts.Now()
	seen := connectedAt.Add(time.Minute)
	fx.brainsrv.endpoints = []Endpoint{{Hostname: "mac.local", LastSeen: seen}}
	if err := os.WriteFile(fx.opts.LogPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	written := seen.Add(30 * time.Minute)
	if err := os.Chtimes(fx.opts.LogPath, written, written); err != nil {
		t.Fatal(err)
	}
	base := StatusOptions{Forwarder: fx.fwd, HTTPClient: fx.brainsrv.server.Client(), Hostname: "mac.local"}

	opts := base
	opts.Now = func() time.Time { return written.Add(time.Minute) }
	s := Status(context.Background(), true, opts)
	if s.Health != "ok" || s.Write != "ok" || len(s.Warnings) != 0 {
		t.Fatalf("a fresh write still in its batch window must not warn: %+v", s)
	}
	if s.VectorLog != VectorLogPath(true) {
		t.Fatalf("vector log = %q", s.VectorLog)
	}

	opts.Now = func() time.Time { return written.Add(10 * time.Minute) }
	s = Status(context.Background(), true, opts)
	if len(s.Warnings) != 1 || !strings.Contains(s.Warnings[0], "dropped") || !strings.Contains(s.Warnings[0], VectorLogPath(true)) {
		t.Fatalf("undelivered lines must be flagged: %+v", s.Warnings)
	}

	fx.brainsrv.writeStatus = http.StatusForbidden
	fx.brainsrv.endpoints[0].LastSeen = written
	s = Status(context.Background(), true, opts)
	if s.Health != "ok" || s.Write != "forbidden" || len(s.Warnings) != 1 || !strings.Contains(s.Warnings[0], "refuses writes") {
		t.Fatalf("a key that lost write must be flagged even when read still works: %+v", s)
	}
}
