package history

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func codexPrompts(id string, n int) string {
	var b strings.Builder
	b.WriteString(codexSession(id, "prompt-"+id+"-0"))
	for i := 1; i < n; i++ {
		fmt.Fprintf(&b, `{"timestamp":"2026-09-19T22:00:%02d.000Z","type":"response_item","payload":{"type":"message","id":"m%d","role":"user","content":[{"type":"input_text","text":%s}]}}`+"\n",
			i%60, i+1, q(fmt.Sprintf("prompt-%s-%d", id, i)))
	}
	return b.String()
}

func eventIDs(t *testing.T, path string) []string {
	t.Helper()
	var ids []string
	for _, l := range strings.Split(strings.TrimSpace(logText(t, path)), "\n") {
		if l == "" {
			continue
		}
		var e struct {
			Event struct {
				ID string `json:"id"`
			} `json:"event"`
		}
		if err := json.Unmarshal([]byte(l), &e); err != nil {
			t.Fatal(err)
		}
		if e.Event.ID == "" {
			t.Fatalf("no event id: %s", l)
		}
		ids = append(ids, e.Event.ID)
	}
	return ids
}

// The Codex sweep stops when the log has no room for undelivered data
// (RetentionLimited), keeps its cursors short of what it did not write,
// and the next sweeps finish the job with nothing lost.
func TestCodexSweepStopsAtRoom(t *testing.T) {
	now := time.Now()
	mk := func() (string, string) {
		home := t.TempDir()
		for _, id := range []string{"cxa", "cxb", "cxc"} {
			write(t, filepath.Join(home, ".codex", "sessions", "2026", "09", "19", "rollout-2026-09-19T22-00-00-"+id+".jsonl"), codexPrompts(id, 6), now.Add(-time.Hour))
		}
		return home, filepath.Join(t.TempDir(), "runtime.jsonl")
	}

	// Reference: no limit.
	home, ref := mk()
	r, err := Sync(Codex, Options{Home: home, LogPath: ref, UserMode: true})
	if err != nil || r.RetentionLimited || r.Events == 0 {
		t.Fatalf("unlimited %v %+v", err, r)
	}
	want := eventIDs(t, ref)

	// Limited: room for a few events per sweep.
	home, logPath := mk()
	var budget int64
	o := Options{Home: home, LogPath: logPath, UserMode: true, Room: func() (int64, error) {
		fi, _ := os.Stat(logPath)
		var used int64
		if fi != nil {
			used = fi.Size()
		}
		return max(budget-used, 0), nil
	}}
	rounds := 0
	for {
		rounds++
		budget += 4 << 10 // the "forwarder" drains 4 KiB per round
		r, err := Sync(Codex, o)
		if err != nil {
			t.Fatalf("round %d: %v", rounds, err)
		}
		if !r.RetentionLimited {
			break
		}
		if r.SessionsPending == 0 {
			t.Fatalf("limited without pending sessions: %+v", r)
		}
		if rounds > 200 {
			t.Fatal("no progress")
		}
	}
	if rounds < 3 {
		t.Fatalf("the room limit never stopped a sweep (%d rounds)", rounds)
	}
	got := eventIDs(t, logPath)
	seen := map[string]bool{}
	for _, id := range got {
		seen[id] = true
	}
	for _, id := range want {
		if !seen[id] {
			t.Fatalf("event %s was never written (have %d of %d)", id, len(seen), len(want))
		}
	}
	if len(seen) != len(want) {
		t.Fatalf("wrote %d distinct events, want %d", len(seen), len(want))
	}

	// No room at all: nothing is written and nothing is marked collected.
	home, logPath = mk()
	o = Options{Home: home, LogPath: logPath, UserMode: true, Room: func() (int64, error) { return 0, nil }}
	r, err = Sync(Codex, o)
	if err != nil || !r.RetentionLimited || r.Events != 0 || r.SessionsPending != 3 {
		t.Fatalf("no room: %v %+v", err, r)
	}
	o.Room = nil
	if r, err = Sync(Codex, o); err != nil || r.Events != len(want) {
		t.Fatalf("after no room: %v %+v (want %d events)", err, r, len(want))
	}
}
