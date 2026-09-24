package learning

// brainsrv_live_test.go runs PLAN §5 acceptance steps 4, 6 and 8 against a
// real brainsrv (afferent). It is skipped unless AFFERENT_LIVE_BRAINSRV_URL
// is set, so `go test ./...` never touches the network.
//
//	AFFERENT_LIVE_BRAINSRV_URL          e.g. http://localhost:18077 (required)
//	AFFERENT_LIVE_BRAINSRV_KEY_FILE     0600 file with an spk_ key holding
//	                                    read+write+forget on the scope, or
//	AFFERENT_LIVE_BRAINSRV_ADMIN_TOKEN  bootstrap token: the test provisions
//	                                    its own Context, principal, grant, key
//	AFFERENT_LIVE_BRAINSRV_SCOPE        base scope (default ws.dev.people.m.harness)
//	AFFERENT_LIVE_BRAINSRV_CONTEXT      Context slug when provisioning
//	                                    (default afferent-live-e2e)
//
// Every run uses a fresh repo directory whose label is unique, so runs never
// see each other's memories. The store lives under t.TempDir().

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/brainsrvcfg"
	"github.com/asymptote-labs/agent-beacon/pkg/asymptoteobserve"
)

const (
	envLiveURL        = "AFFERENT_LIVE_BRAINSRV_URL"
	envLiveKeyFile    = "AFFERENT_LIVE_BRAINSRV_KEY_FILE"
	envLiveAdminToken = "AFFERENT_LIVE_BRAINSRV_ADMIN_TOKEN"
	envLiveScope      = "AFFERENT_LIVE_BRAINSRV_SCOPE"
	envLiveContext    = "AFFERENT_LIVE_BRAINSRV_CONTEXT"
)

func liveEnv(name, def string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return def
}

// liveHTTP sends one JSON request to brainsrv and decodes a JSON response.
func liveHTTP(t *testing.T, method, url, bearer string, headers map[string]string, in, out interface{}) int {
	t.Helper()
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			t.Fatal(err)
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if out != nil && resp.StatusCode < 300 {
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("%s %s: decode %q: %v", method, url, raw, err)
		}
	}
	if resp.StatusCode >= 300 {
		t.Logf("%s %s → %d %s", method, url, resp.StatusCode, raw)
	}
	return resp.StatusCode
}

// liveKeyFile returns a key file for scope: the one given, or one minted with
// the admin token (Context create tolerates an existing slug).
func liveKeyFile(t *testing.T, base, scope string) string {
	t.Helper()
	if path := liveEnv(envLiveKeyFile, ""); path != "" {
		return path
	}
	admin := liveEnv(envLiveAdminToken, "")
	if admin == "" {
		t.Skipf("%s is set but neither %s nor %s is", envLiveURL, envLiveKeyFile, envLiveAdminToken)
	}
	slug := liveEnv(envLiveContext, "afferent-live-e2e")
	if code := liveHTTP(t, http.MethodPost, base+"/v1/admin/contexts", admin, nil,
		map[string]string{"slug": slug, "isolation": "schema"}, nil); code != http.StatusCreated && code != http.StatusBadRequest && code != http.StatusConflict {
		t.Fatalf("create context: HTTP %d", code)
	}
	var principal struct {
		ID string `json:"id"`
	}
	if code := liveHTTP(t, http.MethodPost, base+"/v1/admin/principals", admin, nil,
		map[string]string{"context_slug": slug, "kind": "user", "display": "afferent-live-test"}, &principal); code != http.StatusCreated {
		t.Fatalf("create principal: HTTP %d", code)
	}
	if code := liveHTTP(t, http.MethodPost, base+"/v1/admin/grants", admin, nil, map[string]interface{}{
		"context_slug": slug, "principal_id": principal.ID, "scope_path": scope, "verbs": []string{"read", "write", "forget"},
	}, nil); code != http.StatusCreated {
		t.Fatalf("create grant: HTTP %d", code)
	}
	var key struct {
		Secret string `json:"secret"`
	}
	if code := liveHTTP(t, http.MethodPost, base+"/v1/admin/api-keys", admin, nil,
		map[string]string{"principal_id": principal.ID, "narrow_scope": scope}, &key); code != http.StatusCreated {
		t.Fatalf("mint key: HTTP %d", code)
	}
	path := filepath.Join(t.TempDir(), "beacon.key")
	if err := os.WriteFile(path, []byte(key.Secret), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// deadLoopbackURL returns an http://localhost URL nothing listens on.
func deadLoopbackURL(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return fmt.Sprintf("http://localhost:%d", port)
}

func liveCandidate(t *testing.T, store *Store, project asymptoteobserve.LearningProjectV1, title, body string) asymptoteobserve.LearningCandidateV1 {
	t.Helper()
	c := asymptoteobserve.LearningCandidateV1{
		SchemaVersion: asymptoteobserve.LearningSchemaVersion,
		State:         asymptoteobserve.LearningCandidateStateCandidate,
		Kind:          asymptoteobserve.LearningMemoryKindGotcha,
		Title:         title,
		Body:          body,
		Applicability: "when running the payments test suite",
		Tags:          []string{"afferent-live"},
		Project:       project,
		Evidence:      []asymptoteobserve.LearningEvidenceV1{{TraceID: "trace-" + title, Summary: "claude_code session"}},
	}
	c.ID = CandidateID(c)
	if err := store.PutCandidate(c); err != nil {
		t.Fatal(err)
	}
	return c
}

// liveRecallNames recalls query at scope directly and returns the distinct
// beacon.memory entity names, optionally as of validAt.
func liveRecallNames(t *testing.T, base, key, scope, query, validAt string) map[string]bool {
	t.Helper()
	in := map[string]interface{}{"query": query, "k": 50, "mode": "memories"}
	if validAt != "" {
		in["filters"] = map[string]string{"valid_at": validAt}
	}
	var out struct {
		Results []BrainsrvRecallHit `json:"results"`
	}
	if code := liveHTTP(t, http.MethodPost, base+"/v1/recall", key, map[string]string{"X-Scope": scope}, in, &out); code != http.StatusOK {
		t.Fatalf("recall: HTTP %d", code)
	}
	names := map[string]bool{}
	for _, h := range out.Results {
		if h.EntityType == BrainsrvMemoryEntityType {
			names[h.EntityName] = true
		}
	}
	return names
}

func TestLiveBrainsrvAcceptance(t *testing.T) {
	base := strings.TrimRight(liveEnv(envLiveURL, ""), "/")
	if base == "" {
		t.Skipf("%s not set; skipping live brainsrv acceptance", envLiveURL)
	}
	scope := liveEnv(envLiveScope, "ws.dev.people.m.harness")
	keyFile := liveKeyFile(t, base, scope)
	key, err := brainsrvcfg.ReadKeyFile(keyFile)
	if err != nil {
		t.Fatalf("key file: %v", err)
	}

	token := fmt.Sprintf("afflive%d", time.Now().UnixNano())
	repo := filepath.Join(t.TempDir(), "live_"+token)
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	project, err := ResolveProject(repo)
	if err != nil {
		t.Fatal(err)
	}
	repoScope := scope + "." + projectLabel(project)
	storePath := filepath.Join(t.TempDir(), "memory.db")

	t.Setenv(brainsrvcfg.EnvBackend, "brainsrv")
	t.Setenv(brainsrvcfg.EnvURL, base)
	t.Setenv(brainsrvcfg.EnvScope, scope)
	t.Setenv(brainsrvcfg.EnvKeyFile, keyFile)
	open := func() *Store {
		s := OpenConfigured(storePath)
		if _, ok := BrainsrvOf(s); !ok {
			t.Fatal("OpenConfigured did not attach the brainsrv backend")
		}
		return s
	}
	search := func() ([]asymptoteobserve.LearningMemoryV1, bool) {
		s := open()
		ms, err := s.ListMemories(Query{ProjectPath: repo, Q: token, Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		degraded, _ := BackendState(s)
		return ms, degraded
	}

	// Step 4: approve → recallable under <base>.<repo label>.
	c1 := liveCandidate(t, Open(storePath), project, "gotcha: payments tests need TZ=UTC "+token,
		"The payments suite asserts on local midnight; export TZ=UTC before go test or the "+token+" ledger cases fail.")
	_, m1, err := ApproveCandidate(open(), c1.ID, "live")
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	backend, _ := BrainsrvOf(open())
	row, ok, err := backend.SyncRow(m1.ID)
	if err != nil || !ok || row.State != SyncStateSynced || row.EntityID == "" {
		t.Fatalf("step 4: sync row = %+v ok=%v err=%v, want synced with an entity id", row, ok, err)
	}
	if !liveRecallNames(t, base, key, repoScope, token, "")[m1.ID] {
		t.Fatalf("step 4: /v1/recall at %s does not return %s", repoScope, m1.ID)
	}
	var entity struct {
		Name  string `json:"name"`
		Type  string `json:"type"`
		State string `json:"state"`
		Scope string `json:"scope"`
	}
	if code := liveHTTP(t, http.MethodGet, base+"/v1/entities/"+row.EntityID, key, map[string]string{"X-Scope": repoScope}, nil, &entity); code != http.StatusOK ||
		entity.Name != m1.ID || entity.Type != BrainsrvMemoryEntityType || entity.Scope != repoScope || entity.State != "active" {
		t.Fatalf("step 4: entity = %+v (HTTP %d)", entity, code)
	}
	if ms, degraded := search(); degraded || len(ms) != 1 || ms[0].ID != m1.ID {
		t.Fatalf("step 4: search = %v degraded=%v, want [%s] from brainsrv", memoryIDs(ms), degraded, m1.ID)
	}
	// The memory comes back from brainsrv alone (GetMissing via
	// /v1/entities) when the local row is gone.
	if db, err := open().db(); err == nil {
		_, _ = db.Exec(`DELETE FROM memories WHERE id = ?`, m1.ID)
		_ = db.Close()
	}
	if ms, degraded := search(); degraded || len(ms) != 1 || ms[0].ID != m1.ID || ms[0].Body != m1.Body {
		t.Fatalf("step 4: search without the local row = %v degraded=%v, want %s mapped from brainsrv", memoryIDs(ms), degraded, m1.ID)
	}
	if err := Open(storePath).PutMemory(m1); err != nil { // restore for step 8
		t.Fatal(err)
	}

	// Step 6: brainsrv down → approval succeeds, 1 pending; sync → 0.
	t.Setenv(brainsrvcfg.EnvURL, deadLoopbackURL(t))
	c2 := liveCandidate(t, Open(storePath), project, "gotcha: payments tests need TZ=UTC and LC_ALL=C "+token,
		"Set TZ=UTC and also LC_ALL=C for the "+token+" ledger cases; locale changes decimal separators.")
	_, m2, err := ApproveCandidate(open(), c2.ID, "live")
	if err != nil {
		t.Fatalf("step 6: approve with brainsrv down failed: %v", err)
	}
	down, _ := BrainsrvOf(open())
	if counts, err := down.SyncCounts(); err != nil || counts[SyncStatePending] != 1 {
		t.Fatalf("step 6: counts while down = %v err=%v, want 1 pending", counts, err)
	}
	if ms, degraded := search(); !degraded || len(ms) != 2 {
		t.Fatalf("step 6: search while down = %v degraded=%v, want both memories from the local fallback", memoryIDs(ms), degraded)
	}
	t.Setenv(brainsrvcfg.EnvURL, base)
	up, _ := BrainsrvOf(open())
	report, err := up.Sync(context.Background(), false)
	if err != nil || report.Synced != 1 || report.Pending+report.Failed != 0 {
		t.Fatalf("step 6: sync = %+v err=%v", report, err)
	}
	if counts, err := up.SyncCounts(); err != nil || counts[SyncStatePending]+counts[SyncStateFailed] != 0 || counts[SyncStateSynced] != 2 {
		t.Fatalf("step 6: counts after sync = %v err=%v, want 2 synced", counts, err)
	}
	if !liveRecallNames(t, base, key, repoScope, token, "")[m2.ID] {
		t.Fatalf("step 6: %s not recallable after sync", m2.ID)
	}

	// Step 8: supersede → search drops the old memory; recall as of before
	// the supersede still has it.
	before := time.Now().UTC().Format(time.RFC3339Nano)
	time.Sleep(1100 * time.Millisecond)
	if _, err := SupersedeCandidate(open(), c1.ID, m2.ID, "live"); err != nil {
		t.Fatalf("step 8: supersede: %v", err)
	}
	row, _, _ = up.SyncRow(m1.ID)
	if row.State != SyncStateSynced {
		t.Fatalf("step 8: old memory sync row = %+v, want synced", row)
	}
	if code := liveHTTP(t, http.MethodGet, base+"/v1/entities/"+row.EntityID, key, map[string]string{"X-Scope": repoScope}, nil, &entity); code != http.StatusOK || entity.State != "superseded" {
		t.Fatalf("step 8: old entity = %+v (HTTP %d), want superseded", entity, code)
	}
	if ms, degraded := search(); degraded || len(ms) != 1 || ms[0].ID != m2.ID {
		t.Fatalf("step 8: search after supersede = %v degraded=%v, want only %s", memoryIDs(ms), degraded, m2.ID)
	}
	if now := liveRecallNames(t, base, key, repoScope, token, ""); now[m1.ID] || !now[m2.ID] {
		t.Fatalf("step 8: present recall = %v, want %s only", now, m2.ID)
	}
	if past := liveRecallNames(t, base, key, repoScope, token, before); !past[m1.ID] {
		t.Fatalf("step 8: recall valid_at=%s = %v, want it to include %s", before, past, m1.ID)
	}
}
