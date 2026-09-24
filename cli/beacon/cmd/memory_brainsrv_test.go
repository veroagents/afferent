package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"

	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/brainsrvcfg"
)

func TestMemoryBrainsrvCommandsRegistered(t *testing.T) {
	for _, path := range [][]string{{"memory", "brainsrv", "status"}, {"memory", "brainsrv", "sync"}} {
		c, _, err := rootCmd.Find(path)
		if err != nil || c == nil || c.Name() != path[len(path)-1] {
			t.Fatalf("command %v not registered: %v", path, err)
		}
	}
	if memoryBrainsrvSyncCmd.Flags().Lookup("all") == nil {
		t.Fatal("sync --all flag missing")
	}
}

func clearBrainsrvEnv(t *testing.T) {
	for _, k := range []string{brainsrvcfg.EnvBackend, brainsrvcfg.EnvURL, brainsrvcfg.EnvScope, brainsrvcfg.EnvKeyFile} {
		t.Setenv(k, "")
	}
}

func setBrainsrvEnv(t *testing.T, url string) {
	keyFile := filepath.Join(t.TempDir(), "beacon.key")
	if err := os.WriteFile(keyFile, []byte("spk_cmdtest\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(brainsrvcfg.EnvBackend, "brainsrv")
	t.Setenv(brainsrvcfg.EnvURL, url)
	t.Setenv(brainsrvcfg.EnvScope, "ws.dev.people.m.harness")
	t.Setenv(brainsrvcfg.EnvKeyFile, keyFile)
}

func TestMemoryBrainsrvStatusRequiresConfig(t *testing.T) {
	clearBrainsrvEnv(t)
	logPath, _ := writeMemoryCommandFixture(t)
	resetMemoryOpts(t)
	memoryOpts.logPath = logPath
	err := runMemoryBrainsrvStatus(&cobra.Command{}, nil)
	if err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("err = %v", err)
	}
}

// Acceptance step 6 in miniature: approve while brainsrv is down, status
// shows one pending, sync against a live brainsrv flushes it.
func TestMemoryBrainsrvPendingThenSync(t *testing.T) {
	var mu sync.Mutex
	var remembers []string
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer spk_cmdtest" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/v1/remember":
			remembers = append(remembers, r.Header.Get("Idempotency-Key"))
			fmt.Fprint(w, `{"resolved":{"m":"00000000-0000-0000-0000-000000000001"}}`)
		case "/v1/ingest/beacon/health":
			if r.URL.Query().Get("scope") != "ws.dev.people.m.harness" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			fmt.Fprint(w, `{"ok":true,"endpoints":[{"hostname":"laptop","last_seen":"2026-09-24T00:00:00Z"}]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer live.Close()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadURL := "http://" + l.Addr().String()
	_ = l.Close()

	logPath, project := writeMemoryCommandFixture(t)
	resetMemoryOpts(t)
	memoryOpts.userMode = true
	memoryOpts.logPath = logPath
	memoryOpts.projectPath = project
	setBrainsrvEnv(t, deadURL)

	candidate := testCommandCandidate(t)
	if err := memoryStore().PutCandidate(candidate); err != nil {
		t.Fatal(err)
	}
	memoryOpts.reason = "reviewed"
	var approveOut bytes.Buffer
	approve := &cobra.Command{}
	approve.SetOut(&approveOut)
	if err := runMemoryCandidatesApprove(approve, []string{candidate.ID}); err != nil {
		t.Fatalf("approve must succeed while brainsrv is down: %v", err)
	}

	memoryOpts.jsonOutput = true
	var statusOut bytes.Buffer
	status := &cobra.Command{}
	status.SetOut(&statusOut)
	if err := runMemoryBrainsrvStatus(status, nil); err == nil {
		t.Fatal("status against a down brainsrv should report the failed health check")
	}
	var st memoryBrainsrvStatusResult
	if err := json.Unmarshal(statusOut.Bytes(), &st); err != nil {
		t.Fatalf("status json: %v\n%s", err, statusOut.String())
	}
	if st.Reachable || st.SyncCounts["pending"] != 1 || st.SyncBacklog != 1 {
		t.Fatalf("status while down = %#v", st)
	}

	t.Setenv(brainsrvcfg.EnvURL, live.URL)
	var syncOut bytes.Buffer
	syncCmd := &cobra.Command{}
	syncCmd.SetOut(&syncOut)
	if err := runMemoryBrainsrvSync(syncCmd, nil); err != nil {
		t.Fatalf("sync: %v\n%s", err, syncOut.String())
	}
	if len(remembers) != 1 || !strings.HasPrefix(remembers[0], "beacon-memory:memory_") {
		t.Fatalf("remembers = %v", remembers)
	}

	statusOut.Reset()
	if err := runMemoryBrainsrvStatus(status, nil); err != nil {
		t.Fatalf("status: %v", err)
	}
	st = memoryBrainsrvStatusResult{}
	if err := json.Unmarshal(statusOut.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if !st.Reachable || st.SyncBacklog != 0 || st.SyncCounts["synced"] != 1 || st.Health == nil || len(st.Health.Endpoints) != 1 {
		t.Fatalf("status after sync = %#v", st)
	}
}
