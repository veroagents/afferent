package learning

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/asymptote-labs/agent-beacon/pkg/asymptoteobserve"
)

func testMemory(id string, project asymptoteobserve.LearningProjectV1, title string) asymptoteobserve.LearningMemoryV1 {
	return asymptoteobserve.LearningMemoryV1{
		SchemaVersion: asymptoteobserve.LearningSchemaVersion,
		ID:            id,
		CandidateID:   "cand-" + id,
		Kind:          asymptoteobserve.LearningMemoryKindDebuggingPattern,
		Title:         title,
		Body:          "body of " + id,
		Project:       project,
		CreatedAt:     "2026-09-24T10:00:00Z",
		UpdatedAt:     "2026-09-24T10:00:00Z",
	}
}

func memoryHit(entityID, name, text string) map[string]interface{} {
	return map[string]interface{}{"table": "attributes", "id": "attr-" + text + "-" + name, "text": text, "entity_id": entityID, "entity_type": "beacon.memory", "entity_name": name}
}

// Two projects whose repos share a basename share the recall scope
// <base>.api; a project-scoped search must still return only its own
// memories, as upstream's project_id filter does.
func TestBrainsrvSearchKeepsProjectIsolationUnderSharedLabel(t *testing.T) {
	quietBackendLog(t)
	fake := newFakeBrainsrv(t)
	store := brainsrvStore(filepath.Join(t.TempDir(), "memory.db"), fake.server.URL)
	work := asymptoteobserve.LearningProjectV1{ID: "p-work", Path: "/work/api"}
	personal := asymptoteobserve.LearningProjectV1{ID: "p-personal", Path: "/personal/api"}
	for _, m := range []asymptoteobserve.LearningMemoryV1{
		testMemory("memory_work", work, "flaky tests at work"),
		testMemory("memory_personal", personal, "flaky tests at home"),
	} {
		if err := store.PutMemory(m); err != nil {
			t.Fatal(err)
		}
	}
	fake.recallHits = []map[string]interface{}{
		memoryHit(fake.entities["memory_personal"], "memory_personal", "summary"),
		memoryHit(fake.entities["memory_work"], "memory_work", "summary"),
	}
	fake.Reset()
	got, err := store.ListMemories(Query{ProjectID: "p-work", Q: "flaky"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "memory_work" {
		t.Fatalf("project-scoped search = %v, want only memory_work", memoryIDs(got))
	}
	if calls := fake.Calls(); len(calls) == 0 || calls[0].Scope != testBaseScope+".api" {
		t.Fatalf("recall calls = %#v", calls)
	}
	// Unscoped search still sees both.
	got, err = store.ListMemories(Query{Q: "flaky"})
	if err != nil || len(got) != 2 {
		t.Fatalf("unscoped search = %v, %v", memoryIDs(got), err)
	}
}

// brainsrv ranks attribute rows and returns several per memory; the search
// re-asks with a larger k until it has enough distinct memories, so a full
// page and later pages are not cut short.
func TestBrainsrvSearchOverfetchesPastPerAttributeHits(t *testing.T) {
	quietBackendLog(t)
	fake := newFakeBrainsrv(t)
	store := brainsrvStore(filepath.Join(t.TempDir(), "memory.db"), fake.server.URL)
	project := asymptoteobserve.LearningProjectV1{ID: "project-1", Path: "/repo"}
	var want []string
	for i := 0; i < 30; i++ {
		id := fmt.Sprintf("memory_%02d", i)
		if err := store.PutMemory(testMemory(id, project, "flaky tests "+id)); err != nil {
			t.Fatal(err)
		}
		want = append(want, id)
		for _, attr := range []string{"summary", "title", "body", "applicability", "kind", "tags"} {
			fake.recallHits = append(fake.recallHits, memoryHit(fake.entities[id], id, attr))
		}
	}
	fake.Reset()
	got, err := store.ListMemories(Query{Q: "flaky", Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(memoryIDs(got), ",") != strings.Join(want[:5], ",") {
		t.Fatalf("page 1 = %v, want %v", memoryIDs(got), want[:5])
	}
	got, err = store.ListMemories(Query{Q: "flaky", Limit: 5, Page: 2})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(memoryIDs(got), ",") != strings.Join(want[5:10], ",") {
		t.Fatalf("page 2 = %v, want %v", memoryIDs(got), want[5:10])
	}
	var ks []float64
	for _, c := range fake.Calls() {
		if c.Path == "/v1/recall" {
			ks = append(ks, c.Body["k"].(float64))
		}
	}
	if len(ks) < 3 || ks[len(ks)-1] <= ks[len(ks)-2] {
		t.Fatalf("recall k per round = %v, want a larger k on the re-ask", ks)
	}
}

// Case 1 of the review: the replacement B is already superseded by C when
// A is superseded by B. brainsrv refuses A→B (409); the sync sends A→C.
func TestBrainsrvSupersedeFollowsLocalChainToLiveHead(t *testing.T) {
	quietBackendLog(t)
	fake := newFakeBrainsrv(t)
	store := brainsrvStore(filepath.Join(t.TempDir(), "memory.db"), fake.server.URL)
	candA := seedCandidate(t, store, "eval-a", "trace-a")
	candB := seedCandidate(t, store, "eval-b", "trace-b")
	candC := seedCandidate(t, store, "eval-c", "trace-c")
	_, a, _ := ApproveCandidate(store, candA.ID, "r")
	_, b, _ := ApproveCandidate(store, candB.ID, "r")
	_, c, _ := ApproveCandidate(store, candC.ID, "r")
	if _, err := SupersedeCandidate(store, candB.ID, c.ID, "b→c"); err != nil {
		t.Fatal(err)
	}
	if _, err := SupersedeCandidate(store, candA.ID, b.ID, "a→b"); err != nil {
		t.Fatal(err)
	}
	if got := fake.superBy[fake.entities[a.ID]]; got != fake.entities[c.ID] {
		t.Fatalf("A superseded by %q in brainsrv, want C %q", got, fake.entities[c.ID])
	}
	row, _, _ := backendOf(t, store).SyncRow(a.ID)
	if row.State != SyncStateSynced {
		t.Fatalf("A sync row = %#v", row)
	}
}

// Case 2 of the review: A→B fails transiently (pending), then B→C lands.
// The retry must not resend A→B (409 → failed for good) but A→C.
func TestBrainsrvSupersedeRetryAfterReplacementSuperseded(t *testing.T) {
	quietBackendLog(t)
	fake := newFakeBrainsrv(t)
	dbPath := filepath.Join(t.TempDir(), "memory.db")
	up := brainsrvStore(dbPath, fake.server.URL)
	candA := seedCandidate(t, up, "eval-a", "trace-a")
	candB := seedCandidate(t, up, "eval-b", "trace-b")
	candC := seedCandidate(t, up, "eval-c", "trace-c")
	_, a, _ := ApproveCandidate(up, candA.ID, "r")
	_, b, _ := ApproveCandidate(up, candB.ID, "r")
	_, c, _ := ApproveCandidate(up, candC.ID, "r")

	down := brainsrvStore(dbPath, deadURL(t))
	if _, err := SupersedeCandidate(down, candA.ID, b.ID, "a→b"); err != nil {
		t.Fatal(err)
	}
	if row, _, _ := backendOf(t, down).SyncRow(a.ID); row.State != SyncStatePending {
		t.Fatalf("A after outage = %#v", row)
	}
	if _, err := SupersedeCandidate(up, candB.ID, c.ID, "b→c"); err != nil {
		t.Fatal(err)
	}
	report, err := backendOf(t, up).Sync(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	if report.Synced != 1 || report.Failed+report.Pending != 0 {
		t.Fatalf("sync report = %#v", report)
	}
	if got := fake.superBy[fake.entities[a.ID]]; got != fake.entities[c.ID] {
		t.Fatalf("A superseded by %q, want C %q", got, fake.entities[c.ID])
	}
}

// The replacement was superseded in brainsrv by another machine, which this
// store does not know: the 409 is resolved by following brainsrv's chain.
func TestBrainsrvSupersedeFollowsRemoteChainOnConflict(t *testing.T) {
	quietBackendLog(t)
	fake := newFakeBrainsrv(t)
	store := brainsrvStore(filepath.Join(t.TempDir(), "memory.db"), fake.server.URL)
	candA := seedCandidate(t, store, "eval-a", "trace-a")
	candB := seedCandidate(t, store, "eval-b", "trace-b")
	_, a, _ := ApproveCandidate(store, candA.ID, "r")
	_, b, _ := ApproveCandidate(store, candB.ID, "r")
	remote := "99999999-9999-9999-9999-999999999999"
	fake.mu.Lock()
	fake.entities["memory_elsewhere"] = remote
	fake.superBy[fake.entities[b.ID]] = remote
	fake.mu.Unlock()
	if _, err := SupersedeCandidate(store, candA.ID, b.ID, "a→b"); err != nil {
		t.Fatal(err)
	}
	if got := fake.superBy[fake.entities[a.ID]]; got != remote {
		t.Fatalf("A superseded by %q, want the remote head %q", got, remote)
	}
	if row, _, _ := backendOf(t, store).SyncRow(a.ID); row.State != SyncStateSynced {
		t.Fatalf("A sync row = %#v", row)
	}
}

// A replacement accepted through GetMissing (approved on another machine,
// never in local SQLite) is resolved in brainsrv; the supersede syncs
// instead of sitting in pending forever, and the replacement's remember is
// never re-sent from this machine.
func TestBrainsrvSupersedeByRemoteOnlyReplacement(t *testing.T) {
	quietBackendLog(t)
	fake := newFakeBrainsrv(t)
	store := brainsrvStore(filepath.Join(t.TempDir(), "memory.db"), fake.server.URL)
	candA := seedCandidate(t, store, "eval-a", "trace-a")
	_, a, err := ApproveCandidate(store, candA.ID, "r")
	if err != nil {
		t.Fatal(err)
	}
	remote := "88888888-8888-8888-8888-888888888888"
	fake.mu.Lock()
	fake.entities["memory_remote"] = remote
	fake.recallHits = []map[string]interface{}{memoryHit(remote, "memory_remote", "summary")}
	fake.entityView[remote] = map[string]interface{}{
		"id": remote, "type": "beacon.memory", "name": "memory_remote", "state": "active", "scope": testBaseScope + ".repo",
		"attributes": map[string]interface{}{
			"title":   map[string]interface{}{"value": "Remote", "value_type": "text"},
			"project": map[string]interface{}{"value": map[string]string{"id": "project-1", "path": "/repo"}, "value_type": "json"},
		},
	}
	fake.mu.Unlock()
	fake.Reset()
	if _, err := SupersedeCandidate(store, candA.ID, "memory_remote", "replaced"); err != nil {
		t.Fatal(err)
	}
	row, _, _ := backendOf(t, store).SyncRow(a.ID)
	if row.State != SyncStateSynced {
		t.Fatalf("A sync row = %#v", row)
	}
	if got := fake.superBy[fake.entities[a.ID]]; got != remote {
		t.Fatalf("A superseded by %q, want %q", got, remote)
	}
	for _, c := range fake.Calls() {
		if c.Idem == "beacon-memory:memory_remote" {
			t.Fatalf("replacement remember re-sent: %#v", c)
		}
	}
}

// A memory brainsrv holds as superseded comes back from GetMissing marked
// superseded, like a local superseded row.
func TestBrainsrvGetMissingReportsSupersededEntity(t *testing.T) {
	quietBackendLog(t)
	fake := newFakeBrainsrv(t)
	store := brainsrvStore(filepath.Join(t.TempDir(), "memory.db"), fake.server.URL)
	oldE, newE := "77777777-7777-7777-7777-777777777771", "77777777-7777-7777-7777-777777777772"
	fake.entities["memory_old"], fake.entities["memory_new"] = oldE, newE
	fake.superBy[oldE] = newE
	fake.recallHits = []map[string]interface{}{memoryHit(oldE, "memory_old", "summary")}
	m, ok, err := store.GetMemory("memory_old")
	if err != nil || !ok || m.SupersededBy != "memory_new" {
		t.Fatalf("GetMemory = %#v ok=%v err=%v", m, ok, err)
	}
}

// A memory whose sync is pending is not in brainsrv yet; a successful recall
// must not hide it (no degraded flag would tell anyone).
func TestBrainsrvSearchIncludesUnsyncedLocalMemories(t *testing.T) {
	quietBackendLog(t)
	dbPath := filepath.Join(t.TempDir(), "memory.db")
	down := brainsrvStore(dbPath, deadURL(t))
	candidate := seedCandidate(t, down, "eval-1", "trace-1")
	_, pending, err := ApproveCandidate(down, candidate.ID, "reviewed")
	if err != nil {
		t.Fatal(err)
	}
	fake := newFakeBrainsrv(t)
	up := brainsrvStore(dbPath, fake.server.URL)
	got, err := up.ListMemories(Query{ProjectPath: "/repo", Q: "smoke"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != pending.ID {
		t.Fatalf("search = %v, want the pending memory", memoryIDs(got))
	}
	if degraded, _ := BackendState(up); degraded {
		t.Fatal("successful recall reported degraded")
	}
	// Once synced, brainsrv's ranking is authoritative: a recall that does
	// not return it does not get it merged back in.
	if _, err := backendOf(t, up).Sync(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	if got, err := up.ListMemories(Query{Q: "smoke"}); err != nil || len(got) != 0 {
		t.Fatalf("synced memory merged from local: %v, %v", memoryIDs(got), err)
	}
}

// A 3xx from brainsrv (or a proxy in front of it) is never followed, so the
// bearer key and memory body are not re-sent to the redirect target.
func TestBrainsrvClientRefusesRedirects(t *testing.T) {
	quietBackendLog(t)
	var leaked atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(target.Close)
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(redirector.Close)
	b := NewBrainsrvBackend(filepath.Join(t.TempDir(), "memory.db"), testBrainsrvConfig(redirector.URL), "spk_secret", nil)
	_, _, err := b.Recall(t.Context(), testBaseScope, "anything", 5)
	if err == nil || !strings.Contains(err.Error(), "307") {
		t.Fatalf("err = %v, want the 307 surfaced", err)
	}
	if n := leaked.Load(); n != 0 {
		t.Fatalf("redirect followed %d time(s)", n)
	}
	// A caller-supplied client without a policy gets the same protection.
	b = NewBrainsrvBackend(filepath.Join(t.TempDir(), "memory.db"), testBrainsrvConfig(redirector.URL), "spk_secret", &http.Client{})
	if _, _, err := b.Recall(t.Context(), testBaseScope, "anything", 5); err == nil || leaked.Load() != 0 {
		t.Fatalf("custom client: err=%v leaked=%d", err, leaked.Load())
	}
}

func memoryIDs(ms []asymptoteobserve.LearningMemoryV1) []string {
	ids := make([]string, 0, len(ms))
	for _, m := range ms {
		ids = append(ids, m.ID)
	}
	return ids
}

// A fresh machine's store has never seen the repository, but the MCP server
// runs inside it: memories approved elsewhere must still be found. A project
// ID that is not the process's own repository still derives no scope.
func TestBrainsrvSearchOnFreshMachineUsesCurrentRepository(t *testing.T) {
	quietBackendLog(t)
	fake := newFakeBrainsrv(t)
	repo := asymptoteobserve.LearningProjectV1{ID: "p-x", Path: "/src/x", RemoteURL: "git@github.com:acme/x.git"}
	machineA := brainsrvStore(filepath.Join(t.TempDir(), "memory.db"), fake.server.URL)
	if err := machineA.PutMemory(testMemory("memory_x", repo, "flaky tests in x")); err != nil {
		t.Fatal(err)
	}
	eid := fake.entities["memory_x"]
	fake.recallHits = []map[string]interface{}{memoryHit(eid, "memory_x", "summary")}
	fake.entityView[eid] = map[string]interface{}{
		"id": eid, "type": "beacon.memory", "name": "memory_x", "state": "active", "scope": testBaseScope + ".x",
		"attributes": map[string]interface{}{
			"title":   map[string]interface{}{"value": "flaky tests in x", "value_type": "text", "valid_from": "2026-09-20T00:00:00Z", "confidence": 1},
			"project": map[string]interface{}{"value": repo, "value_type": "json", "valid_from": "2026-09-20T00:00:00Z", "confidence": 1},
		},
	}

	machineB := brainsrvStore(filepath.Join(t.TempDir(), "memory.db"), fake.server.URL)
	prev := resolveCurrentProject
	t.Cleanup(func() { resolveCurrentProject = prev })

	resolveCurrentProject = func() (asymptoteobserve.LearningProjectV1, error) {
		return asymptoteobserve.LearningProjectV1{ID: "p-other", Path: "/src/other"}, nil
	}
	fake.Reset()
	got, err := machineB.ListMemories(Query{ProjectID: "p-x", Q: "flaky"})
	if err != nil || len(got) != 0 || len(fake.Calls()) != 0 {
		t.Fatalf("unknown non-current project: got %v err %v calls %d, want no recall", memoryIDs(got), err, len(fake.Calls()))
	}

	resolveCurrentProject = func() (asymptoteobserve.LearningProjectV1, error) { return repo, nil }
	fake.Reset()
	got, err = machineB.ListMemories(Query{ProjectID: "p-x", Q: "flaky"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "memory_x" {
		t.Fatalf("fresh machine search = %v, want memory_x", memoryIDs(got))
	}
	if calls := fake.Calls(); len(calls) == 0 || calls[0].Scope != testBaseScope+".x" {
		t.Fatalf("recall calls = %#v, want scope %s.x", calls, testBaseScope)
	}
}
