package learning

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/brainsrvcfg"
	"github.com/asymptote-labs/agent-beacon/pkg/asymptoteobserve"
)

var updateGolden = flag.Bool("update", false, "rewrite golden files")

const testBaseScope = "ws.dev.people.m.harness"

type fakeCall struct {
	Method string
	Path   string
	Scope  string
	Idem   string
	Auth   string
	Body   map[string]interface{}
}

// fakeBrainsrv is a minimal in-memory brainsrv for the memory routes.
type fakeBrainsrv struct {
	t      *testing.T
	server *httptest.Server

	mu         sync.Mutex
	calls      []fakeCall
	entities   map[string]string // entity name → id
	replays    map[string]string // idempotency key → entity id
	recallHits []map[string]interface{}
	entityView map[string]interface{} // id → GET /v1/entities/{id} body
	recallCode int
	superBy    map[string]string // entity id → superseding entity id
}

func newFakeBrainsrv(t *testing.T) *fakeBrainsrv {
	f := &fakeBrainsrv{t: t, entities: map[string]string{}, replays: map[string]string{}, entityView: map[string]interface{}{}, superBy: map[string]string{}}
	f.server = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeBrainsrv) handle(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	call := fakeCall{Method: r.Method, Path: r.URL.Path, Scope: r.Header.Get("X-Scope"), Idem: r.Header.Get("Idempotency-Key"), Auth: r.Header.Get("Authorization")}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &call.Body); err != nil {
			f.t.Errorf("bad json body on %s: %v", r.URL.Path, err)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/remember":
		if call.Idem == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if _, hasText := call.Body["text"]; hasText {
			w.WriteHeader(http.StatusNotImplemented)
			return
		}
		if id, ok := f.replays[call.Idem]; ok {
			fmt.Fprintf(w, `{"resolved":{"m":%q},"replayed":true}`, id)
			return
		}
		facts := call.Body["facts"].(map[string]interface{})
		ent := facts["entities"].([]interface{})[0].(map[string]interface{})
		name := ent["name"].(string)
		id, ok := f.entities[name]
		if !ok {
			id = fmt.Sprintf("00000000-0000-0000-0000-%012d", len(f.entities)+1)
			f.entities[name] = id
		}
		f.replays[call.Idem] = id
		fmt.Fprintf(w, `{"created":[],"resolved":{"m":%q}}`, id)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/supersede"):
		if call.Idem == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		// brainsrv's SupersedeEntity: 409 when the old entity is superseded
		// by a different entity or the replacement is not active.
		oldID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/entities/"), "/supersede")
		by, _ := call.Body["by"].(string)
		if cur, ok := f.superBy[oldID]; ok && cur != by {
			w.WriteHeader(http.StatusConflict)
			fmt.Fprintf(w, `{"error":"entity %s is already superseded by %s"}`, oldID, cur)
			return
		}
		if _, ok := f.superBy[by]; ok {
			w.WriteHeader(http.StatusConflict)
			fmt.Fprintf(w, `{"error":"replacement entity %s is superseded"}`, by)
			return
		}
		f.superBy[oldID] = by
		fmt.Fprint(w, `{"trace_id":"t"}`)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/recall":
		if f.recallCode != 0 {
			w.WriteHeader(f.recallCode)
			fmt.Fprint(w, `{"error":"boom"}`)
			return
		}
		hits := f.recallHits
		if k, ok := call.Body["k"].(float64); ok && int(k) < len(hits) {
			hits = hits[:int(k)]
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"results": hits})
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/entities/"):
		id := strings.TrimPrefix(r.URL.Path, "/v1/entities/")
		view, ok := f.entityView[id]
		if !ok {
			view, ok = f.synthView(id)
		}
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"error":"entity not found"}`)
			return
		}
		_ = json.NewEncoder(w).Encode(view)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/ingest/beacon/health":
		fmt.Fprint(w, `{"ok":true,"endpoints":[]}`)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// synthView renders an entity written through /v1/remember with its
// supersede state. Callers hold f.mu.
func (f *fakeBrainsrv) synthView(id string) (map[string]interface{}, bool) {
	for name, eid := range f.entities {
		if eid != id {
			continue
		}
		view := map[string]interface{}{"id": id, "type": "beacon.memory", "name": name, "state": "active", "scope": testBaseScope + ".repo", "attributes": map[string]interface{}{}}
		if by, ok := f.superBy[id]; ok {
			view["state"], view["superseded_by"] = "superseded", by
		}
		return view, true
	}
	return nil, false
}

func (f *fakeBrainsrv) Calls() []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeCall(nil), f.calls...)
}

func (f *fakeBrainsrv) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
}

func testBrainsrvConfig(url string) brainsrvcfg.Config {
	return brainsrvcfg.Config{URL: url, Scope: testBaseScope, KeyFile: "unused"}
}

// brainsrvStore opens the store at dbPath routed through a brainsrv at url,
// the way OpenConfigured does.
func brainsrvStore(dbPath, url string) *Store {
	return withBackend(Open(dbPath), NewBrainsrvBackend(dbPath, testBrainsrvConfig(url), "spk_test", nil))
}

func quietBackendLog(t *testing.T) *bytes.Buffer {
	var buf bytes.Buffer
	prev := backendLog
	backendLog = &buf
	t.Cleanup(func() { backendLog = prev })
	return &buf
}

// deadURL returns a loopback URL nothing listens on.
func deadURL(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return "http://" + addr
}

func seedCandidate(t *testing.T, store *Store, evalID, traceID string) asymptoteobserve.LearningCandidateV1 {
	t.Helper()
	eval := testLearningEvaluationWithID(evalID, traceID)
	eval.RubricHash = RubricHash()
	if err := store.PutEvaluation(eval); err != nil {
		t.Fatal(err)
	}
	candidate, ok := CandidateFromEvaluation(eval)
	if !ok {
		t.Fatal("candidate not created")
	}
	if err := store.PutCandidate(candidate); err != nil {
		t.Fatal(err)
	}
	return candidate
}

func backendOf(t *testing.T, store *Store) *BrainsrvBackend {
	t.Helper()
	b, ok := BrainsrvOf(store)
	if !ok {
		t.Fatal("store has no brainsrv backend")
	}
	return b
}

func TestBrainsrvRememberBodyGolden(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "memory.db")
	store := Open(dbPath)
	candidate := seedCandidate(t, store, "eval-golden", "trace-golden")
	memory := asymptoteobserve.LearningMemoryV1{
		SchemaVersion: asymptoteobserve.LearningSchemaVersion,
		ID:            "memory_golden",
		CandidateID:   candidate.ID,
		Kind:          asymptoteobserve.LearningMemoryKindDebuggingPattern,
		Title:         "Run the package smoke twice",
		Body:          "The first run warms the cache.",
		Applicability: "package smoke is flaky",
		Tags:          []string{"smoke", "ci"},
		Project:       asymptoteobserve.LearningProjectV1{ID: "project-1", Path: "/repo", RemoteURL: "git@github.com:veroagents/afferent.git"},
		Evidence:      []asymptoteobserve.LearningEvidenceV1{{TraceID: "trace-golden", EventIDs: []string{"event-1"}}},
		CreatedAt:     "2026-09-24T10:11:12.5Z",
		UpdatedAt:     "2026-09-24T10:11:12.5Z",
		SupersededBy:  "memory_other", // never part of the body
	}
	b := NewBrainsrvBackend(dbPath, testBrainsrvConfig("https://brain.example"), "spk_test", nil)
	got, err := json.MarshalIndent(b.rememberBody(memory), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	golden := filepath.Join("testdata", "brainsrv_remember_body.golden.json")
	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("%v (run with -update to create)", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("remember body differs from %s:\n%s", golden, got)
	}
	if !strings.Contains(string(got), RubricHash()) {
		t.Fatal("rubric hash of the source evaluation missing")
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(got, &top); err != nil {
		t.Fatal(err)
	}
	if _, hasText := top["text"]; hasText || len(top) != 1 {
		t.Fatalf("remember body must be facts only (brainsrv answers 501 to text): keys %v", top)
	}
	if got := b.memoryScope(memory); got != testBaseScope+".afferent" {
		t.Fatalf("memory scope = %q", got)
	}
}

func TestBrainsrvApproveSendsExactlyOneRemember(t *testing.T) {
	quietBackendLog(t)
	fake := newFakeBrainsrv(t)
	store := brainsrvStore(filepath.Join(t.TempDir(), "memory.db"), fake.server.URL)
	candidate := seedCandidate(t, store, "eval-1", "trace-1")

	_, memory, err := ApproveCandidate(store, candidate.ID, "reviewed")
	if err != nil {
		t.Fatalf("ApproveCandidate: %v", err)
	}
	calls := fake.Calls()
	if len(calls) != 1 {
		t.Fatalf("calls = %#v, want exactly one remember", calls)
	}
	c := calls[0]
	if c.Method != http.MethodPost || c.Path != "/v1/remember" {
		t.Fatalf("call = %s %s", c.Method, c.Path)
	}
	if c.Idem != "beacon-memory:"+memory.ID {
		t.Fatalf("Idempotency-Key = %q", c.Idem)
	}
	if c.Scope != testBaseScope+".repo" {
		t.Fatalf("X-Scope = %q", c.Scope)
	}
	if c.Auth != "Bearer spk_test" {
		t.Fatalf("Authorization = %q", c.Auth)
	}
	facts := c.Body["facts"].(map[string]interface{})
	if facts["trust"] != 0.9 || facts["valid_from"] == nil {
		t.Fatalf("facts = %#v", facts)
	}
	row, ok, err := backendOf(t, store).SyncRow(memory.ID)
	if err != nil || !ok || row.State != SyncStateSynced || row.EntityID == "" {
		t.Fatalf("sync row = %#v ok=%v err=%v", row, ok, err)
	}
}

func TestBrainsrvDownApprovalSucceedsAndSyncFlushes(t *testing.T) {
	logs := quietBackendLog(t)
	dbPath := filepath.Join(t.TempDir(), "memory.db")
	down := brainsrvStore(dbPath, deadURL(t))
	candidate := seedCandidate(t, down, "eval-1", "trace-1")

	approved, memory, err := ApproveCandidate(down, candidate.ID, "reviewed")
	if err != nil {
		t.Fatalf("approval must succeed while brainsrv is down: %v", err)
	}
	if approved.State != asymptoteobserve.LearningCandidateStateApproved {
		t.Fatalf("candidate = %#v", approved)
	}
	if _, ok, err := Open(dbPath).GetMemory(memory.ID); err != nil || !ok {
		t.Fatalf("memory not stored locally: ok=%v err=%v", ok, err)
	}
	row, ok, err := backendOf(t, down).SyncRow(memory.ID)
	if err != nil || !ok || row.State != SyncStatePending || row.LastError == "" {
		t.Fatalf("sync row = %#v ok=%v err=%v", row, ok, err)
	}
	if !strings.Contains(logs.String(), memory.ID) {
		t.Fatalf("backend failure not logged: %q", logs.String())
	}

	fake := newFakeBrainsrv(t)
	up := backendOf(t, brainsrvStore(dbPath, fake.server.URL))
	report, err := up.Sync(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	if report.Attempted != 1 || report.Synced != 1 || report.Pending+report.Failed != 0 {
		t.Fatalf("report = %#v", report)
	}
	calls := fake.Calls()
	if len(calls) != 1 || calls[0].Path != "/v1/remember" || calls[0].Idem != "beacon-memory:"+memory.ID {
		t.Fatalf("calls = %#v", calls)
	}
	counts, err := up.SyncCounts()
	if err != nil {
		t.Fatal(err)
	}
	if counts[SyncStatePending] != 0 || counts[SyncStateSynced] != 1 {
		t.Fatalf("counts = %#v", counts)
	}

	// Nothing left: a second sync is a no-op, --all re-sends every memory.
	fake.Reset()
	if report, err := up.Sync(t.Context(), false); err != nil || report.Attempted != 0 {
		t.Fatalf("second sync = %#v, %v", report, err)
	}
	if report, err := up.Sync(t.Context(), true); err != nil || report.Synced != 1 {
		t.Fatalf("sync --all = %#v, %v", report, err)
	}
	if calls := fake.Calls(); len(calls) != 1 || calls[0].Path != "/v1/remember" {
		t.Fatalf("sync --all calls = %#v", calls)
	}
}

func TestBrainsrvSupersedeRemembersReplacementThenSupersedes(t *testing.T) {
	quietBackendLog(t)
	fake := newFakeBrainsrv(t)
	store := brainsrvStore(filepath.Join(t.TempDir(), "memory.db"), fake.server.URL)
	oldCandidate := seedCandidate(t, store, "eval-old", "trace-old")
	_, oldMemory, err := ApproveCandidate(store, oldCandidate.ID, "reviewed")
	if err != nil {
		t.Fatal(err)
	}
	fake.Reset()

	newCandidate := seedCandidate(t, store, "eval-new", "trace-new")
	_, newMemory, err := ApproveCandidate(store, newCandidate.ID, "reviewed")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := SupersedeCandidate(store, oldCandidate.ID, newMemory.ID, "replaced"); err != nil {
		t.Fatalf("SupersedeCandidate: %v", err)
	}

	calls := fake.Calls()
	if len(calls) != 2 {
		t.Fatalf("calls = %#v, want remember then supersede", calls)
	}
	oldEntity, newEntity := fake.entities[oldMemory.ID], fake.entities[newMemory.ID]
	if calls[0].Path != "/v1/remember" || calls[0].Idem != "beacon-memory:"+newMemory.ID {
		t.Fatalf("first call = %#v, want the replacement's remember", calls[0])
	}
	sup := calls[1]
	if sup.Method != http.MethodPost || sup.Path != "/v1/entities/"+oldEntity+"/supersede" {
		t.Fatalf("second call = %s %s", sup.Method, sup.Path)
	}
	if sup.Idem != "beacon-supersede:"+oldEntity+":"+newEntity {
		t.Fatalf("supersede Idempotency-Key = %q", sup.Idem)
	}
	if sup.Body["by"] != newEntity || sup.Body["valid_from"] == nil || sup.Scope != testBaseScope+".repo" {
		t.Fatalf("supersede call = %#v", sup)
	}
	row, _, _ := backendOf(t, store).SyncRow(oldMemory.ID)
	if row.State != SyncStateSynced {
		t.Fatalf("old memory sync row = %#v", row)
	}
}

// A memory approved before the backend was configured has no cached entity
// id: supersede recovers it with an idempotent remember, then supersedes.
func TestBrainsrvSupersedeRecoversEntityIDWithoutSyncRow(t *testing.T) {
	quietBackendLog(t)
	fake := newFakeBrainsrv(t)
	dbPath := filepath.Join(t.TempDir(), "memory.db")
	plain := Open(dbPath)
	oldCandidate := seedCandidate(t, plain, "eval-old", "trace-old")
	_, oldMemory, err := ApproveCandidate(plain, oldCandidate.ID, "before backend")
	if err != nil {
		t.Fatal(err)
	}
	store := brainsrvStore(dbPath, fake.server.URL)
	newCandidate := seedCandidate(t, store, "eval-new", "trace-new")
	_, newMemory, err := ApproveCandidate(store, newCandidate.ID, "reviewed")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := SupersedeCandidate(store, oldCandidate.ID, newMemory.ID, "replaced"); err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, c := range fake.Calls() {
		paths = append(paths, c.Path+" "+c.Idem)
	}
	oldEntity := fake.entities[oldMemory.ID]
	want := []string{
		"/v1/remember beacon-memory:" + newMemory.ID,
		"/v1/remember beacon-memory:" + oldMemory.ID,
		"/v1/entities/" + oldEntity + "/supersede beacon-supersede:" + oldEntity + ":" + fake.entities[newMemory.ID],
	}
	if strings.Join(paths, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls:\n%s\nwant:\n%s", strings.Join(paths, "\n"), strings.Join(want, "\n"))
	}
}

func TestBrainsrvSearchMapsRecallHits(t *testing.T) {
	quietBackendLog(t)
	fake := newFakeBrainsrv(t)
	store := brainsrvStore(filepath.Join(t.TempDir(), "memory.db"), fake.server.URL)
	candidate := seedCandidate(t, store, "eval-1", "trace-1")
	_, local, err := ApproveCandidate(store, candidate.ID, "reviewed")
	if err != nil {
		t.Fatal(err)
	}
	localEntity := fake.entities[local.ID]
	remoteEntity := "11111111-1111-1111-1111-111111111111"
	fake.recallHits = []map[string]interface{}{
		{"table": "attributes", "id": "a1", "text": "remote summary", "entity_id": remoteEntity, "entity_type": "beacon.memory", "entity_name": "memory_remote"},
		{"table": "turns", "id": "t1", "text": "we fixed the smoke by running it twice", "known_at": "2026-09-24T10:00:00Z", "src_kind": "turn"},
		{"table": "attributes", "id": "a2", "text": "local summary", "entity_id": localEntity, "entity_type": "beacon.memory", "entity_name": local.ID},
		{"table": "attributes", "id": "a3", "text": "local title", "entity_id": localEntity, "entity_type": "beacon.memory", "entity_name": local.ID},
		{"table": "attributes", "id": "a4", "text": "gone", "entity_id": "22222222-2222-2222-2222-222222222222", "entity_type": "beacon.memory", "entity_name": "memory_gone"},
	}
	fake.entityView[remoteEntity] = map[string]interface{}{
		"id": remoteEntity, "type": "beacon.memory", "name": "memory_remote", "state": "active", "scope": testBaseScope + ".repo",
		"attributes": map[string]interface{}{
			"title":   map[string]interface{}{"value": "Remote lesson", "value_type": "text", "valid_from": "2026-09-20T00:00:00Z", "confidence": 1},
			"body":    map[string]interface{}{"value": "Learned elsewhere.", "value_type": "text", "valid_from": "2026-09-20T00:00:00Z", "confidence": 1},
			"kind":    map[string]interface{}{"value": "gotcha", "value_type": "text", "valid_from": "2026-09-20T00:00:00Z", "confidence": 1},
			"tags":    map[string]interface{}{"value": []string{"x"}, "value_type": "json", "valid_from": "2026-09-20T00:00:00Z", "confidence": 1},
			"project": map[string]interface{}{"value": map[string]string{"id": "project-1", "path": "/repo"}, "value_type": "json", "valid_from": "2026-09-20T00:00:00Z", "confidence": 1},
		},
	}
	fake.Reset()

	got, err := store.ListMemories(Query{ProjectPath: "/repo/sub", Q: "smoke"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "memory_remote" || got[1].ID != local.ID {
		t.Fatalf("memories = %#v", got)
	}
	if got[0].Title != "Remote lesson" || got[0].Kind != "gotcha" || got[0].Project.ID != "project-1" || len(got[0].Tags) != 1 || got[0].CreatedAt == "" {
		t.Fatalf("remote memory mapping = %#v", got[0])
	}
	calls := fake.Calls()
	if calls[0].Path != "/v1/recall" || calls[0].Scope != testBaseScope+".repo" || calls[0].Body["mode"] != "memories" || calls[0].Body["query"] != "smoke" {
		t.Fatalf("recall call = %#v", calls[0])
	}
	degraded, history := BackendState(store)
	if degraded || len(history) != 1 || history[0].Table != "turns" || history[0].Source != "brainsrv-episodic" {
		t.Fatalf("degraded=%v history=%#v", degraded, history)
	}

	// Kind filter applies to backend results too.
	got, err = store.ListMemories(Query{Q: "smoke", Kind: "gotcha"})
	if err != nil || len(got) != 1 || got[0].ID != "memory_remote" {
		t.Fatalf("kind-filtered = %#v, %v", got, err)
	}
	for _, c := range fake.Calls() {
		if c.Path == "/v1/recall" && c.Body["query"] == "smoke" && c.Scope != testBaseScope && c.Scope != testBaseScope+".repo" {
			t.Fatalf("recall scope = %q", c.Scope)
		}
	}
}

func TestBrainsrvSearchNeverDerivesScopeFromUnknownProjectPath(t *testing.T) {
	quietBackendLog(t)
	fake := newFakeBrainsrv(t)
	store := brainsrvStore(filepath.Join(t.TempDir(), "memory.db"), fake.server.URL)
	candidate := seedCandidate(t, store, "eval-1", "trace-1")
	if _, _, err := ApproveCandidate(store, candidate.ID, "reviewed"); err != nil {
		t.Fatal(err)
	}
	fake.Reset()

	got, err := store.ListMemories(Query{ProjectPath: "/elsewhere/evil-repo", Q: "smoke"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("unknown project returned %#v", got)
	}
	for _, c := range fake.Calls() {
		if strings.Contains(c.Scope, "evil") || c.Path == "/v1/recall" {
			t.Fatalf("request derived from an unknown project path: %#v", c)
		}
	}
	// No project at all searches the base scope.
	if _, err := store.ListMemories(Query{Q: "smoke"}); err != nil {
		t.Fatal(err)
	}
	calls := fake.Calls()
	if len(calls) != 1 || calls[0].Scope != testBaseScope {
		t.Fatalf("calls = %#v", calls)
	}
}

func TestBrainsrvSearchFailureFallsBackToLocal(t *testing.T) {
	quietBackendLog(t)
	fake := newFakeBrainsrv(t)
	store := brainsrvStore(filepath.Join(t.TempDir(), "memory.db"), fake.server.URL)
	candidate := seedCandidate(t, store, "eval-1", "trace-1")
	_, memory, err := ApproveCandidate(store, candidate.ID, "reviewed")
	if err != nil {
		t.Fatal(err)
	}
	fake.recallCode = http.StatusInternalServerError
	got, err := store.ListMemories(Query{Q: "smoke"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != memory.ID {
		t.Fatalf("fallback = %#v", got)
	}
	if degraded, _ := BackendState(store); !degraded {
		t.Fatal("fallback not marked degraded")
	}
	// Listing without a query never calls the backend.
	fake.Reset()
	if _, err := store.ListMemories(Query{}); err != nil {
		t.Fatal(err)
	}
	if calls := fake.Calls(); len(calls) != 0 {
		t.Fatalf("list without query called brainsrv: %#v", calls)
	}
}

func TestBrainsrvGetMissingReadsEntity(t *testing.T) {
	quietBackendLog(t)
	fake := newFakeBrainsrv(t)
	store := brainsrvStore(filepath.Join(t.TempDir(), "memory.db"), fake.server.URL)
	entity := "33333333-3333-3333-3333-333333333333"
	fake.recallHits = []map[string]interface{}{
		{"table": "attributes", "id": "a1", "text": "x", "entity_id": entity, "entity_type": "beacon.memory", "entity_name": "memory_elsewhere"},
	}
	fake.entityView[entity] = map[string]interface{}{
		"id": entity, "type": "beacon.memory", "name": "memory_elsewhere", "state": "active",
		"attributes": map[string]interface{}{"title": map[string]interface{}{"value": "Elsewhere", "value_type": "text"}},
	}
	m, ok, err := store.GetMemory("memory_elsewhere")
	if err != nil || !ok || m.Title != "Elsewhere" {
		t.Fatalf("GetMemory = %#v ok=%v err=%v", m, ok, err)
	}
	if _, ok, err := store.GetMemory("memory_nowhere"); err != nil || ok {
		t.Fatalf("missing memory: ok=%v err=%v", ok, err)
	}
	// Unreachable backend: a local miss stays a miss, never an error.
	down := brainsrvStore(filepath.Join(t.TempDir(), "memory.db"), deadURL(t))
	if _, ok, err := down.GetMemory("memory_elsewhere"); err != nil || ok {
		t.Fatalf("down: ok=%v err=%v", ok, err)
	}
}

func TestOpenConfigured(t *testing.T) {
	logs := quietBackendLog(t)
	dbPath := filepath.Join(t.TempDir(), "memory.db")
	for _, k := range []string{brainsrvcfg.EnvBackend, brainsrvcfg.EnvURL, brainsrvcfg.EnvScope, brainsrvcfg.EnvKeyFile} {
		t.Setenv(k, "")
	}
	if s := OpenConfigured(dbPath); s.hooks != nil || s.Path() != dbPath {
		t.Fatalf("unset env: %#v", s)
	}
	if logs.Len() != 0 {
		t.Fatalf("unset env logged %q", logs.String())
	}

	t.Setenv(brainsrvcfg.EnvBackend, "brainsrv")
	t.Setenv(brainsrvcfg.EnvURL, "http://brain.example.com")
	t.Setenv(brainsrvcfg.EnvScope, testBaseScope)
	t.Setenv(brainsrvcfg.EnvKeyFile, filepath.Join(t.TempDir(), "missing.key"))
	if s := OpenConfigured(dbPath); s.hooks != nil {
		t.Fatal("invalid config must fall back to plain Open")
	}
	if !strings.Contains(logs.String(), "https") {
		t.Fatalf("invalid config warning = %q", logs.String())
	}

	keyFile := filepath.Join(t.TempDir(), "beacon.key")
	if err := os.WriteFile(keyFile, []byte("spk_live\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(brainsrvcfg.EnvURL, "http://127.0.0.1:1")
	t.Setenv(brainsrvcfg.EnvKeyFile, keyFile)
	s := OpenConfigured(dbPath)
	b, ok := BrainsrvOf(s)
	if !ok || b.key != "spk_live" || b.Config().Scope != testBaseScope {
		t.Fatalf("configured store = %#v", s)
	}
}

// With no backend, the seam changes nothing: PutMemory/ListMemories/GetMemory
// never touch memory_sync and never call out.
func TestNoBackendLeavesStoreUntouched(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "memory.db")
	store := Open(dbPath)
	candidate := seedCandidate(t, store, "eval-1", "trace-1")
	if _, _, err := ApproveCandidate(store, candidate.ID, "reviewed"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ListMemories(Query{Q: "smoke"}); err != nil {
		t.Fatal(err)
	}
	db, err := store.db()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = 'memory_sync'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("memory_sync exists without a backend: n=%d err=%v", n, err)
	}
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != storeSchemaVersion {
		t.Fatalf("user_version = %d, %v", version, err)
	}
}

func TestSyncSchemaLeavesUserVersion(t *testing.T) {
	fake := newFakeBrainsrv(t)
	store := brainsrvStore(filepath.Join(t.TempDir(), "memory.db"), fake.server.URL)
	if _, err := backendOf(t, store).SyncCounts(); err != nil {
		t.Fatal(err)
	}
	db, err := store.db()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != storeSchemaVersion {
		t.Fatalf("user_version = %d, %v", version, err)
	}
}
