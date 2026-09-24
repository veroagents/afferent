package brainsrv

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/endpoint/service"
	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/testenv"
)

const testKey = "spk_member_key_0123456789"

type fakeForwarder struct {
	unitPath  string
	calls     []string
	loadErr   error
	unloadErr error
	// loaded is what Status reports. drainPolls keeps it loaded for that many Status calls
	// after Unload (launchd's bootout returns while Vector drains); onDrain runs on each of
	// them, standing in for the exiting Vector. stuck never lets it stop.
	loaded     bool
	drainPolls int
	onDrain    func()
	stuck      bool
}

func (f *fakeForwarder) Supported() bool           { return true }
func (f *fakeForwarder) UnsupportedReason() string { return "" }
func (f *fakeForwarder) Label() string             { return LaunchdLabel }
func (f *fakeForwarder) UnitPath() (string, error) { return f.unitPath, nil }
func (f *fakeForwarder) WriteUnit(vectorBin, configPath string) (string, error) {
	f.calls = append(f.calls, "write "+vectorBin+" --config "+configPath)
	if f.unitPath != "" {
		if err := os.WriteFile(f.unitPath, []byte("unit"), 0o644); err != nil {
			return "", err
		}
	}
	return f.unitPath, nil
}
func (f *fakeForwarder) Load() error {
	f.calls = append(f.calls, "load")
	if f.loadErr == nil {
		f.loaded = true
	}
	return f.loadErr
}
func (f *fakeForwarder) Unload() error {
	f.calls = append(f.calls, "unload")
	if f.unloadErr != nil {
		return f.unloadErr
	}
	if f.drainPolls == 0 && !f.stuck {
		f.loaded = false
	}
	return nil
}
func (f *fakeForwarder) RemoveUnits() {
	f.calls = append(f.calls, "remove")
	if f.unitPath != "" {
		_ = os.Remove(f.unitPath)
	}
}
func (f *fakeForwarder) Status() service.Status {
	if f.loaded && !f.stuck && f.drainPolls > 0 && len(f.calls) > 0 && f.calls[len(f.calls)-1] == "unload" {
		f.drainPolls--
		if f.onDrain != nil {
			f.onDrain()
		}
		if f.drainPolls == 0 {
			f.loaded = false
		}
	}
	return service.Status{Label: LaunchdLabel, Kind: string(service.KindLaunchd), Loaded: f.loaded, Running: f.loaded}
}

func (f *fakeForwarder) touched() bool { return len(f.calls) > 0 }

// fakeBrainsrv answers the two Beacon ingest routes with fixed status codes.
type fakeBrainsrv struct {
	server       *httptest.Server
	healthStatus int
	writeStatus  int
	endpoints    []Endpoint
	key          string
	mu           sync.Mutex
	requests     []string
}

func newFakeBrainsrv(t *testing.T) *fakeBrainsrv {
	t.Helper()
	f := &fakeBrainsrv{healthStatus: http.StatusOK, writeStatus: http.StatusOK, key: testKey}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests = append(f.requests, r.Method+" "+r.URL.RequestURI())
		f.mu.Unlock()
		f.mu.Lock()
		key := f.key
		f.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+key {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid credentials"}`))
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == HealthPath:
			if r.URL.Query().Get("scope") == "" {
				t.Errorf("health called without ?scope=")
			}
			w.WriteHeader(f.healthStatus)
			if f.healthStatus == http.StatusOK {
				_ = json.NewEncoder(w).Encode(HealthResponse{OK: true, Endpoints: f.endpoints})
			} else {
				_, _ = w.Write([]byte(`{"error":"denied: read not granted"}`))
			}
		case r.Method == http.MethodPost && r.URL.Path == RuntimeIngestPath:
			if r.Header.Get("X-Scope") == "" || r.Header.Get("Content-Type") != "application/x-ndjson" {
				t.Errorf("write probe headers: %v", r.Header)
			}
			w.WriteHeader(f.writeStatus)
			_, _ = w.Write([]byte(`{"accepted":0,"duplicate":0,"rejected":0,"sessions_touched":0}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeBrainsrv) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

// fakeVector writes an executable that answers --version and validate like Vector 0.56.
func fakeVector(t *testing.T, validateExit int) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake vector script needs a POSIX shell")
	}
	path := filepath.Join(t.TempDir(), "vector")
	script := "#!/bin/sh\ncase \"$1\" in\n  --version) echo \"vector 0.56.0 (aarch64-apple-darwin test)\";;\n  validate) echo invalid >&2; exit " + strconv.Itoa(validateExit) + ";;\n  *) exit 0;;\nesac\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeKeyFile(t *testing.T, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "beacon.key")
	if err := os.WriteFile(path, []byte(testKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

type connectFixture struct {
	brainsrv *fakeBrainsrv
	fwd      *fakeForwarder
	opts     ConnectOptions
}

func newConnectFixture(t *testing.T) *connectFixture {
	t.Helper()
	home := t.TempDir()
	testenv.SetHome(t, home)
	t.Setenv("BEACON_VECTOR_BIN", "")
	fb := newFakeBrainsrv(t)
	fwd := &fakeForwarder{unitPath: filepath.Join(home, "unit.plist")}
	oldPoll := stopPollInterval
	stopPollInterval = time.Millisecond
	t.Cleanup(func() { stopPollInterval = oldPoll })
	return &connectFixture{
		brainsrv: fb,
		fwd:      fwd,
		opts: ConnectOptions{
			UserMode:   true,
			URL:        fb.server.URL,
			Scope:      "ws.w1.people.m.harness",
			KeyFile:    writeKeyFile(t, 0o600),
			LogPath:    filepath.Join(home, "runtime.jsonl"),
			VectorBin:  fakeVector(t, 0),
			Forwarder:  fwd,
			HTTPClient: fb.server.Client(),
			Now:        func() time.Time { return time.Date(2026, 9, 24, 1, 0, 0, 0, time.UTC) },
		},
	}
}

func assertNothingWritten(t *testing.T, fx *connectFixture) {
	t.Helper()
	for _, path := range []string{SecretsPath(true), VectorConfigPath(true), ConnectionPath(true)} {
		if _, err := os.Stat(path); err == nil {
			t.Errorf("%s was written by a refused connect", path)
		}
	}
	if fx.fwd.touched() {
		t.Errorf("refused connect touched the service manager: %v", fx.fwd.calls)
	}
}

func TestConnectWritesSecretsConfigUnitAndRecord(t *testing.T) {
	fx := newConnectFixture(t)
	result, err := Connect(context.Background(), fx.opts)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if info, err := os.Stat(Dir(true)); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("state dir: %v %v", info, err)
	}
	if testenv.HasPOSIXFileModes() {
		for path, mode := range map[string]os.FileMode{SecretsPath(true): 0o600, ConnectionPath(true): 0o600, VectorConfigPath(true): 0o644} {
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != mode {
				t.Errorf("%s: %v %v, want %#o", path, info, err, mode)
			}
		}
	}
	secrets, _ := os.ReadFile(SecretsPath(true))
	if string(secrets) != SecretsFileContent(testKey) {
		t.Fatalf("secrets = %q", secrets)
	}
	config, _ := os.ReadFile(VectorConfigPath(true))
	for _, want := range []string{
		`uri = "` + fx.brainsrv.server.URL + `/v1/ingest/beacon/runtime"`,
		`X-Scope = "ws.w1.people.m.harness"`,
		`read_from = "end"`,
		`data_dir = "` + DataDir(true) + `"`,
		`path = "` + SecretsPath(true) + `"`,
	} {
		if !strings.Contains(string(config), want) {
			t.Errorf("config missing %q", want)
		}
	}
	if strings.Contains(string(config), testKey) {
		t.Fatal("the key leaked into vector.toml")
	}
	if len(fx.fwd.calls) != 2 || !strings.HasPrefix(fx.fwd.calls[0], "write ") || fx.fwd.calls[1] != "load" {
		t.Fatalf("forwarder calls = %v", fx.fwd.calls)
	}
	conn, err := LoadConnection(true)
	if err != nil || conn.Scope != fx.opts.Scope || conn.KeyPrefix != "spk_member_k" || conn.VectorVersion != "0.56.0" {
		t.Fatalf("connection = %+v %v", conn, err)
	}
	if result.Forwarder != LaunchdLabel || result.Reconnected {
		t.Fatalf("result = %+v", result)
	}
	if got := strings.Join(fx.brainsrv.requests, ","); !strings.Contains(got, "GET /v1/ingest/beacon/health?scope=ws.w1.people.m.harness") || !strings.Contains(got, "POST /v1/ingest/beacon/runtime") {
		t.Fatalf("brainsrv requests = %s", got)
	}
	// Nothing preflight-scratch is left behind.
	entries, _ := os.ReadDir(Dir(true))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			t.Errorf("leftover %s", e.Name())
		}
	}
}

func TestConnectRefusesWhenBrainsrvDeniesTheKey(t *testing.T) {
	cases := []struct {
		name          string
		health, write int
		want          error
	}{
		{"health 401", http.StatusUnauthorized, http.StatusOK, ErrUnauthorized},
		{"health 403", http.StatusForbidden, http.StatusOK, ErrForbidden},
		{"write 403", http.StatusOK, http.StatusForbidden, ErrForbidden},
		{"write 401", http.StatusOK, http.StatusUnauthorized, ErrUnauthorized},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fx := newConnectFixture(t)
			fx.brainsrv.healthStatus, fx.brainsrv.writeStatus = c.health, c.write
			_, err := Connect(context.Background(), fx.opts)
			if !errors.Is(err, c.want) || !strings.Contains(err.Error(), "refusing to connect") {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
			if c.want == ErrForbidden && !strings.Contains(err.Error(), fx.opts.Scope) {
				t.Fatalf("403 error must name the scope: %v", err)
			}
			assertNothingWritten(t, fx)
		})
	}
}

func TestConnectRefusesAWrongKeyAndAnUnreachableServer(t *testing.T) {
	fx := newConnectFixture(t)
	path := filepath.Join(t.TempDir(), "other.key")
	if err := os.WriteFile(path, []byte("spk_someone_else"), 0o600); err != nil {
		t.Fatal(err)
	}
	fx.opts.KeyFile = path
	if _, err := Connect(context.Background(), fx.opts); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("err = %v", err)
	}
	assertNothingWritten(t, fx)

	fx = newConnectFixture(t)
	fx.brainsrv.server.Close()
	if _, err := Connect(context.Background(), fx.opts); err == nil || !strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("err = %v", err)
	}
	assertNothingWritten(t, fx)
}

func TestConnectSurfacesKeyFilePermissionAndSymlinkRejection(t *testing.T) {
	if !testenv.HasPOSIXFileModes() {
		t.Skip("POSIX file modes only")
	}
	fx := newConnectFixture(t)
	fx.opts.KeyFile = writeKeyFile(t, 0o644)
	_, err := Connect(context.Background(), fx.opts)
	if err == nil || !strings.Contains(err.Error(), "--key-file") || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("0644 key file: err = %v", err)
	}

	link := filepath.Join(t.TempDir(), "link.key")
	if err := os.Symlink(writeKeyFile(t, 0o600), link); err != nil {
		t.Fatal(err)
	}
	fx.opts.KeyFile = link
	_, err = Connect(context.Background(), fx.opts)
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink key file: err = %v", err)
	}
	if fx.brainsrv.requestCount() != 0 {
		t.Fatal("a rejected key file must be refused before any network call")
	}
	assertNothingWritten(t, fx)
}

func TestConnectValidatesURLAndScopeBeforeReadingTheKey(t *testing.T) {
	fx := newConnectFixture(t)
	fx.opts.URL = "http://brain.example.com"
	if _, err := Connect(context.Background(), fx.opts); err == nil || !strings.Contains(err.Error(), "--url") {
		t.Fatalf("err = %v", err)
	}
	fx.opts.URL = fx.brainsrv.server.URL
	fx.opts.Scope = "Not.A.Scope"
	if _, err := Connect(context.Background(), fx.opts); err == nil || !strings.Contains(err.Error(), "--scope") {
		t.Fatalf("err = %v", err)
	}
	if fx.brainsrv.requestCount() != 0 {
		t.Fatal("invalid flags must be refused before any network call")
	}
	assertNothingWritten(t, fx)
}

func TestConnectRefusesAConfigVectorRejects(t *testing.T) {
	fx := newConnectFixture(t)
	fx.opts.VectorBin = fakeVector(t, 1)
	_, err := Connect(context.Background(), fx.opts)
	if err == nil || !strings.Contains(err.Error(), "vector validate failed") {
		t.Fatalf("err = %v", err)
	}
	assertNothingWritten(t, fx)
}

func TestFirstConnectThatFailsToLoadRemovesTheUnit(t *testing.T) {
	fx := newConnectFixture(t)
	fx.fwd.loadErr = errors.New("launchd said no")
	_, err := Connect(context.Background(), fx.opts)
	if err == nil || !strings.Contains(err.Error(), "could not be started") {
		t.Fatalf("err = %v", err)
	}
	if got := strings.Join(fx.fwd.calls, ","); !strings.HasSuffix(got, "load,unload,remove") {
		t.Fatalf("calls = %s", got)
	}
	if _, err := os.Stat(ConnectionPath(true)); err == nil {
		t.Fatal("a failed connect must not record a connection")
	}
}

func TestBackfillClearsCheckpointsAndALaterConnectResumes(t *testing.T) {
	fx := newConnectFixture(t)
	if _, err := Connect(context.Background(), fx.opts); err != nil {
		t.Fatal(err)
	}
	writeCheckpoint(t, 1234)

	fx.opts.Backfill = true
	fx.fwd.calls = nil
	result, err := Connect(context.Background(), fx.opts)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Reconnected || !result.Connection.Backfill {
		t.Fatalf("result = %+v", result)
	}
	if _, err := os.Stat(CheckpointDir(true)); !os.IsNotExist(err) {
		t.Fatalf("--backfill must clear the checkpoints: %v", err)
	}
	if got := strings.Join(fx.fwd.calls, ","); !strings.HasPrefix(got, "write ") || !strings.HasSuffix(got, ",unload,load") {
		t.Fatalf("--backfill must write the new unit, then stop Vector before clearing checkpoints and loading: %v", fx.fwd.calls)
	}
	config, _ := os.ReadFile(VectorConfigPath(true))
	if !strings.Contains(string(config), `read_from = "beginning"`) {
		t.Fatal("--backfill must render read_from = beginning")
	}

	writeCheckpoint(t, 99)
	fx.opts.Backfill = false
	if _, err := Connect(context.Background(), fx.opts); err != nil {
		t.Fatal(err)
	}
	config, _ = os.ReadFile(VectorConfigPath(true))
	if !strings.Contains(string(config), `read_from = "end"`) {
		t.Fatal("a connect without --backfill must render read_from = end")
	}
	cps, err := ReadCheckpoints(true)
	if err != nil || len(cps) != 1 || cps[0].Position != 99 {
		t.Fatalf("checkpoints must survive a plain reconnect: %+v %v", cps, err)
	}
}

func writeCheckpoint(t *testing.T, position int64) {
	t.Helper()
	if err := os.MkdirAll(CheckpointDir(true), 0o700); err != nil {
		t.Fatal(err)
	}
	doc := `{"version":"1","checkpoints":[{"fingerprint":{"first_lines_checksum":42},"position":` + strconv.FormatInt(position, 10) + `,"modified":"2026-09-24T01:00:00Z"}]}`
	if err := os.WriteFile(filepath.Join(CheckpointDir(true), "checkpoints.json"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDisconnectRemovesKeyAndUnitButKeepsDataUnlessPurged(t *testing.T) {
	fx := newConnectFixture(t)
	if _, err := Connect(context.Background(), fx.opts); err != nil {
		t.Fatal(err)
	}
	writeCheckpoint(t, 7)
	fx.fwd.calls = nil
	if err := Disconnect(DisconnectOptions{UserMode: true, Forwarder: fx.fwd}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(fx.fwd.calls, ","); got != "unload,remove" {
		t.Fatalf("calls = %s", got)
	}
	for _, path := range []string{SecretsPath(true), VectorConfigPath(true), ConnectionPath(true), fx.fwd.unitPath} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s survived disconnect", path)
		}
	}
	if _, err := ReadCheckpoints(true); err != nil {
		t.Fatalf("data dir must be kept without --purge: %v", err)
	}

	// Nothing installed any more: a second disconnect leaves the service manager alone.
	fx.fwd.calls = nil
	if err := Disconnect(DisconnectOptions{UserMode: true, Forwarder: fx.fwd, Purge: true}); err != nil {
		t.Fatal(err)
	}
	if fx.fwd.touched() {
		t.Fatalf("disconnect with nothing installed touched the service manager: %v", fx.fwd.calls)
	}
	if _, err := os.Stat(Dir(true)); !os.IsNotExist(err) {
		t.Fatal("--purge must remove the state dir")
	}
}

func TestStatusReportsCheckpointAndLastSeen(t *testing.T) {
	fx := newConnectFixture(t)
	if s := Status(context.Background(), true, StatusOptions{Forwarder: fx.fwd, SkipHealthCheck: true}); s.Connected {
		t.Fatalf("status before connect = %+v", s)
	}
	if _, err := Connect(context.Background(), fx.opts); err != nil {
		t.Fatal(err)
	}
	seen := time.Date(2026, 9, 24, 2, 0, 0, 0, time.UTC)
	fx.brainsrv.endpoints = []Endpoint{{Hostname: "other", LastSeen: seen.Add(time.Hour)}, {Hostname: "mac.local", LastSeen: seen}}
	s := Status(context.Background(), true, StatusOptions{Forwarder: fx.fwd, HTTPClient: fx.brainsrv.server.Client(), Hostname: "mac.local"})
	if !s.Connected || !s.Forwarder.Running || s.Health != "ok" || s.LastSeen == nil || !s.LastSeen.Equal(seen) {
		t.Fatalf("status = %+v", s)
	}
	if s.CheckpointMessage == "" || len(s.Checkpoints) != 0 {
		t.Fatalf("no checkpoint yet should be explained: %+v", s)
	}
	writeCheckpoint(t, 4096)
	fx.brainsrv.healthStatus = http.StatusUnauthorized
	s = Status(context.Background(), true, StatusOptions{Forwarder: fx.fwd, HTTPClient: fx.brainsrv.server.Client()})
	if len(s.Checkpoints) != 1 || s.Checkpoints[0].Position != 4096 || s.Health != "unauthorized" {
		t.Fatalf("status = %+v", s)
	}
	encoded, _ := json.Marshal(s)
	if strings.Contains(string(encoded), testKey) {
		t.Fatal("status leaked the key")
	}
}
