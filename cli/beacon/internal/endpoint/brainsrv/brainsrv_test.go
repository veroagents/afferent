package brainsrv

import (
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/endpoint/asymptote"
	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/endpoint/service"
	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/testenv"
)

var updateGolden = flag.Bool("update", false, "rewrite testdata golden files")

func goldenRenderOptions() RenderOptions {
	return RenderOptions{
		LogPath:     "/Users/me/.beacon/logs/runtime.jsonl",
		URL:         "https://brain.example.com/",
		Scope:       "ws.w1.people.m.harness",
		SecretsFile: "/Users/me/.beacon/endpoint/afferent-brainsrv/vector-secrets.json",
		DataDir:     "/Users/me/.beacon/endpoint/afferent-brainsrv/vector-data",
	}
}

func TestRenderVectorConfigMatchesGolden(t *testing.T) {
	got, err := RenderVectorConfig(goldenRenderOptions())
	if err != nil {
		t.Fatal(err)
	}
	golden := filepath.Join("testdata", "vector.toml.golden")
	if *updateGolden {
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("%v (run go test ./internal/endpoint/brainsrv -update)", err)
	}
	if got != string(want) {
		t.Fatalf("rendered config differs from %s (run with -update if intended):\n%s", golden, got)
	}
}

func TestRenderVectorConfigSubstitutesEveryLiteral(t *testing.T) {
	got, err := RenderVectorConfig(goldenRenderOptions())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`data_dir = "/Users/me/.beacon/endpoint/afferent-brainsrv/vector-data"`,
		`path = "/Users/me/.beacon/endpoint/afferent-brainsrv/vector-secrets.json"`,
		`include = ["/Users/me/.beacon/logs/runtime.jsonl"]`,
		`read_from = "end"`,
		`uri = "https://brain.example.com/v1/ingest/beacon/runtime"`,
		`compression = "gzip"`,
		`token = "SECRET[beacon.brainsrv_key]"`,
		`method = "newline_delimited"`,
		`Content-Type = "application/x-ndjson"`,
		`X-Scope = "ws.w1.people.m.harness"`,
		"max_bytes = 5000000\nmax_events = 5000",
		"type = \"disk\"\nmax_size = 536870912\nwhen_full = \"block\"",
		"retry_initial_backoff_secs = 2\nretry_max_duration_secs = 300",
		`uri = "https://brain.example.com/v1/ingest/beacon/health?scope=ws.w1.people.m.harness"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("render missing %q", want)
		}
	}
	for _, forbidden := range []string{"${", "inventory", "asymptote", "BEACON_PRIVACY_TRANSFORMS", "device_key"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("render contains %q", forbidden)
		}
	}
	template, err := VectorConfig("/x/runtime.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(template, privacyTransformMarker) {
		t.Fatal("template must keep the # BEACON_PRIVACY_TRANSFORMS hook")
	}
}

func TestRenderVectorConfigBackfillReadsFromBeginning(t *testing.T) {
	opts := goldenRenderOptions()
	opts.Backfill = true
	got, err := RenderVectorConfig(opts)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, `read_from = "beginning"`) || strings.Contains(got, `read_from = "end"`) {
		t.Fatalf("backfill render:\n%s", got)
	}
}

func TestRenderVectorConfigRefusesUnsafeInput(t *testing.T) {
	cases := map[string]func(*RenderOptions){
		"plain http to a remote host": func(o *RenderOptions) { o.URL = "http://brain.example.com" },
		"no url":                      func(o *RenderOptions) { o.URL = "" },
		"uppercase scope":             func(o *RenderOptions) { o.Scope = "WS.x" },
		"scope with a quote":          func(o *RenderOptions) { o.Scope = `ws"x` },
		"empty scope":                 func(o *RenderOptions) { o.Scope = "" },
		"no secrets file":             func(o *RenderOptions) { o.SecretsFile = "" },
		"no data dir":                 func(o *RenderOptions) { o.DataDir = "" },
	}
	for name, mutate := range cases {
		opts := goldenRenderOptions()
		mutate(&opts)
		if _, err := RenderVectorConfig(opts); err == nil {
			t.Errorf("%s: render succeeded", name)
		}
	}
	opts := goldenRenderOptions()
	opts.URL = "http://127.0.0.1:8077"
	if _, err := RenderVectorConfig(opts); err != nil {
		t.Fatalf("loopback http must be allowed: %v", err)
	}
	opts.URL = "http://brain.example.com"
	if _, err := RenderVectorConfig(opts); !errors.Is(err, ErrInsecureURL) {
		t.Fatalf("want ErrInsecureURL, got %v", err)
	}
}

func TestHealthURLEncodesTheScope(t *testing.T) {
	if got := HealthURL("https://b.example/", "ws.a_b.people.m"); got != "https://b.example/v1/ingest/beacon/health?scope=ws.a_b.people.m" {
		t.Fatalf("got %q", got)
	}
	if got := HealthURL("https://b.example", "a&b=c d"); got != "https://b.example/v1/ingest/beacon/health?scope=a%26b%3Dc+d" {
		t.Fatalf("scope not query-escaped: %q", got)
	}
	if got := HealthURL("https://b.example", ""); got != "https://b.example/v1/ingest/beacon/health" {
		t.Fatalf("got %q", got)
	}
}

func TestSecretsFileContentIsTheSecretBackendShape(t *testing.T) {
	if got := SecretsFileContent(`spk_a"b`); got != `{"brainsrv_key":"spk_a\"b"}`+"\n" {
		t.Fatalf("got %q", got)
	}
}

func TestInstallPackWritesTemplateAndReadme(t *testing.T) {
	out := t.TempDir()
	if err := InstallPack(out, "/logs/runtime.jsonl"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(out, "vector.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `include = ["/logs/runtime.jsonl"]`) || !strings.Contains(string(data), "${"+EnvURL+"}") {
		t.Fatalf("pack vector.toml:\n%s", data)
	}
	if _, err := os.Stat(filepath.Join(out, "README.md")); err != nil {
		t.Fatal(err)
	}
	files, err := Files()
	if err != nil || len(files) != 2 {
		t.Fatalf("Files() = %d, %v", len(files), err)
	}
}

// Coexistence with Beacon Managed (SPEC B3): both forwarders render, and they share no
// service name, state path, secrets file or Vector data_dir (so no checkpoints or buffer).
func TestCoexistsWithTheBeaconManagedForwarder(t *testing.T) {
	testenv.SetHome(t, t.TempDir())
	for _, userMode := range []bool{true, false} {
		ours := StatePaths(userMode)
		theirs := []string{asymptote.Dir(userMode), asymptote.EnrollmentPath(userMode), asymptote.SecretsPath(userMode),
			asymptote.VectorConfigPath(userMode), asymptote.DataDir(userMode), asymptote.InstallIDPath(userMode), asymptote.ConnectPendingPath(userMode)}
		for _, a := range ours {
			for _, b := range theirs {
				if a == b || within(a, b) || within(b, a) {
					t.Errorf("user=%t: brainsrv path %s overlaps asymptote path %s", userMode, a, b)
				}
			}
		}

		asym, err := asymptote.RenderVectorConfig(asymptote.RenderOptions{
			LogPath: "/logs/runtime.jsonl", IngestURL: "https://ingest.example.com",
			SecretsFile: asymptote.SecretsPath(userMode), DataDir: asymptote.DataDir(userMode),
		})
		if err != nil {
			t.Fatal(err)
		}
		ourRender, err := RenderVectorConfig(RenderOptions{
			LogPath: "/logs/runtime.jsonl", URL: "https://brain.example.com", Scope: "ws.w.people.m.harness",
			SecretsFile: SecretsPath(userMode), DataDir: DataDir(userMode),
		})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(ourRender, asymptote.DataDir(userMode)) || strings.Contains(asym, DataDir(userMode)) ||
			strings.Contains(ourRender, asymptote.SecretsPath(userMode)) || strings.Contains(asym, SecretsPath(userMode)) {
			t.Error("rendered configs share a data_dir or secrets file")
		}
	}
	for _, kind := range []service.Kind{service.KindLaunchd, service.KindSystemd} {
		for _, userMode := range []bool{true, false} {
			ours := NewForwarderManager(userMode)
			ours.Kind = kind
			theirs := service.ForwarderManager{UserMode: userMode, Kind: kind}
			if ours.Label() == theirs.Label() {
				t.Errorf("%s: both forwarders are %s", kind, ours.Label())
			}
			op, _ := ours.UnitPath()
			tp, _ := theirs.UnitPath()
			if op == tp {
				t.Errorf("%s user=%t: both forwarders write %s", kind, userMode, op)
			}
		}
	}
	if LaunchdLabel != "com.afferent.brainsrv-forwarder" || SystemdUnit != "afferent-brainsrv-forwarder.service" {
		t.Fatal("service names changed")
	}
}

func within(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != "." && !strings.HasPrefix(rel, "..")
}
