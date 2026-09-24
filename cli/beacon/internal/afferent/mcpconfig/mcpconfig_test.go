package mcpconfig

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

var entry = Entry{Command: "/opt/homebrew/bin/afferent", Args: []string{"mcp", "proxy"}}

// noCLI is a LookPath that finds nothing: the tests never run a real
// claude or codex binary.
func noCLI(string) (string, error) { return "", exec.ErrNotFound }

func opts(home string) Options { return Options{Home: home, LookPath: noCLI} }

func writeFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func apply(t *testing.T, h string, o Options) Result {
	t.Helper()
	r, err := Apply(context.Background(), h, entry, o)
	if err != nil {
		t.Fatalf("%s: %v", h, err)
	}
	return r
}

const claudeJSON = `{
  "numStartups": 42,
  "projects": {
    "/Users/x/p": {"allowedTools": [], "mcpServers": {"local": {"command": "x"}}}
  },
  "mcpServers": {
    "github": {
      "type": "http",
      "url": "https://api.githubcopilot.com/mcp/"
    }
  },
  "zeta": "last"
}
`

func TestClaudeFileEditPreservesEverythingElse(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".claude.json")
	writeFile(t, path, claudeJSON, 0o600)

	r := apply(t, Claude, opts(home))
	if r.Action != "added" || r.Via != "file" || r.Backup != path+BackupSuffix {
		t.Fatalf("result %+v", r)
	}
	got := read(t, path)
	if read(t, r.Backup) != claudeJSON {
		t.Fatal("backup is not the original")
	}
	// Byte-identical outside the inserted member.
	want := strings.Replace(claudeJSON, `      "url": "https://api.githubcopilot.com/mcp/"
    }
  },`, `      "url": "https://api.githubcopilot.com/mcp/"
    },
    "brain": {
      "args": [
        "mcp",
        "proxy"
      ],
      "command": "/opt/homebrew/bin/afferent",
      "env": {},
      "type": "stdio"
    }
  },`, 1)
	if got != want {
		t.Fatalf("edited file:\n%s\nwant:\n%s", got, want)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}

	// Idempotent: nothing changes, no new backup.
	os.Remove(r.Backup)
	if r := apply(t, Claude, opts(home)); r.Action != "unchanged" || r.Backup != "" {
		t.Fatalf("second run %+v", r)
	}
	if read(t, path) != got {
		t.Fatal("second run changed the file")
	}
	if _, err := os.Stat(path + BackupSuffix); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a no-op wrote a backup")
	}

	// A different binary path updates the entry in place.
	o := opts(home)
	moved := Entry{Command: "/usr/local/bin/afferent", Args: entry.Args}
	r, err := Apply(context.Background(), Claude, moved, o)
	if err != nil || r.Action != "updated" || !strings.Contains(read(t, path), "/usr/local/bin/afferent") {
		t.Fatalf("update %v %+v", err, r)
	}

	// Remove restores the original bytes.
	o.Remove = true
	if r := apply(t, Claude, o); r.Action != "removed" {
		t.Fatalf("remove %+v", r)
	}
	if read(t, path) != claudeJSON {
		t.Fatalf("after remove:\n%s", read(t, path))
	}
	if r := apply(t, Claude, o); r.Action != "absent" {
		t.Fatalf("second remove %+v", r)
	}
}

func TestClaudeUsesCLIWhenPresent(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".claude.json")
	writeFile(t, path, claudeJSON, 0o600)
	var ran [][]string
	o := Options{
		Home:     home,
		LookPath: func(n string) (string, error) { return "/fake/bin/" + n, nil },
		Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			ran = append(ran, append([]string{name}, args...))
			return nil, nil
		},
	}
	r := apply(t, Claude, o)
	if r.Via != "claude CLI" || r.Action != "added" {
		t.Fatalf("result %+v", r)
	}
	want := "/fake/bin/claude mcp add --scope user brain -- /opt/homebrew/bin/afferent mcp proxy"
	if len(ran) != 1 || strings.Join(ran[0], " ") != want {
		t.Fatalf("ran %v", ran)
	}
	if read(t, path) != claudeJSON {
		t.Fatal("with the CLI, afferent must not edit the file itself")
	}
	if read(t, path+BackupSuffix) != claudeJSON {
		t.Fatal("no backup before the CLI edit")
	}

	// Already registered (as the CLI would have written it): no command.
	writeFile(t, path, `{"mcpServers":{"brain":{"type":"stdio","command":"/opt/homebrew/bin/afferent","args":["mcp","proxy"],"env":{}}}}`, 0o600)
	ran = nil
	if r := apply(t, Claude, o); r.Action != "unchanged" || len(ran) != 0 {
		t.Fatalf("idempotent %+v %v", r, ran)
	}
	// Remove goes through the CLI too.
	o.Remove = true
	if r := apply(t, Claude, o); r.Action != "removed" || len(ran) != 1 || strings.Join(ran[0][1:], " ") != "mcp remove --scope user brain" {
		t.Fatalf("remove %+v %v", r, ran)
	}

	// A failing CLI falls back to the file edit.
	o.Remove = false
	writeFile(t, path, claudeJSON, 0o600)
	o.Run = func(context.Context, string, ...string) ([]byte, error) { return []byte("boom"), errors.New("exit 1") }
	r = apply(t, Claude, o)
	if r.Via != "file" || !strings.Contains(r.Note, "boom") || !strings.Contains(read(t, path), `"brain"`) {
		t.Fatalf("fallback %+v", r)
	}
}

func TestCursorCreateAndPreserve(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".cursor", "mcp.json")

	// No file yet: created.
	r := apply(t, Cursor, opts(home))
	if r.Action != "added" || r.Backup != "" {
		t.Fatalf("create %+v", r)
	}
	var doc map[string]map[string]map[string]any
	if err := json.Unmarshal([]byte(read(t, path)), &doc); err != nil || doc["mcpServers"]["brain"]["command"] != entry.Command {
		t.Fatalf("created %v %s", err, read(t, path))
	}

	// Existing servers and other keys survive; empty mcpServers works.
	for _, orig := range []string{
		"{\n\t\"mcpServers\": {\n\t\t\"other\": {\"command\": \"o\"}\n\t},\n\t\"x\": 1\n}\n",
		`{"mcpServers":{}}`,
		`{}`,
	} {
		writeFile(t, path, orig, 0o644)
		if r := apply(t, Cursor, opts(home)); r.Action != "added" {
			t.Fatalf("%q: %+v", orig, r)
		}
		got := read(t, path)
		var d map[string]any
		if err := json.Unmarshal([]byte(got), &d); err != nil {
			t.Fatalf("%q → invalid JSON %v:\n%s", orig, err, got)
		}
		if strings.Contains(orig, "other") && (!strings.Contains(got, `"other": {"command": "o"}`) || !strings.Contains(got, `"x": 1`) || !strings.Contains(got, "\t\t\"brain\": {")) {
			t.Fatalf("lost content or indentation:\n%s", got)
		}
		o := opts(home)
		o.Remove = true
		apply(t, Cursor, o)
		if orig == `{}` {
			// The mcpServers object afferent added stays, empty.
			if got := read(t, path); !json.Valid([]byte(got)) || strings.Contains(got, "brain") {
				t.Fatalf("remove from {}:\n%s", got)
			}
			continue
		}
		if read(t, path) != orig {
			t.Fatalf("remove did not restore %q:\n%s", orig, read(t, path))
		}
	}

	// Not JSON: refused, untouched.
	writeFile(t, path, "{ // comment\n}", 0o644)
	if _, err := Apply(context.Background(), Cursor, entry, opts(home)); err == nil || read(t, path) != "{ // comment\n}" {
		t.Fatalf("invalid JSON must be refused: %v", err)
	}
}

const codexTOML = `model = "gpt-5"

[mcp_servers.docs]
command = "docs-mcp"
args = ["--stdio"]

[profiles.fast]
model = "gpt-5-mini"
`

func TestCodexBlock(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".codex", "config.toml")
	writeFile(t, path, codexTOML, 0o600)

	r := apply(t, Codex, opts(home))
	if r.Action != "added" || read(t, r.Backup) != codexTOML {
		t.Fatalf("add %+v", r)
	}
	got := read(t, path)
	if !strings.HasPrefix(got, codexTOML) {
		t.Fatalf("existing content changed:\n%s", got)
	}
	for _, want := range []string{"\n[mcp_servers.brain]\n", `command = "/opt/homebrew/bin/afferent"`, `args = ["mcp", "proxy"]`, tomlBegin, tomlEnd} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q:\n%s", want, got)
		}
	}
	if r := apply(t, Codex, opts(home)); r.Action != "unchanged" || read(t, path) != got {
		t.Fatalf("idempotent %+v", r)
	}

	// The user edits after our block; an update replaces only the block.
	writeFile(t, path, got+"\n[tui]\nnotifications = true\n", 0o600)
	moved := Entry{Command: `C:\Program Files\afferent "x".exe`, Args: entry.Args}
	r, err := Apply(context.Background(), Codex, moved, opts(home))
	if err != nil || r.Action != "updated" {
		t.Fatalf("update %v %+v", err, r)
	}
	got = read(t, path)
	if !strings.Contains(got, `command = "C:\\Program Files\\afferent \"x\".exe"`) || !strings.HasSuffix(got, "[tui]\nnotifications = true\n") || strings.Count(got, tomlBegin) != 1 {
		t.Fatalf("update:\n%s", got)
	}

	o := opts(home)
	o.Remove = true
	if r := apply(t, Codex, o); r.Action != "removed" {
		t.Fatalf("remove %+v", r)
	}
	if got := read(t, path); got != codexTOML+"\n[tui]\nnotifications = true\n" {
		t.Fatalf("after remove:\n%q", got)
	}

	// A file without a trailing newline, and a new file.
	writeFile(t, path, `model = "x"`, 0o600)
	apply(t, Codex, opts(home))
	if got := read(t, path); !strings.HasPrefix(got, "model = \"x\"\n\n"+tomlBegin) {
		t.Fatalf("no-newline file:\n%q", got)
	}
	os.RemoveAll(filepath.Join(home, ".codex"))
	apply(t, Codex, opts(home))
	if got := read(t, path); !strings.HasPrefix(got, tomlBegin) {
		t.Fatalf("new file:\n%q", got)
	}
}

func TestCodexConflicts(t *testing.T) {
	for name, content := range map[string]string{
		"table":       "[mcp_servers.brain]\ncommand = \"x\"\n",
		"quoted":      "[mcp_servers.\"brain\"]\ncommand = \"x\"\n",
		"subtable":    "[mcp_servers.brain.env]\nA = \"1\"\n",
		"under":       "[mcp_servers]\nbrain = { command = \"x\" }\n",
		"inline root": "mcp_servers = { other = { command = \"x\" } }\n",
		"open block":  tomlBegin + "\n[mcp_servers.brain]\n",
	} {
		home := t.TempDir()
		path := filepath.Join(home, ".codex", "config.toml")
		writeFile(t, path, content, 0o600)
		if _, err := Apply(context.Background(), Codex, entry, opts(home)); err == nil {
			t.Errorf("%s: expected a refusal", name)
		}
		if read(t, path) != content {
			t.Errorf("%s: file changed", name)
		}
	}
}

func TestDryRunTouchesNothing(t *testing.T) {
	home := t.TempDir()
	writeFile(t, filepath.Join(home, ".claude.json"), claudeJSON, 0o600)
	writeFile(t, filepath.Join(home, ".codex", "config.toml"), codexTOML, 0o600)
	ran := 0
	o := Options{Home: home, DryRun: true,
		LookPath: func(n string) (string, error) { return "/fake/" + n, nil },
		Run:      func(context.Context, string, ...string) ([]byte, error) { ran++; return nil, nil },
	}
	for _, h := range All {
		r := apply(t, h, o)
		if r.Action != "added" || r.Diff == "" || r.Backup != "" {
			t.Fatalf("%s dry run %+v", h, r)
		}
		if !strings.Contains(r.Diff, "+") || !strings.Contains(r.Diff, `brain`) {
			t.Fatalf("%s diff:\n%s", h, r.Diff)
		}
	}
	if ran != 0 {
		t.Fatal("dry run ran the claude CLI")
	}
	if read(t, filepath.Join(home, ".claude.json")) != claudeJSON || read(t, filepath.Join(home, ".codex", "config.toml")) != codexTOML {
		t.Fatal("dry run changed a file")
	}
	if _, err := os.Stat(filepath.Join(home, ".cursor")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("dry run created ~/.cursor")
	}
	entries, _ := os.ReadDir(home)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), BackupSuffix) {
			t.Fatal("dry run wrote a backup")
		}
	}
}

func TestParseAndDetect(t *testing.T) {
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, ".cursor"), 0o755)
	o := opts(home)
	if got := Detect(o); len(got) != 1 || got[0] != Cursor {
		t.Fatalf("detect %v", got)
	}
	if got, err := ParseHarnesses("claude-code, codex,claude", o); err != nil || strings.Join(got, ",") != "claude,codex" {
		t.Fatalf("parse %v %v", got, err)
	}
	if _, err := ParseHarnesses("vim", o); err == nil {
		t.Fatal("unknown harness accepted")
	}
	if got, _ := ParseHarnesses("all", o); len(got) != 3 {
		t.Fatalf("all %v", got)
	}
}

func TestSymlinkRefused(t *testing.T) {
	home := t.TempDir()
	real := filepath.Join(t.TempDir(), "real.json")
	writeFile(t, real, "{}", 0o600)
	os.MkdirAll(filepath.Join(home, ".cursor"), 0o755)
	if err := os.Symlink(real, filepath.Join(home, ".cursor", "mcp.json")); err != nil {
		t.Skip(err)
	}
	if _, err := Apply(context.Background(), Cursor, entry, opts(home)); err == nil {
		t.Fatal("editing through a symlink must be refused")
	}
}
