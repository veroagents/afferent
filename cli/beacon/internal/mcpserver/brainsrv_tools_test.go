package mcpserver

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/brainsrvcfg"
	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/learning"
	"github.com/asymptote-labs/agent-beacon/pkg/asymptoteobserve"
)

const testBrainsrvScope = "ws.dev.people.m.harness"

// fakeRecall is a minimal brainsrv that answers /v1/recall.
type fakeRecall struct {
	mu     sync.Mutex
	hits   []map[string]interface{}
	status int
	calls  []recordedRecall
}

type recordedRecall struct {
	Scope string
	Auth  string
	Body  map[string]interface{}
}

func (f *fakeRecall) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]interface{}
	_ = json.Unmarshal(raw, &body)
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost || r.URL.Path != "/v1/recall" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	f.calls = append(f.calls, recordedRecall{Scope: r.Header.Get("X-Scope"), Auth: r.Header.Get("Authorization"), Body: body})
	if f.status != 0 {
		w.WriteHeader(f.status)
		_, _ = io.WriteString(w, `{"error":"boom"}`)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"results": f.hits})
}

func (f *fakeRecall) Calls() []recordedRecall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedRecall(nil), f.calls...)
}

// clearBrainsrvEnv makes the test independent of the developer's shell.
func clearBrainsrvEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{brainsrvcfg.EnvBackend, brainsrvcfg.EnvURL, brainsrvcfg.EnvScope, brainsrvcfg.EnvKeyFile} {
		t.Setenv(key, "")
	}
}

// useBrainsrv configures the brainsrv backend through the environment, the
// way a real MCP server process gets it, pointing at fake.
func useBrainsrv(t *testing.T, fake http.Handler) {
	t.Helper()
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	keyFile := filepath.Join(t.TempDir(), "brainsrv.key")
	if err := os.WriteFile(keyFile, []byte("spk_test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(brainsrvcfg.EnvBackend, brainsrvcfg.BackendBrainsrv)
	t.Setenv(brainsrvcfg.EnvURL, server.URL)
	t.Setenv(brainsrvcfg.EnvScope, testBrainsrvScope)
	t.Setenv(brainsrvcfg.EnvKeyFile, keyFile)
}

var testProject = asymptoteobserve.LearningProjectV1{ID: "project-1", Path: "/repo"}

// seedMemories writes memories to local SQLite without any backend.
func seedMemories(t *testing.T, logPath string, memories ...asymptoteobserve.LearningMemoryV1) {
	t.Helper()
	store := learning.Open(learning.PathForRuntimeLog(logPath))
	for _, m := range memories {
		if err := store.PutMemory(m); err != nil {
			t.Fatal(err)
		}
	}
}

func testMemory(id, title, updatedAt string) asymptoteobserve.LearningMemoryV1 {
	return asymptoteobserve.LearningMemoryV1{
		ID:          id,
		CandidateID: "candidate-" + id,
		Kind:        asymptoteobserve.LearningMemoryKindGotcha,
		Title:       title,
		Body:        "Retry the package smoke once before changing code.",
		Project:     testProject,
		CreatedAt:   updatedAt,
		UpdatedAt:   updatedAt,
	}
}

func memoryHit(id string) map[string]interface{} {
	return map[string]interface{}{
		"table": "attributes", "id": "attr-" + id, "text": "summary " + id,
		"entity_id": "entity-" + id, "entity_type": learning.BrainsrvMemoryEntityType, "entity_name": id,
	}
}

func callJSON(t *testing.T, server *Server, name string, args map[string]interface{}) (map[string]interface{}, toolResult) {
	t.Helper()
	result, err := server.callTool(t.Context(), callToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s returned error: %v", name, err)
	}
	if result.IsError {
		return nil, result
	}
	var out map[string]interface{}
	if err := json.Unmarshal([]byte(result.Content[0].Text), &out); err != nil {
		t.Fatalf("decode %s: %v (%s)", name, err, result.Content[0].Text)
	}
	return out, result
}

func resultIDs(t *testing.T, out map[string]interface{}, field string) []string {
	t.Helper()
	items, _ := out[field].([]interface{})
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.(map[string]interface{})["id"].(string))
	}
	return ids
}

func TestBrainsrvMemoryToolsPreserveRecallOrder(t *testing.T) {
	clearBrainsrvEnv(t)
	logPath := filepath.Join(t.TempDir(), "endpoint", "logs", "runtime.jsonl")
	older := testMemory("memory-older", "Package smoke retry (older)", "2026-01-01T00:00:00Z")
	newer := testMemory("memory-newer", "Package smoke retry (newer)", "2026-02-01T00:00:00Z")
	seedMemories(t, logPath, older, newer)

	args := map[string]interface{}{"project_id": testProject.ID, "q": "package smoke"}
	local, _ := callJSON(t, New(Options{LogPath: logPath}), "search_memory", args)
	if got := resultIDs(t, local, "memories"); !reflect.DeepEqual(got, []string{newer.ID, older.ID}) {
		t.Fatalf("local order = %v, want newest first", got)
	}

	fake := &fakeRecall{hits: []map[string]interface{}{memoryHit(older.ID), memoryHit(older.ID), memoryHit(newer.ID)}}
	useBrainsrv(t, fake)
	server := New(Options{LogPath: logPath})
	for _, tc := range []struct{ tool, field string }{{"search_memory", "memories"}, {"get_memory_context", "context"}} {
		out, _ := callJSON(t, server, tc.tool, args)
		if got := resultIDs(t, out, tc.field); !reflect.DeepEqual(got, []string{older.ID, newer.ID}) {
			t.Fatalf("%s order = %v, want recall order", tc.tool, got)
		}
		if _, ok := out["degraded"]; ok {
			t.Fatalf("%s reported degraded on a healthy backend: %v", tc.tool, out)
		}
		if _, ok := out["history"]; ok {
			t.Fatalf("%s returned history without include_history: %v", tc.tool, out)
		}
	}
	calls := fake.Calls()
	if len(calls) != 2 || calls[0].Scope != testBrainsrvScope+".repo" || calls[0].Auth != "Bearer spk_test" || calls[0].Body["mode"] != "memories" {
		t.Fatalf("recall calls = %#v", calls)
	}
}

// TestBrainsrvMemoryToolsMatchUpstreamShape pins the replaced handlers to
// upstream's: a failing backend answers exactly what upstream would, plus
// degraded:true.
func TestBrainsrvMemoryToolsMatchUpstreamShape(t *testing.T) {
	clearBrainsrvEnv(t)
	logPath := filepath.Join(t.TempDir(), "endpoint", "logs", "runtime.jsonl")
	seedMemories(t, logPath,
		testMemory("memory-a", "Package smoke retry A", "2026-01-01T00:00:00Z"),
		testMemory("memory-b", "Package smoke retry B", "2026-02-01T00:00:00Z"),
	)
	cases := []struct {
		tool string
		args map[string]interface{}
	}{
		{"search_memory", map[string]interface{}{"project_id": testProject.ID, "q": "smoke", "limit": float64(1)}},
		{"get_memory_context", map[string]interface{}{"project_id": testProject.ID, "task": "package smoke", "limit": float64(9)}},
		{"get_memory_context", map[string]interface{}{"project_id": testProject.ID}},
	}
	upstream := New(Options{LogPath: logPath})
	var want []map[string]interface{}
	for _, tc := range cases {
		out, _ := callJSON(t, upstream, tc.tool, tc.args)
		want = append(want, out)
	}

	fake := &fakeRecall{status: http.StatusServiceUnavailable}
	useBrainsrv(t, fake)
	server := New(Options{LogPath: logPath})
	for i, tc := range cases {
		got, _ := callJSON(t, server, tc.tool, tc.args)
		hasText := tc.args["q"] != nil || tc.args["task"] != nil
		if hasText && got["degraded"] != true {
			t.Fatalf("case %d: degraded = %v, want true (%v)", i, got["degraded"], got)
		}
		if !hasText {
			if _, ok := got["degraded"]; ok {
				t.Fatalf("case %d: listing without a query never calls the backend, got degraded: %v", i, got)
			}
		}
		delete(got, "degraded")
		if !reflect.DeepEqual(got, want[i]) {
			t.Fatalf("case %d: fallback = %v, upstream = %v", i, got, want[i])
		}
	}
	if len(fake.Calls()) != 2 {
		t.Fatalf("recall calls = %d, want 2", len(fake.Calls()))
	}
}

func TestBrainsrvMemoryContextIncludeHistory(t *testing.T) {
	clearBrainsrvEnv(t)
	logPath := filepath.Join(t.TempDir(), "endpoint", "logs", "runtime.jsonl")
	memory := testMemory("memory-1", "Package smoke retry", "2026-01-01T00:00:00Z")
	seedMemories(t, logPath, memory)
	fake := &fakeRecall{hits: []map[string]interface{}{
		{"table": "turns", "id": "turn-1", "text": "user: the package smoke failed with ECONNRESET", "known_at": "2026-09-20T10:00:00Z", "src_kind": "beacon"},
		memoryHit(memory.ID),
		{"table": "attributes", "id": "attr-9", "text": "uses pnpm", "known_at": "2026-09-21T10:00:00Z", "src_kind": "extraction", "entity_id": "e9", "entity_type": "project", "entity_name": "repo"},
	}}
	useBrainsrv(t, fake)
	server := New(Options{LogPath: logPath})

	schema := server.tools["get_memory_context"].InputSchema["properties"].(map[string]interface{})
	if _, ok := schema["include_history"]; !ok {
		t.Fatalf("get_memory_context schema lacks include_history: %v", schema)
	}
	if _, ok := server.tools["search_memory"].InputSchema["properties"].(map[string]interface{})["include_history"]; ok {
		t.Fatal("search_memory must keep upstream's schema")
	}

	args := map[string]interface{}{"project_id": testProject.ID, "task": "package smoke", "include_history": true}
	out, _ := callJSON(t, server, "get_memory_context", args)
	if got := resultIDs(t, out, "context"); !reflect.DeepEqual(got, []string{memory.ID}) {
		t.Fatalf("context = %v", got)
	}
	history, _ := out["history"].([]interface{})
	if len(history) != 2 {
		t.Fatalf("history = %v", out["history"])
	}
	first := history[0].(map[string]interface{})
	want := map[string]interface{}{"text": "user: the package smoke failed with ECONNRESET", "table": "turns", "known_at": "2026-09-20T10:00:00Z", "src_kind": "beacon", "source": "brainsrv-episodic"}
	if !reflect.DeepEqual(first, want) {
		t.Fatalf("history[0] = %v, want %v", first, want)
	}
	if second := history[1].(map[string]interface{}); second["table"] != "attributes" || second["source"] != "brainsrv-episodic" {
		t.Fatalf("history[1] = %v", second)
	}

	delete(args, "include_history")
	out, _ = callJSON(t, server, "get_memory_context", args)
	if _, ok := out["history"]; ok {
		t.Fatalf("history returned by default: %v", out)
	}
}

func TestRecallBrainRegisteredOnlyWithBackend(t *testing.T) {
	clearBrainsrvEnv(t)
	logPath := filepath.Join(t.TempDir(), "runtime.jsonl")
	upstream := New(Options{LogPath: logPath})
	want := "search_activity,summarize_activity,get_activity_event,list_activity_filters,search_memory,get_memory,get_memory_context"
	if got := strings.Join(upstream.ToolNames(), ","); got != want {
		t.Fatalf("ToolNames without backend = %s", got)
	}
	if _, ok := upstream.tools["get_memory_context"].InputSchema["properties"].(map[string]interface{})["include_history"]; ok {
		t.Fatal("include_history must not appear without a backend")
	}

	// A selected but unusable backend (no key file) also leaves upstream's list.
	t.Setenv(brainsrvcfg.EnvBackend, brainsrvcfg.BackendBrainsrv)
	t.Setenv(brainsrvcfg.EnvURL, "https://brainsrv.example")
	t.Setenv(brainsrvcfg.EnvScope, testBrainsrvScope)
	t.Setenv(brainsrvcfg.EnvKeyFile, filepath.Join(t.TempDir(), "missing.key"))
	if got := strings.Join(New(Options{LogPath: logPath}).ToolNames(), ","); got != want {
		t.Fatalf("ToolNames with broken backend config = %s", got)
	}

	useBrainsrv(t, &fakeRecall{})
	server := New(Options{LogPath: logPath})
	if err := server.HasExpectedTools(); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(server.ToolNames(), ","); got != want+","+recallBrainTool {
		t.Fatalf("ToolNames with backend = %s", got)
	}
	tool := server.tools[recallBrainTool]
	if !strings.Contains(strings.ToLower(tool.Description), "experimental") {
		t.Fatalf("recall_brain description not marked experimental: %q", tool.Description)
	}
}

func TestRecallBrainScopes(t *testing.T) {
	clearBrainsrvEnv(t)
	fake := &fakeRecall{hits: []map[string]interface{}{
		{"table": "turns", "id": "turn-1", "text": "assistant: reran the smoke", "known_at": "2026-09-20T10:00:00Z", "src_kind": "beacon"},
		memoryHit("memory-1"),
	}}
	useBrainsrv(t, fake)
	server := New(Options{LogPath: filepath.Join(t.TempDir(), "runtime.jsonl")})

	out, _ := callJSON(t, server, recallBrainTool, map[string]interface{}{"query": "smoke"})
	if out["scope"] != testBrainsrvScope || out["returned"] != float64(2) {
		t.Fatalf("default scope result = %v", out)
	}
	results := out["results"].([]interface{})
	if results[0].(map[string]interface{})["table"] != "turns" || results[1].(map[string]interface{})["entity_name"] != "memory-1" {
		t.Fatalf("results not in recall order: %v", results)
	}

	narrow := testBrainsrvScope + ".repo.claude_code"
	out, _ = callJSON(t, server, recallBrainTool, map[string]interface{}{"query": "smoke", "scope": narrow, "limit": float64(1)})
	if out["scope"] != narrow || out["returned"] != float64(1) {
		t.Fatalf("narrow scope result = %v", out)
	}
	calls := fake.Calls()
	if len(calls) != 2 || calls[0].Scope != testBrainsrvScope || calls[1].Scope != narrow || calls[1].Body["k"] != float64(1) {
		t.Fatalf("recall calls = %#v", calls)
	}

	for _, scope := range []string{
		"ws.dev.people.other.harness",
		"ws.dev.people.m",
		testBrainsrvScope + "x",
		testBrainsrvScope + "..repo",
		testBrainsrvScope + ".Repo",
		"ws",
	} {
		_, result := callJSON(t, server, recallBrainTool, map[string]interface{}{"query": "smoke", "scope": scope})
		if !result.IsError || !strings.Contains(result.Content[0].Text, "must be") {
			t.Fatalf("scope %q not rejected: %#v", scope, result)
		}
	}
	_, result := callJSON(t, server, recallBrainTool, map[string]interface{}{"scope": testBrainsrvScope})
	if !result.IsError {
		t.Fatalf("missing query not rejected: %#v", result)
	}
	if len(fake.Calls()) != 2 {
		t.Fatalf("rejected calls reached brainsrv: %#v", fake.Calls())
	}
}
