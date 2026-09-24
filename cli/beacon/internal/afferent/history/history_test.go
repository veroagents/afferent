package history

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func q(s string) string { b, _ := json.Marshal(s); return string(b) }

func claudeUser(session, uuid, text string) string {
	return `{"parentUuid":null,"isSidechain":false,"type":"user","message":{"role":"user","content":` + q(text) + `},"uuid":` + q(uuid) +
		`,"timestamp":"2026-09-19T22:00:00.000Z","cwd":"/tmp/repo","sessionId":` + q(session) + `,"version":"2.1.154","gitBranch":"main"}` + "\n"
}

func codexSession(id, text string) string {
	return `{"timestamp":"2026-09-19T22:00:00.000Z","type":"session_meta","payload":{"session_id":` + q(id) + `,"id":` + q(id) +
		`,"timestamp":"2026-09-19T22:00:00.000Z","cwd":"/tmp/repo","originator":"codex-tui","cli_version":"0.153.4","source":"cli"}}` + "\n" +
		`{"timestamp":"2026-09-19T22:00:02.000Z","type":"response_item","payload":{"type":"message","id":"m1","role":"user","content":[{"type":"input_text","text":` + q(text) + `}]}}` + "\n"
}

func write(t *testing.T, path, content string, mtime time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func logText(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(b)
}

func countLines(t *testing.T, path string) int {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	n := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		n++
	}
	return n
}

func TestSyncClaudeAndCodexWithBeaconCursors(t *testing.T) {
	home := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "runtime.jsonl")
	now := time.Now()
	write(t, filepath.Join(home, ".claude", "projects", "-tmp-repo", "new.jsonl"), claudeUser("new", "u1", "recent prompt"), now.Add(-time.Hour))
	write(t, filepath.Join(home, ".claude", "projects", "-tmp-repo", "old.jsonl"), claudeUser("old", "u1", "ancient prompt"), now.Add(-90*24*time.Hour))
	write(t, filepath.Join(home, ".codex", "sessions", "2026", "09", "19", "rollout-2026-09-19T22-00-00-cx1.jsonl"), codexSession("cx1", "codex prompt"), now.Add(-time.Hour))

	if !Present(home, Claude) || !Present(home, Codex) || Present(t.TempDir(), Claude) {
		t.Fatal("Present")
	}
	o := Options{Home: home, LogPath: logPath, UserMode: true, Since: 30 * 24 * time.Hour, Now: func() time.Time { return now }}
	rc, err := Sync(Claude, o)
	if err != nil {
		t.Fatal(err)
	}
	if rc.SkippedOld != 1 || rc.Events == 0 || rc.StatePath != filepath.Join(home, ".beacon", "endpoint", "state", "claude.json") {
		t.Fatalf("claude %+v", rc)
	}
	rx, err := Sync(Codex, o)
	if err != nil || rx.Events == 0 {
		t.Fatalf("codex %v %+v", err, rx)
	}
	text := logText(t, logPath)
	for _, want := range []string{"recent prompt", "codex prompt"} {
		if !strings.Contains(text, want) {
			t.Fatalf("log misses %q", want)
		}
	}
	if strings.Contains(text, "ancient prompt") {
		t.Fatal("--since did not skip the old session")
	}
	// Beacon's own cursor files hold the progress: a second sweep, by
	// afferent or by `beacon endpoint claude sync`, writes nothing.
	for _, h := range All {
		if _, err := os.Stat(StatePath(home, h, true)); err != nil {
			t.Fatalf("%s cursor: %v", h, err)
		}
	}
	before := countLines(t, logPath)
	o.Since = 0
	if r, err := Sync(Claude, o); err != nil || r.Events != 0 {
		t.Fatalf("second claude sweep %v %+v", err, r)
	}
	if r, err := Sync(Codex, o); err != nil || r.Events != 0 {
		t.Fatalf("second codex sweep %v %+v", err, r)
	}
	if countLines(t, logPath) != before {
		t.Fatal("a repeat sweep wrote to the log")
	}

	// The old session resumes: only the new record is collected.
	old := filepath.Join(home, ".claude", "projects", "-tmp-repo", "old.jsonl")
	write(t, old, claudeUser("old", "u1", "ancient prompt")+claudeUser("old", "u2", "resumed prompt"), now)
	if _, err := Sync(Claude, o); err != nil {
		t.Fatal(err)
	}
	text = logText(t, logPath)
	if !strings.Contains(text, "resumed prompt") || strings.Contains(text, "ancient prompt") {
		t.Fatal("a resumed old session should continue from its end")
	}
}

func TestSyncValidates(t *testing.T) {
	if _, err := Sync(Claude, Options{}); err == nil {
		t.Fatal("empty options")
	}
	if _, err := Sync("cursor", Options{Home: t.TempDir(), LogPath: "x"}); err == nil {
		t.Fatal("unsupported harness")
	}
}
