package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func (h *harness) writeHome(rel, content string) string {
	h.t.Helper()
	p := filepath.Join(h.home, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		h.t.Fatal(err)
	}
	return p
}

func (h *harness) readHome(rel string) string {
	h.t.Helper()
	b, err := os.ReadFile(filepath.Join(h.home, rel))
	if err != nil {
		h.t.Fatal(err)
	}
	return string(b)
}

const homeClaudeJSON = "{\n  \"numStartups\": 3,\n  \"mcpServers\": {}\n}\n"
const homeCodexTOML = "model = \"gpt-5\"\n"

// agents gives the temp HOME a Claude Code, Cursor and Codex install with
// some history.
func (h *harness) agents() {
	h.writeHome(".claude.json", homeClaudeJSON)
	h.writeHome(".codex/config.toml", homeCodexTOML)
	if err := os.MkdirAll(filepath.Join(h.home, ".cursor"), 0o755); err != nil {
		h.t.Fatal(err)
	}
	h.writeHome(".claude/projects/-tmp-repo/sess-1.jsonl",
		`{"parentUuid":null,"isSidechain":false,"type":"user","message":{"role":"user","content":"hello from history"},"uuid":"u1","timestamp":"2026-09-19T22:00:00.000Z","cwd":"/tmp/repo","sessionId":"sess-1","version":"2.1.154","gitBranch":"main"}`+"\n")
}

// snapshot records every file (content and mode) under the dirs.
func snapshot(t *testing.T, dirs ...string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, d := range dirs {
		filepath.WalkDir(d, func(p string, e fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			fi, _ := e.Info()
			v := fi.Mode().String()
			if !e.IsDir() {
				b, _ := os.ReadFile(p)
				v += " " + string(b)
			}
			out[p] = v
			return nil
		})
	}
	return out
}

func diffSnapshots(a, b map[string]string) []string {
	var d []string
	for k, v := range a {
		if b[k] != v {
			d = append(d, "changed or removed: "+k)
		}
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			d = append(d, "created: "+k)
		}
	}
	return d
}

func TestMCPConfigCommand(t *testing.T) {
	h := newHarness(t)
	h.agents()
	out, _, err := h.run("mcp", "config", "--dry-run")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Claude Code: would add the brain server to", "Cursor:", "Codex:", `+    "brain": {`, "+[mcp_servers.brain]"} {
		if !strings.Contains(out, want) {
			t.Fatalf("dry run output misses %q:\n%s", want, out)
		}
	}
	if h.readHome(".claude.json") != homeClaudeJSON || h.readHome(".codex/config.toml") != homeCodexTOML {
		t.Fatal("dry run changed a file")
	}

	out, _, err = h.run("mcp", "config")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	var claude struct {
		MCPServers map[string]struct {
			Type    string
			Command string
			Args    []string
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(h.readHome(".claude.json")), &claude); err != nil {
		t.Fatal(err)
	}
	b := claude.MCPServers["brain"]
	// AFFERENT_CONFIG_DIR is set in this harness, so the entry carries it.
	if b.Command != "/usr/local/bin/afferent" || strings.Join(b.Args, " ") != "mcp proxy --config-dir "+h.dir || b.Type != "stdio" {
		t.Fatalf("claude entry %+v", b)
	}
	if !strings.Contains(h.readHome(".cursor/mcp.json"), `"command": "/usr/local/bin/afferent"`) {
		t.Fatal(h.readHome(".cursor/mcp.json"))
	}
	if !strings.Contains(h.readHome(".codex/config.toml"), `args = ["mcp", "proxy", "--config-dir", "`+h.dir+`"]`) {
		t.Fatal(h.readHome(".codex/config.toml"))
	}
	if h.readHome(".claude.json"+".afferent.bak") != homeClaudeJSON {
		t.Fatal("no backup")
	}

	out, _, _ = h.run("mcp", "config")
	if strings.Count(out, "already configured") != 3 {
		t.Fatalf("second run should be a no-op:\n%s", out)
	}
	if _, _, err := h.run("mcp", "config", "--remove"); err != nil {
		t.Fatal(err)
	}
	if h.readHome(".claude.json") != homeClaudeJSON || h.readHome(".codex/config.toml") != homeCodexTOML {
		t.Fatalf("remove did not restore:\n%s\n%s", h.readHome(".claude.json"), h.readHome(".codex/config.toml"))
	}

	// With the claude CLI on PATH, Claude Code is configured through it.
	h.lookPath = func(n string) (string, error) {
		if n == "claude" {
			return "/fake/claude", nil
		}
		return "", fmt.Errorf("not found")
	}
	if _, _, err := h.run("mcp", "config", "--harness", "claude"); err != nil {
		t.Fatal(err)
	}
	if len(h.ran) != 1 || strings.Join(h.ran[0], " ") != "/fake/claude mcp add --scope user brain -- /usr/local/bin/afferent mcp proxy --config-dir "+h.dir {
		t.Fatalf("ran %v", h.ran)
	}
}

func TestMCPProxyCommand(t *testing.T) {
	h := newHarness(t)
	if _, _, err := h.run("login", "--no-browser"); err != nil {
		t.Fatal(err)
	}
	var scopes []string
	h.mcp = func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Context") != "afferent-poc" || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ey") {
			http.Error(w, "unauthorized", 401)
			return
		}
		scopes = append(scopes, r.Header.Get("X-Scope"))
		var m struct {
			ID     json.RawMessage
			Method string
		}
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &m)
		switch {
		case m.Method == "initialize":
			w.Header().Set("Mcp-Session-Id", "s1")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2025-06-18"}}`, m.ID)
		case len(m.ID) == 0 || r.Method == http.MethodDelete:
			w.WriteHeader(202)
		default:
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "data: {\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":{\"tools\":[]}}\n\n", m.ID)
		}
	}
	h.stdin = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","clientInfo":{"name":"t"}}}` + "\n" +
		`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n" +
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}` + "\n"
	out, stderr, err := h.run("mcp", "proxy")
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], `"protocolVersion"`) || !strings.Contains(lines[1], `"tools":[]`) {
		t.Fatalf("stdout must carry only the two answers:\n%s", out)
	}
	for _, s := range scopes {
		if s != "ws.dev.people.drew.harness" {
			t.Fatalf("X-Scope %q (want the member scope from /v1/whoami)", s)
		}
	}
	if !strings.Contains(stderr, "relaying to") {
		t.Fatalf("stderr %s", stderr)
	}
}

func TestSyncCommandBackfillsForTheForwarder(t *testing.T) {
	h := newHarness(t)
	h.agents()
	rec := h.recordIngest()
	if _, _, err := h.run("login", "--no-browser"); err != nil {
		t.Fatal(err)
	}
	// History already in the log before afferent is not sent.
	h.writeEvents("pre-1")
	out, _, err := h.run("sync")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "Claude Code: 1 sessions") || !strings.Contains(out, "checkpoints now start") {
		t.Fatalf("sync output:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(h.home, ".beacon", "endpoint", "state", "claude.json")); err != nil {
		t.Fatalf("Beacon's cursor file: %v", err)
	}
	if out, _, err := h.run("forward", "--once"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	rec.mu.Lock()
	ids := append([]string(nil), rec.ids...)
	rec.mu.Unlock()
	if len(ids) == 0 {
		t.Fatal("the backfilled history was not forwarded")
	}
	for _, id := range ids {
		if id == "pre-1" {
			t.Fatal("history from before the first sync was sent")
		}
	}
	// A second sync adds nothing.
	out, _, _ = h.run("sync")
	if !strings.Contains(out, "0 events written") {
		t.Fatalf("second sync:\n%s", out)
	}
}

func TestSetupDryRunTouchesNothing(t *testing.T) {
	for _, signedIn := range []bool{false, true} {
		t.Run(fmt.Sprint("signedIn=", signedIn), func(t *testing.T) {
			h := newHarness(t)
			h.agents()
			if signedIn {
				if _, _, err := h.run("login", "--no-browser"); err != nil {
					t.Fatal(err)
				}
				// Nearly expired: a real run would refresh, a dry run must not.
				c, _ := h.store().Load()
				c.Expiry = c.Expiry.Add(-899e9)
				h.store().Save(c)
			}
			before := snapshot(t, h.home, h.dir, filepath.Dir(h.logPath))
			polls, refreshes, opened := h.authsrv.Polls(), h.authsrv.RefreshCalls.Load(), len(h.opened)
			out, _, err := h.run("setup", "--dry-run", "--harness", "claude,codex,cursor")
			if err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			if d := diffSnapshots(before, snapshot(t, h.home, h.dir, filepath.Dir(h.logPath))); len(d) > 0 {
				t.Fatalf("dry run touched files: %v\n%s", d, out)
			}
			if len(h.capture.installs) != 0 || len(h.svc.calls) != 0 || len(h.ran) != 0 {
				t.Fatalf("dry run acted: capture %v service %v ran %v", h.capture.installs, h.svc.calls, h.ran)
			}
			if h.authsrv.Polls() != polls || h.authsrv.RefreshCalls.Load() != refreshes || len(h.opened) != opened {
				t.Fatal("dry run talked to authsrv or opened a browser")
			}
			want := []string{"Dry run", "would install for Claude Code, Codex, Cursor", "would install and start it", "would add the brain server for", "would offer to backfill Claude Code"}
			if signedIn {
				want = append(want, "signed in as drew@vero.localhost", "would ask brainsrv")
			} else {
				want = append(want, "would sign in")
			}
			for _, w := range want {
				if !strings.Contains(out, w) {
					t.Errorf("output misses %q:\n%s", w, out)
				}
			}
		})
	}
}

func TestSetupYesRunsEverythingAndIsIdempotent(t *testing.T) {
	h := newHarness(t)
	h.agents()
	h.recordIngest()
	out, _, err := h.run("setup", "--yes", "--no-browser")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if len(h.capture.installs) != 3 || !strings.HasSuffix(h.capture.installs[0], h.logPath) {
		t.Fatalf("capture %v", h.capture.installs)
	}
	if _, err := h.store().Load(); err != nil {
		t.Fatalf("not signed in after setup: %v", err)
	}
	if len(h.svc.calls) == 0 {
		t.Fatal("service not installed")
	}
	if !strings.Contains(h.readHome(".claude.json"), `"brain"`) || !strings.Contains(h.readHome(".codex/config.toml"), "[mcp_servers.brain]") {
		t.Fatal("MCP config not written")
	}
	if b, _ := os.ReadFile(h.logPath); !strings.Contains(string(b), "hello from history") {
		t.Fatal("history not backfilled")
	}
	for _, want := range []string{"Your member scope: ws.dev.people.drew.harness", "Summary", "installed for Claude Code, Cursor, Codex", "backfilled Claude Code"} {
		if !strings.Contains(out, want) {
			t.Errorf("output misses %q:\n%s", want, out)
		}
	}

	h.capture.installs = nil
	out, _, err = h.run("setup", "--yes", "--skip", "service")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"already installed", "Already signed in", "already configured", "skipped (--skip)"} {
		if !strings.Contains(out, want) {
			t.Errorf("second run misses %q:\n%s", want, out)
		}
	}
	if len(h.capture.installs) != 0 {
		t.Fatal("second run reinstalled capture")
	}
}

func TestSetupDeclinesWithoutAnswers(t *testing.T) {
	h := newHarness(t)
	h.agents()
	out, _, err := h.run("setup", "--skip", "service") // stdin is empty: every question is "no"
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if len(h.capture.installs) != 0 || h.authsrv.Polls() != 0 || h.readHome(".claude.json") != homeClaudeJSON {
		t.Fatal("acted without a yes")
	}
	for _, want := range []string{"not installed (declined)", "not signed in (declined)"} {
		if !strings.Contains(out, want) {
			t.Errorf("output misses %q:\n%s", want, out)
		}
	}
	if _, _, err := h.run("setup", "--skip", "nope"); err == nil {
		t.Fatal("unknown step accepted")
	}
}
