package learning

// backend.go is afferent's memory-backend seam (PLAN §4 Phase 2, SPEC B1).
// Store has one hooks field and three one-line call-throughs (PutMemory,
// ListMemories, GetMemory); every other line of the integration lives here
// and in brainsrv.go so upstream's store.go stays rebase-friendly.

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/asymptote-labs/agent-beacon/pkg/asymptoteobserve"
)

// MemoryBackend is where approved memories live canonically (SPEC B1).
type MemoryBackend interface {
	PutMemory(ctx context.Context, m asymptoteobserve.LearningMemoryV1) error
	GetMemory(ctx context.Context, id string) (asymptoteobserve.LearningMemoryV1, bool, error)
	SearchMemories(ctx context.Context, q Query) ([]asymptoteobserve.LearningMemoryV1, error)
}

// StoreHooks is the Store's single extension seam. A nil hooks value (the
// default from Open) leaves Store behaviour exactly as upstream wrote it.
type StoreHooks interface {
	// AfterPut runs after PutMemory committed the local write. It must never
	// fail the caller: backend errors are recorded, not returned.
	AfterPut(m asymptoteobserve.LearningMemoryV1)
	// Search serves ListMemories for a text query. ok=false makes the Store
	// fall back to its local SQLite search.
	Search(q Query) ([]asymptoteobserve.LearningMemoryV1, bool)
	// GetMissing serves GetMemory when the memory is not in local SQLite.
	GetMissing(id string) (asymptoteobserve.LearningMemoryV1, bool, error)
}

// HistoryHit is a non-memory recall hit (a past prompt, response or extracted
// fact) kept for MCP get_memory_context include_history (B2).
type HistoryHit struct {
	Text    string `json:"text"`
	Table   string `json:"table"`
	KnownAt string `json:"known_at,omitempty"`
	SrcKind string `json:"src_kind,omitempty"`
	Source  string `json:"source"`
}

// historySearcher is implemented by backends that also return non-memory
// hits alongside a memory search.
type historySearcher interface {
	searchWithHistory(ctx context.Context, q Query) ([]asymptoteobserve.LearningMemoryV1, []HistoryHit, error)
}

// backendTimeout bounds every backend call made through the hooks (PLAN B-2:
// Store methods take no ctx).
const backendTimeout = 5 * time.Second

// backendLog receives backend warnings. Stores are created per request, so
// stderr (never stdout, which the MCP server speaks JSON-RPC on).
var backendLog io.Writer = os.Stderr

func logBackendf(format string, args ...interface{}) {
	fmt.Fprintf(backendLog, "beacon: memory backend: "+format+"\n", args...)
}

var configWarned sync.Map

// warnConfigOnce reports an unusable backend configuration once per process,
// so a long-lived MCP server does not repeat it on every request.
func warnConfigOnce(err error) {
	msg := err.Error()
	if _, loaded := configWarned.LoadOrStore(msg, true); loaded {
		return
	}
	logBackendf("brainsrv backend disabled, using local memory only: %s", msg)
}

// OpenConfigured opens the store at path (the same argument Open takes) with
// the backend selected by the environment or the afferent sign-in (see
// configuredBackend in brainsrv_afferent.go). With neither it is exactly
// Open(path). A present but invalid configuration logs a warning and also
// returns plain Open(path): a broken backend never breaks the CLI.
func OpenConfigured(path string) *Store {
	store := Open(path)
	if b := configuredBackend(path, os.Getenv); b != nil {
		store.hooks = newBackendHooks(b)
	}
	return store
}

// withBackend returns a copy of the store that routes through backend. Tests
// and callers that build a backend by hand use it; OpenConfigured is the
// production path.
func withBackend(store *Store, backend MemoryBackend) *Store {
	return &Store{dbPath: store.dbPath, hooks: newBackendHooks(backend)}
}

// local returns the same store without hooks, for backend code that must read
// SQLite without recursing into itself.
func (s *Store) local() *Store { return &Store{dbPath: s.dbPath} }

// backendHooks adapts a MemoryBackend to StoreHooks. It carries the result
// state of the last Search so callers (MCP, B2) can report degradation and
// history. Stores are created per request, so this state is per request.
type backendHooks struct {
	backend MemoryBackend

	mu       sync.Mutex
	degraded bool
	history  []HistoryHit
}

func newBackendHooks(backend MemoryBackend) *backendHooks {
	return &backendHooks{backend: backend}
}

func (h *backendHooks) AfterPut(m asymptoteobserve.LearningMemoryV1) {
	ctx, cancel := context.WithTimeout(context.Background(), backendTimeout)
	defer cancel()
	if err := h.backend.PutMemory(ctx, m); err != nil {
		logBackendf("memory %s saved locally; brainsrv sync pending: %v", m.ID, err)
	}
}

func (h *backendHooks) Search(q Query) ([]asymptoteobserve.LearningMemoryV1, bool) {
	if strings.TrimSpace(q.Q) == "" {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), backendTimeout)
	defer cancel()
	var (
		out  []asymptoteobserve.LearningMemoryV1
		hist []HistoryHit
		err  error
	)
	if hs, ok := h.backend.(historySearcher); ok {
		out, hist, err = hs.searchWithHistory(ctx, q)
	} else {
		out, err = h.backend.SearchMemories(ctx, q)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if err != nil {
		logBackendf("search failed, falling back to local memory: %v", err)
		h.degraded = true
		h.history = nil
		return nil, false
	}
	h.degraded = false
	h.history = hist
	if out == nil {
		out = []asymptoteobserve.LearningMemoryV1{}
	}
	return out, true
}

func (h *backendHooks) GetMissing(id string) (asymptoteobserve.LearningMemoryV1, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), backendTimeout)
	defer cancel()
	m, ok, err := h.backend.GetMemory(ctx, id)
	if err != nil {
		// A local miss stays a miss when the backend is unreachable.
		logBackendf("lookup of %s failed: %v", id, err)
		return asymptoteobserve.LearningMemoryV1{}, false, nil
	}
	return m, ok, nil
}

// BackendState reports the outcome of the store's last backend search:
// degraded is true when the backend failed and ListMemories fell back to
// local SQLite; history holds the non-memory hits of a successful search.
// A store without a backend reports (false, nil).
func BackendState(s *Store) (degraded bool, history []HistoryHit) {
	h, ok := s.hooks.(*backendHooks)
	if !ok || h == nil {
		return false, nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.degraded, append([]HistoryHit(nil), h.history...)
}

// HasBackend reports whether the store routes through a memory backend.
func HasBackend(s *Store) bool { return s != nil && s.hooks != nil }

// BrainsrvOf returns the brainsrv backend of a store opened by
// OpenConfigured, if one is configured.
func BrainsrvOf(s *Store) (*BrainsrvBackend, bool) {
	if s == nil {
		return nil, false
	}
	h, ok := s.hooks.(*backendHooks)
	if !ok || h == nil {
		return nil, false
	}
	b, ok := h.backend.(*BrainsrvBackend)
	return b, ok
}
