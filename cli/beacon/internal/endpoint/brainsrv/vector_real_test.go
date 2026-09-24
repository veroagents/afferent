package brainsrv

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/endpoint/asymptote"
)

// realVector finds an installed Vector for the validate test: BEACON_VECTOR_BIN, then vector
// on PATH, then ~/.local/bin/vector. The test skips when there is none, or when it is older
// than asymptote.MinVectorVersion.
func realVector(t *testing.T) asymptote.VectorInfo {
	t.Helper()
	var candidates []string
	if bin := os.Getenv(asymptote.VectorBinEnv); bin != "" {
		candidates = append(candidates, bin)
	}
	if bin, err := exec.LookPath("vector"); err == nil {
		candidates = append(candidates, bin)
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".local", "bin", "vector"))
	}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err != nil || info.IsDir() {
			continue
		}
		vector, err := asymptote.FindVector(candidate)
		if err != nil {
			t.Skipf("Vector at %s is unusable: %v", candidate, err)
		}
		return vector
	}
	t.Skip("no Vector installed (set " + asymptote.VectorBinEnv + "); skipping the real vector validate")
	return asymptote.VectorInfo{}
}

// The rendered forwarder config, both the default and the --backfill variant, is accepted by
// a real Vector >= MinVectorVersion.
func TestRenderedConfigPassesRealVectorValidate(t *testing.T) {
	vector := realVector(t)
	for _, backfill := range []bool{false, true} {
		dir := t.TempDir()
		opts := RenderOptions{
			LogPath:  filepath.Join(dir, "runtime.jsonl"),
			URL:      "https://brain.example.com",
			Scope:    "ws.w1.people.m.harness",
			Backfill: backfill,
		}
		if err := preflightVectorConfig(vector.Path, dir, opts); err != nil {
			t.Fatalf("Vector %s rejected the rendered config (backfill=%t): %v", vector.Version, backfill, err)
		}
	}
	// The hand-run template with its environment references set.
	dir := t.TempDir()
	secrets := filepath.Join(dir, SecretsFileName)
	if err := os.WriteFile(secrets, []byte(SecretsFileContent("spk_test")), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "data"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := InstallPack(filepath.Join(dir, "pack"), filepath.Join(dir, "runtime.jsonl")); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(vector.Path, "validate", "--skip-healthchecks", filepath.Join(dir, "pack", "vector.toml"))
	cmd.Env = append(os.Environ(),
		EnvURL+"=https://brain.example.com",
		EnvScope+"=ws.w1.people.m.harness",
		EnvSecretsFile+"="+secrets,
		EnvDataDir+"="+filepath.Join(dir, "data"),
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Vector rejected the pack template: %v\n%s", err, out)
	}
}

// Literal values with $ and " survive Vector's interpolation and TOML parsing: a real Vector
// accepts the render (it fails with "Missing environment variable" on an unescaped $VAR).
func TestRenderedConfigWithDollarAndQuotePassesRealVectorValidate(t *testing.T) {
	vector := realVector(t)
	dir := filepath.Join(t.TempDir(), "state$FOOBAR_UNSET")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	opts := RenderOptions{
		LogPath:  filepath.Join(dir, `logs "q" $NOPE_UNSET`, "runtime.jsonl"),
		URL:      "https://brain.example.com/brain$x",
		Scope:    "ws.w1.people.m.harness",
		Backfill: true,
	}
	if err := preflightVectorConfig(vector.Path, dir, opts); err != nil {
		t.Fatalf("Vector %s rejected a render with $ and \" in literals: %v", vector.Version, err)
	}
}
