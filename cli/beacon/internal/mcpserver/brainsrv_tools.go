package mcpserver

// brainsrv_tools.go is afferent's MCP surface for the brainsrv memory backend
// (PLAN §4 Phase 3, SPEC B2). server.go only gains the result-struct fields
// and one registerBrainsrvTools() call; everything else lives here so
// upstream's file stays rebase-friendly.
//
// Without a configured backend registerBrainsrvTools does nothing: the tool
// list, schemas and handlers are exactly upstream's. With one it
//   - replaces the search_memory and get_memory_context handlers with
//     versions that report `degraded` (the backend failed and SQLite answered)
//     and, for get_memory_context, an opt-in `history` of non-memory recall
//     hits (`include_history`); results and limits are otherwise the same
//   - registers the experimental recall_brain tool.
//
// get_memory is left alone: it reads local SQLite first and never degrades.
//
// Upstream drift: the replaced handlers mirror upstream's search_memory and
// get_memory_context handlers. When upstream changes those, mirror the change
// here (TestBrainsrvMemoryToolsMatchUpstreamShape catches result drift).

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/learning"
	"github.com/asymptote-labs/agent-beacon/pkg/asymptoteobserve"
)

const (
	recallBrainTool         = "recall_brain"
	recallBrainDefaultLimit = 8
	recallBrainMaxLimit     = 20
	// recallBrainTimeout bounds one recall_brain call.
	recallBrainTimeout = 10 * time.Second
	// historyTextLimit caps each history hit's text like memory bodies.
	historyTextLimit = 1200
)

// registerBrainsrvTools installs the brainsrv-aware memory tools when a
// backend is configured (BEACON_MEMORY_BACKEND=brainsrv and a readable key).
// It is a no-op otherwise, so HasExpectedTools and upstream's tool list are
// unchanged.
func (s *Server) registerBrainsrvTools() {
	if _, ok := learning.BrainsrvOf(s.memoryStore()); !ok {
		return
	}
	s.replaceTool("search_memory", func(tool Tool) Tool {
		tool.handler = s.brainsrvSearchMemory
		return tool
	})
	s.replaceTool("get_memory_context", func(tool Tool) Tool {
		tool.InputSchema = withIncludeHistory(tool.InputSchema)
		tool.handler = s.brainsrvMemoryContext
		return tool
	})
	s.register(Tool{
		Name: recallBrainTool,
		Description: "Experimental: recall from the brainsrv memory backend across approved memories and captured agent history, " +
			"ranked by brainsrv. scope defaults to your base scope; a narrower scope must sit under it.",
		InputSchema: objectSchema(map[string]interface{}{
			"query": map[string]interface{}{"type": "string", "description": "What to recall."},
			"scope": map[string]interface{}{"type": "string", "description": "brainsrv scope at or under your base scope, such as <base>.<repo>. Defaults to the base scope."},
			"limit": map[string]interface{}{"type": "integer", "description": "Maximum number of hits to return (default 8, max 20)."},
		}, []string{"query"}, "Experimental brainsrv recall."),
		handler: s.recallBrain,
	})
}

// replaceTool rewrites a registered tool in place, keeping its list order.
func (s *Server) replaceTool(name string, edit func(Tool) Tool) {
	tool, ok := s.tools[name]
	if !ok {
		return
	}
	s.tools[name] = edit(tool)
}

// withIncludeHistory returns a copy of a memory query schema with the
// optional include_history flag added.
func withIncludeHistory(schema map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(schema))
	for k, v := range schema {
		out[k] = v
	}
	props := map[string]interface{}{}
	if in, ok := schema["properties"].(map[string]interface{}); ok {
		for k, v := range in {
			props[k] = v
		}
	}
	props["include_history"] = map[string]interface{}{
		"type":        "boolean",
		"description": "Also return related past agent activity recalled from brainsrv (default false).",
	}
	out["properties"] = props
	return out
}

// listMemoriesWithState runs ListMemories on a fresh configured store and
// reports whether the backend degraded to SQLite, plus the search's
// non-memory history hits.
func (s *Server) listMemoriesWithState(query learning.Query) ([]asymptoteobserve.LearningMemoryV1, bool, []learning.HistoryHit, error) {
	store := s.memoryStore()
	memories, err := store.ListMemories(query)
	if err != nil {
		return nil, false, nil, err
	}
	degraded, history := learning.BackendState(store)
	return memories, degraded, history, nil
}

// brainsrvSearchMemory is upstream's search_memory handler plus `degraded`.
func (s *Server) brainsrvSearchMemory(ctx context.Context, args map[string]interface{}) (interface{}, error) {
	query, err := s.parseMemoryQuery(args)
	if err != nil {
		return nil, err
	}
	memories, degraded, _, err := s.listMemoriesWithState(query)
	if err != nil {
		return nil, err
	}
	limit := normalizeMemoryLimit(query.Limit)
	if len(memories) > limit {
		memories = memories[:limit]
	}
	return memorySearchResult{Memories: memorySummaries(memories), Returned: len(memories), Limit: limit, Degraded: degraded}, nil
}

// brainsrvMemoryContext is upstream's get_memory_context handler plus
// `degraded` and the opt-in `history`.
func (s *Server) brainsrvMemoryContext(ctx context.Context, args map[string]interface{}) (interface{}, error) {
	query, err := s.parseMemoryQuery(args)
	if err != nil {
		return nil, err
	}
	if query.Limit <= 0 || query.Limit > 5 {
		query.Limit = 5
	}
	memories, degraded, history, err := s.listMemoriesWithState(query)
	if err != nil {
		return nil, err
	}
	if len(memories) > query.Limit {
		memories = memories[:query.Limit]
	}
	result := memoryContextResult{Context: memorySummaries(memories), Returned: len(memories), Limit: query.Limit, Degraded: degraded}
	if boolArg(args, "include_history") {
		result.History = compactHistory(history, query.Limit)
	}
	return result, nil
}

// compactHistory caps the history at limit hits and trims each text.
func compactHistory(history []learning.HistoryHit, limit int) []learning.HistoryHit {
	if len(history) > limit {
		history = history[:limit]
	}
	out := make([]learning.HistoryHit, 0, len(history))
	for _, hit := range history {
		hit.Text = asymptoteobserve.CleanString(hit.Text, historyTextLimit, true)
		out = append(out, hit)
	}
	return out
}

type recallBrainHit struct {
	Table      string  `json:"table"`
	ID         string  `json:"id,omitempty"`
	Text       string  `json:"text"`
	KnownAt    string  `json:"known_at,omitempty"`
	SrcKind    string  `json:"src_kind,omitempty"`
	Category   string  `json:"category,omitempty"`
	Confidence float64 `json:"confidence,omitempty"`
	EntityID   string  `json:"entity_id,omitempty"`
	EntityType string  `json:"entity_type,omitempty"`
	EntityName string  `json:"entity_name,omitempty"`
}

type recallBrainResult struct {
	Scope    string           `json:"scope"`
	Results  []recallBrainHit `json:"results"`
	Returned int              `json:"returned"`
	Limit    int              `json:"limit"`
	// BrainsrvDegraded is brainsrv's own list of retrieval stages that were
	// skipped (for example a missing embedder).
	BrainsrvDegraded []string `json:"brainsrv_degraded,omitempty"`
}

// recallBrain is the experimental recall_brain tool: a raw brainsrv recall at
// the base scope or a scope under it. Anything outside the base scope is
// rejected before any request is sent (brainsrv also enforces the key's
// narrow_scope).
func (s *Server) recallBrain(ctx context.Context, args map[string]interface{}) (interface{}, error) {
	backend, ok := learning.BrainsrvOf(s.memoryStore())
	if !ok {
		return nil, errors.New("brainsrv memory backend is not configured")
	}
	query := stringArg(args, "query")
	if query == "" {
		return nil, errors.New("query is required")
	}
	base := backend.Config().Scope
	scope := stringArg(args, "scope")
	if scope == "" {
		scope = base
	}
	if !backend.Config().Covers(scope) {
		return nil, fmt.Errorf("scope %q must be %q or a scope under it", scope, base)
	}
	limit := intArg(args, "limit")
	if limit <= 0 {
		limit = recallBrainDefaultLimit
	}
	if limit > recallBrainMaxLimit {
		limit = recallBrainMaxLimit
	}
	ctx, cancel := context.WithTimeout(ctx, recallBrainTimeout)
	defer cancel()
	hits, degraded, err := backend.Recall(ctx, scope, query, limit)
	if err != nil {
		return nil, err
	}
	if len(hits) > limit {
		hits = hits[:limit]
	}
	out := make([]recallBrainHit, 0, len(hits))
	for _, hit := range hits {
		out = append(out, recallBrainHit{
			Table:      hit.Table,
			ID:         hit.ID,
			Text:       asymptoteobserve.CleanString(hit.Text, historyTextLimit, true),
			KnownAt:    hit.KnownAt,
			SrcKind:    hit.SrcKind,
			Category:   hit.Category,
			Confidence: hit.Confidence,
			EntityID:   hit.EntityID,
			EntityType: hit.EntityType,
			EntityName: hit.EntityName,
		})
	}
	return recallBrainResult{Scope: scope, Results: out, Returned: len(out), Limit: limit, BrainsrvDegraded: degraded}, nil
}

// boolArg reads a boolean tool argument, accepting JSON booleans and the
// strings "true"/"false".
func boolArg(args map[string]interface{}, key string) bool {
	switch value := args[key].(type) {
	case bool:
		return value
	case string:
		return strings.EqualFold(strings.TrimSpace(value), "true")
	default:
		return false
	}
}
