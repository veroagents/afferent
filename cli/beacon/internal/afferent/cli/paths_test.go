package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/afferent/config"
	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/afferent/forward"
	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/afferent/service"
)

// A relative --config-dir / AFFERENT_STATE_DIR must be written absolute
// into the service unit and agents' MCP config, which run elsewhere.
func TestRelativeDirsAreWrittenAbsolute(t *testing.T) {
	h := newHarness(t)
	t.Chdir(t.TempDir())
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	h.envs[config.EnvConfigDir] = ""
	h.envs[config.EnvStateDir] = "st"
	if _, errOut, err := h.run("--config-dir", "aff", "service", "install"); err != nil {
		t.Fatalf("%v %s", err, errOut)
	}
	b, err := os.ReadFile(filepath.Join(h.home, "Library", "LaunchAgents", service.Label+".plist"))
	if err != nil {
		t.Fatal(err)
	}
	plist := string(b)
	for _, want := range []string{
		"<string>" + filepath.Join(wd, "aff") + "</string>",
		"<string>" + filepath.Join(wd, "st") + "</string>",
		filepath.Join(wd, "st", forward.ServiceLogFile),
	} {
		if !strings.Contains(plist, want) {
			t.Errorf("plist lacks %q:\n%s", want, plist)
		}
	}
	for _, bad := range []string{"<string>aff</string>", "<string>st</string>"} {
		if strings.Contains(plist, bad) {
			t.Errorf("plist has relative %q", bad)
		}
	}

	h.lookPath = func(string) (string, error) { return "", os.ErrNotExist }
	if _, errOut, err := h.run("--config-dir", "aff", "mcp", "config", "--harness", "cursor"); err != nil {
		t.Fatalf("%v %s", err, errOut)
	}
	mj, err := os.ReadFile(filepath.Join(h.home, ".cursor", "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(mj), filepath.Join(wd, "aff")) {
		t.Fatalf("mcp.json has no absolute config dir:\n%s", mj)
	}
}

// login and logout forget the cached member scope, which belongs to the
// previous session's member.
func TestLoginAndLogoutClearTheScopeCache(t *testing.T) {
	h := newHarness(t)
	cache := filepath.Join(config.StateDir(h.dir, func(string) string { return "" }), forward.ScopeCacheFile)
	put := func() {
		if err := os.MkdirAll(filepath.Dir(cache), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(cache, []byte(`{"scope":"ws.previous.harness"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	put()
	if _, errOut, err := h.run("login", "--no-browser"); err != nil {
		t.Fatalf("%v %s", err, errOut)
	}
	if _, err := os.Stat(cache); !os.IsNotExist(err) {
		t.Fatal("login kept the previous scope cache")
	}
	put()
	if _, errOut, err := h.run("logout"); err != nil {
		t.Fatalf("%v %s", err, errOut)
	}
	if _, err := os.Stat(cache); !os.IsNotExist(err) {
		t.Fatal("logout kept the scope cache")
	}
}

func TestSyncHarnessOrder(t *testing.T) {
	got, err := syncHarnesses("codex,claude", t.TempDir())
	if err != nil || strings.Join(got, ",") != "claude,codex" {
		t.Fatalf("%v %v: Claude must sweep before Codex", got, err)
	}
}
