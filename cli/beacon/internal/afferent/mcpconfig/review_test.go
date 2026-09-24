package mcpconfig

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// The dry-run diff must never echo other servers' secrets: no context
// lines, and string values other than afferent's own are redacted.
func TestDiffRedactsOtherServersSecrets(t *testing.T) {
	cases := map[string]struct{ h, path, content string }{
		"cursor pretty": {Cursor, ".cursor/mcp.json",
			"{\n  \"mcpServers\": {\n    \"github\": {\n      \"command\": \"npx\",\n      \"env\": {\n        \"GITHUB_PERSONAL_ACCESS_TOKEN\": \"ghp_secret\"\n      }\n    }\n  }\n}\n"},
		"cursor compact": {Cursor, ".cursor/mcp.json",
			`{"mcpServers":{"github":{"command":"npx","env":{"GITHUB_PERSONAL_ACCESS_TOKEN":"ghp_secret"}}}}`},
		"claude headers": {Claude, ".claude.json",
			"{\n  \"mcpServers\": {\n    \"api\": {\n      \"type\": \"http\",\n      \"url\": \"https://x\",\n      \"headers\": {\n        \"Authorization\": \"Bearer ghp_secret\"\n      }\n    }\n  }\n}\n"},
		"claude update": {Claude, ".claude.json",
			`{"mcpServers":{"brain":{"type":"http","url":"https://x","headers":{"Authorization":"Bearer ghp_secret"}}}}`},
		"codex tail": {Codex, ".codex/config.toml",
			"[mcp_servers.github]\ncommand = \"npx\"\nenv = { GITHUB_PERSONAL_ACCESS_TOKEN = \"ghp_secret\" }\n"},
		"codex literal tail": {Codex, ".codex/config.toml",
			"[mcp_servers.github]\ncommand = 'npx'\nenv = { TOKEN = 'ghp_secret' }"},
	}
	for name, c := range cases {
		home := t.TempDir()
		writeFile(t, filepath.Join(home, c.path), c.content, 0o600)
		o := opts(home)
		o.DryRun = true
		r, err := Apply(context.Background(), c.h, entry, o)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if r.Diff == "" {
			t.Fatalf("%s: no diff", name)
		}
		if strings.Contains(r.Diff, "ghp_secret") {
			t.Errorf("%s: diff leaks a secret:\n%s", name, r.Diff)
		}
		if !strings.Contains(r.Diff, entry.Command) {
			t.Errorf("%s: diff hides afferent's own command:\n%s", name, r.Diff)
		}
	}
}

func TestRedactLine(t *testing.T) {
	allowed := map[string]bool{"keep": true}
	for in, want := range map[string]string{
		`"k": "v",`:                `"k": "<redacted>",`,
		`"k":"keep"`:               `"k":"keep"`,
		`"k": "a\"b", "j": "keep"`: `"k": "<redacted>", "j": "keep"`,
		`x = 'lit'`:                `x = "<redacted>"`,
		`"a" = "b"`:                `"a" = "<redacted>"`,
		`args = ["keep", "other"]`: `args = ["keep", "<redacted>"]`,
		`"k": "unterminated`:       `"k": "<redacted>"`,
		`    },`:                   `    },`,
	} {
		if got := redactLine(in, allowed); got != want {
			t.Errorf("redactLine(%q) = %q, want %q", in, got, want)
		}
	}
}

// A single-quoted (literal) key names the same table as a bare one.
func TestCodexConflictLiteralKey(t *testing.T) {
	for _, content := range []string{
		"[mcp_servers.'brain']\ncommand = \"other\"\n",
		"[ mcp_servers . 'brain' ]\ncommand = \"other\"\n",
		"['mcp_servers'.brain.env]\nA = \"1\"\n",
	} {
		home := t.TempDir()
		path := filepath.Join(home, ".codex", "config.toml")
		writeFile(t, path, content, 0o600)
		if _, err := Apply(context.Background(), Codex, entry, opts(home)); err == nil {
			t.Errorf("%q: expected a refusal", content)
		}
		if read(t, path) != content {
			t.Errorf("%q: file changed", content)
		}
	}
}
