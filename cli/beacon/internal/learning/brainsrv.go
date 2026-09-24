package learning

// brainsrv.go is the brainsrv memory backend (PLAN §4 Phase 2, SPEC B1).
//
// Writes: an approved memory is written to local SQLite first (upstream
// behaviour, unchanged) and then sent to POST /v1/remember as structured
// facts only (PLAN A-2: brainsrv returns 501 for free text). Searchable
// content rides in a `summary` text attribute, which brainsrv BM25-indexes at
// once and embeds shortly after.
//
// Entity ids: brainsrv answers /v1/remember with the reconcile result, whose
// `resolved` map carries the brainsrv entity id of our local_id "m". The id
// is stored in memory_sync.entity_id. When it is missing (a memory approved
// before the backend was configured, or a lost local row), the backend
// re-sends the memory's own remember: brainsrv replays a recorded
// Idempotency-Key (beacon-memory:<id>) with the original result, and creates
// the entity if it was never written, so the lookup is exact, needs no name
// search and is safe to repeat. Supersede (P-A5) uses those ids.
//
// Failure never fails approval: the local write has already committed, the
// memory_sync row goes to pending (transient) or failed (brainsrv refused it)
// and `beacon memory brainsrv sync` retries it.
//
// Reads: recall hits of entity type beacon.memory map back to memories by
// entity_name (local SQLite first, else GET /v1/entities/{id}). Superseded
// memories are dropped by brainsrv itself; there is deliberately no
// client-side state filter (PLAN A-6).

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/brainsrvcfg"
	"github.com/asymptote-labs/agent-beacon/pkg/asymptoteobserve"
)

const (
	// BrainsrvMemoryEntityType is the brainsrv entity type of a Beacon memory.
	BrainsrvMemoryEntityType = "beacon.memory"
	// brainsrvMemoryLocalID is the local_id of the memory entity in a remember.
	brainsrvMemoryLocalID = "m"
	// brainsrvMemoryTrust is the source trust of a human-reviewed memory.
	brainsrvMemoryTrust = 0.9
	// historySource labels non-memory recall hits (B2).
	historySource = "brainsrv-episodic"

	// memory_sync states.
	SyncStatePending = "pending"
	SyncStateSynced  = "synced"
	SyncStateFailed  = "failed"
)

// BrainsrvBackend is the HTTP client for brainsrv's memory API.
type BrainsrvBackend struct {
	store  *Store // local store without hooks
	cfg    brainsrvcfg.Config
	key    string
	client *http.Client
	// tokens and xContext are set for afferent credentials (an authsrv JWT
	// and its Context) instead of key; see brainsrv_afferent.go.
	tokens   TokenSource
	xContext string
}

// NewBrainsrvBackend returns a backend for the store at storePath. A nil
// client uses a client with a 15s timeout. Redirects are never followed
// (see refuseRedirect) unless the caller's client sets its own policy.
func NewBrainsrvBackend(storePath string, cfg brainsrvcfg.Config, key string, client *http.Client) *BrainsrvBackend {
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	if client.CheckRedirect == nil {
		c := *client
		c.CheckRedirect = refuseRedirect
		client = &c
	}
	return &BrainsrvBackend{store: Open(storePath), cfg: cfg, key: key, client: client}
}

// refuseRedirect stops the client at any 3xx. Go's default policy re-sends
// the Authorization header to a same-host or subdomain target whatever its
// scheme, so a redirect to http:// would leak the spk_ key (and, for 307/308,
// the memory body) in cleartext and defeat the https-only URL rule. The 3xx
// comes back as a BrainsrvError instead.
func refuseRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// Config returns the backend's configuration (the key is not part of it).
func (b *BrainsrvBackend) Config() brainsrvcfg.Config { return b.cfg }

// ---- HTTP ----------------------------------------------------------------

// BrainsrvError is a non-2xx brainsrv response.
type BrainsrvError struct {
	Method string
	Path   string
	Status int
	Body   string
}

func (e *BrainsrvError) Error() string {
	return fmt.Sprintf("brainsrv %s %s: %d %s", e.Method, e.Path, e.Status, strings.TrimSpace(e.Body))
}

// permanent reports a refusal that retrying the same request cannot fix.
func (e *BrainsrvError) permanent() bool {
	return e.Status >= 400 && e.Status < 500 && e.Status != http.StatusRequestTimeout && e.Status != http.StatusTooManyRequests
}

func isPermanent(err error) bool {
	var be *BrainsrvError
	return errors.As(err, &be) && be.permanent()
}

func isNotFound(err error) bool {
	var be *BrainsrvError
	return errors.As(err, &be) && be.Status == http.StatusNotFound
}

func isConflict(err error) bool {
	var be *BrainsrvError
	return errors.As(err, &be) && be.Status == http.StatusConflict
}

func (b *BrainsrvBackend) do(ctx context.Context, method, path, scope, idem string, in, out interface{}) error {
	var payload []byte
	if in != nil {
		var err error
		if payload, err = json.Marshal(in); err != nil {
			return err
		}
	}
	newReq := func() (*http.Request, error) {
		var body io.Reader
		if in != nil {
			body = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, b.cfg.URL+path, body)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")
		if in != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if scope != "" {
			req.Header.Set("X-Scope", scope)
		}
		if idem != "" {
			req.Header.Set("Idempotency-Key", idem)
		}
		return req, nil
	}
	resp, err := b.send(ctx, newReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg := string(data)
		if len(msg) > 512 {
			msg = msg[:512]
		}
		return &BrainsrvError{Method: method, Path: strings.SplitN(path, "?", 2)[0], Status: resp.StatusCode, Body: msg}
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("brainsrv %s %s: decode response: %w", method, path, err)
		}
	}
	return nil
}

// ---- scopes --------------------------------------------------------------

// projectLabel is the repo label of a project: RemoteURL, falling back to
// Path, falling back to ID (SPEC B1), through the shared sanitizer so
// memories co-locate with the forwarder's history for the same repo.
func projectLabel(p asymptoteobserve.LearningProjectV1) string {
	v := firstNonEmpty(p.RemoteURL, p.Path, p.ID)
	if v == "" {
		return brainsrvcfg.NoRepoLabel
	}
	return brainsrvcfg.RepoLabel(v)
}

func (b *BrainsrvBackend) memoryScope(m asymptoteobserve.LearningMemoryV1) string {
	return b.cfg.ScopeFor(projectLabel(m.Project))
}

// searchScope derives the recall scope for q and the project it is limited
// to. ProjectPath is the store's trust boundary: it is resolved only through
// Store.ProjectIDForPath, and only a project this store already holds yields
// a scope. An unknown project yields ok=false (no search), never a scope
// derived from the raw path. No project at all searches the whole base scope
// (projectID "").
//
// The scope alone does not isolate a project: labels are the repo basename
// (PLAN §1), so ~/work/api and ~/personal/api share <base>.api. Callers must
// also drop hits whose Project.ID is not projectID, as upstream's project_id
// filter does.
func (b *BrainsrvBackend) searchScope(q Query) (scope, projectID string, ok bool, err error) {
	if strings.TrimSpace(q.ProjectID) == "" && strings.TrimSpace(q.ProjectPath) == "" {
		return b.cfg.Scope, "", true, nil
	}
	scoped, err := b.store.scopeQuery(q)
	if err != nil {
		return "", "", false, err
	}
	project, found, err := b.projectByID(scoped.ProjectID)
	if err != nil {
		return "", "", false, err
	}
	if !found {
		// A project this store has never seen (a fresh machine) is still
		// searchable when it is the repository this process runs in: that is
		// resolved from disk by the process itself, never from a request path,
		// so the ProjectPath trust boundary holds.
		current, cerr := resolveCurrentProject()
		if cerr != nil || current.ID == "" || current.ID != scoped.ProjectID {
			return "", "", false, nil
		}
		project = current
	}
	return b.cfg.ScopeFor(projectLabel(project)), scoped.ProjectID, true, nil
}

// resolveCurrentProject is the repository this process runs in (the MCP
// server's working directory). A variable so tests can stand in for the disk.
var resolveCurrentProject = func() (asymptoteobserve.LearningProjectV1, error) {
	return ResolveProject("")
}

// projectByID returns the full project record the store holds for id.
func (b *BrainsrvBackend) projectByID(id string) (asymptoteobserve.LearningProjectV1, bool, error) {
	if strings.TrimSpace(id) == "" {
		return asymptoteobserve.LearningProjectV1{}, false, nil
	}
	db, err := b.store.db()
	if err != nil {
		return asymptoteobserve.LearningProjectV1{}, false, err
	}
	defer db.Close()
	var raw sql.NullString
	err = db.QueryRow(`
		SELECT p FROM (
			SELECT 1 AS o, json_extract(memory_json, '$.project') AS p FROM memories WHERE project_id = ?
			UNION ALL
			SELECT 2, json_extract(candidate_json, '$.project') FROM candidates WHERE project_id = ?
			UNION ALL
			SELECT 3, json_extract(evaluation_json, '$.project') FROM evaluations WHERE project_id = ?
		) WHERE p IS NOT NULL ORDER BY o LIMIT 1`, id, id, id).Scan(&raw)
	if err == sql.ErrNoRows {
		return asymptoteobserve.LearningProjectV1{}, false, nil
	}
	if err != nil {
		return asymptoteobserve.LearningProjectV1{}, false, err
	}
	var project asymptoteobserve.LearningProjectV1
	if err := json.Unmarshal([]byte(raw.String), &project); err != nil {
		return asymptoteobserve.LearningProjectV1{}, false, err
	}
	if project.ID == "" {
		project.ID = id
	}
	return project, true, nil
}

// commonScope returns the deepest scope at or above both a and b.
func commonScope(a, b string) string {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	n := 0
	for n < len(as) && n < len(bs) && as[n] == bs[n] {
		n++
	}
	return strings.Join(as[:n], ".")
}

// ---- remember ------------------------------------------------------------

type brainsrvRememberRequest struct {
	Facts brainsrvFacts `json:"facts"`
}

type brainsrvFacts struct {
	Trust      float64             `json:"trust"`
	ValidFrom  string              `json:"valid_from,omitempty"`
	Entities   []brainsrvEntity    `json:"entities"`
	Attributes []brainsrvAttribute `json:"attributes"`
}

type brainsrvEntity struct {
	LocalID string `json:"local_id"`
	Name    string `json:"name"`
	Type    string `json:"type"`
}

type brainsrvAttribute struct {
	Entity    string          `json:"entity"`
	Predicate string          `json:"predicate"`
	Value     json.RawMessage `json:"value"`
	ValueType string          `json:"value_type"`
}

type brainsrvRememberResult struct {
	Resolved map[string]string `json:"resolved"`
	Replayed bool              `json:"replayed"`
}

// memorySummary is the recall carrier: title, body and applicability.
func memorySummary(m asymptoteobserve.LearningMemoryV1) string {
	s := strings.TrimSpace(m.Title) + "\n\n" + strings.TrimSpace(m.Body)
	if a := strings.TrimSpace(m.Applicability); a != "" {
		s += "\n\nApplies when: " + a
	}
	return s
}

// rememberBody maps a memory onto the /v1/remember facts body (PLAN §4
// Phase 2). The body is a pure function of the memory's content (UpdatedAt
// and SupersededBy are not part of it), so re-sending it under the same
// Idempotency-Key is always the same request.
func (b *BrainsrvBackend) rememberBody(m asymptoteobserve.LearningMemoryV1) brainsrvRememberRequest {
	text := func(pred, v string) *brainsrvAttribute {
		if strings.TrimSpace(v) == "" {
			return nil
		}
		raw, _ := json.Marshal(v)
		return &brainsrvAttribute{Entity: brainsrvMemoryLocalID, Predicate: pred, Value: raw, ValueType: "text"}
	}
	jsonAttr := func(pred string, v interface{}) *brainsrvAttribute {
		raw, err := json.Marshal(v)
		if err != nil {
			return nil
		}
		return &brainsrvAttribute{Entity: brainsrvMemoryLocalID, Predicate: pred, Value: raw, ValueType: "json"}
	}
	tags := m.Tags
	if tags == nil {
		tags = []string{}
	}
	evidence := m.Evidence
	if evidence == nil {
		evidence = []asymptoteobserve.LearningEvidenceV1{}
	}
	var attrs []brainsrvAttribute
	for _, a := range []*brainsrvAttribute{
		text("summary", memorySummary(m)),
		text("title", m.Title),
		text("body", m.Body),
		text("kind", m.Kind),
		text("applicability", m.Applicability),
		jsonAttr("tags", tags),
		text("candidate_id", m.CandidateID),
		jsonAttr("evidence", evidence),
		jsonAttr("project", m.Project),
		text("beacon.rubric_hash", b.rubricHashFor(m)),
	} {
		if a != nil {
			attrs = append(attrs, *a)
		}
	}
	facts := brainsrvFacts{
		Trust:      brainsrvMemoryTrust,
		Entities:   []brainsrvEntity{{LocalID: brainsrvMemoryLocalID, Name: m.ID, Type: BrainsrvMemoryEntityType}},
		Attributes: attrs,
	}
	if t, err := time.Parse(time.RFC3339Nano, m.CreatedAt); err == nil {
		facts.ValidFrom = t.UTC().Format(time.RFC3339Nano)
	}
	return brainsrvRememberRequest{Facts: facts}
}

// rubricHashFor returns the rubric hash of the evaluation the memory's
// candidate came from, when the store has it.
func (b *BrainsrvBackend) rubricHashFor(m asymptoteobserve.LearningMemoryV1) string {
	if m.CandidateID == "" {
		return ""
	}
	c, ok, err := b.store.GetCandidate(m.CandidateID)
	if err != nil || !ok || c.SourceEvaluationID == "" {
		return ""
	}
	e, ok, err := b.store.GetEvaluation(c.SourceEvaluationID)
	if err != nil || !ok {
		return ""
	}
	return e.RubricHash
}

func rememberKey(memoryID string) string { return "beacon-memory:" + memoryID }

func supersedeKey(oldEntity, newEntity string) string {
	return "beacon-supersede:" + oldEntity + ":" + newEntity
}

// remember writes m and returns its brainsrv entity id.
func (b *BrainsrvBackend) remember(ctx context.Context, m asymptoteobserve.LearningMemoryV1) (string, error) {
	var res brainsrvRememberResult
	if err := b.do(ctx, http.MethodPost, "/v1/remember", b.memoryScope(m), rememberKey(m.ID), b.rememberBody(m), &res); err != nil {
		return "", err
	}
	id := strings.TrimSpace(res.Resolved[brainsrvMemoryLocalID])
	if id == "" {
		return "", fmt.Errorf("brainsrv remember of %s returned no entity id", m.ID)
	}
	_ = b.setEntityID(m.ID, id)
	return id, nil
}

// entityID returns m's brainsrv entity id: memory_sync first when useCache,
// else (or on a cache miss) through an idempotent remember.
func (b *BrainsrvBackend) entityID(ctx context.Context, m asymptoteobserve.LearningMemoryV1, useCache bool) (string, error) {
	if useCache {
		if row, ok, err := b.syncRow(m.ID); err == nil && ok && row.EntityID != "" {
			return row.EntityID, nil
		}
	}
	return b.remember(ctx, m)
}

func (b *BrainsrvBackend) supersede(ctx context.Context, scope, oldEntity, newEntity, validFrom string) error {
	body := map[string]interface{}{"by": newEntity}
	if t, err := time.Parse(time.RFC3339Nano, validFrom); err == nil {
		body["valid_from"] = t.UTC().Format(time.RFC3339Nano)
	}
	return b.do(ctx, http.MethodPost, "/v1/entities/"+url.PathEscape(oldEntity)+"/supersede", scope, supersedeKey(oldEntity, newEntity), body, nil)
}

// maxSupersedeHops bounds how far a supersede chain is followed, locally and
// in brainsrv, so a corrupt or cyclic chain fails instead of looping.
const maxSupersedeHops = 16

// syncMemory makes brainsrv match the local memory m: m is remembered, and
// when m is superseded the old entity is superseded by the live head of its
// replacement chain (P-A5). useCache reuses entity ids from memory_sync
// instead of re-sending remembers; approval of a fresh memory always sends.
func (b *BrainsrvBackend) syncMemory(ctx context.Context, m asymptoteobserve.LearningMemoryV1, useCache bool) error {
	oldID, err := b.entityID(ctx, m, useCache)
	if err != nil {
		return err
	}
	if strings.TrimSpace(m.SupersededBy) == "" {
		return nil
	}
	newID, replScope, err := b.replacementEntity(ctx, m, useCache)
	if err != nil {
		return err
	}
	scope := commonScope(b.memoryScope(m), replScope)
	if !b.cfg.Covers(scope) {
		scope = b.cfg.Scope
	}
	// valid_from is the supersede time recorded locally, so a retry sends the
	// identical request under the same Idempotency-Key.
	return b.supersedeToHead(ctx, scope, oldID, newID, m.UpdatedAt)
}

// replacementEntity returns the brainsrv entity id and scope of the live
// head of m's replacement chain. brainsrv refuses (409) a supersede whose
// replacement is itself superseded, so a local chain A→B→C sends A→C. A
// replacement that is not in local SQLite (approved on another machine and
// accepted through GetMissing) is resolved in brainsrv without re-sending
// its remember.
func (b *BrainsrvBackend) replacementEntity(ctx context.Context, m asymptoteobserve.LearningMemoryV1, useCache bool) (string, string, error) {
	seen := map[string]bool{m.ID: true}
	id := strings.TrimSpace(m.SupersededBy)
	for hop := 0; hop < maxSupersedeHops; hop++ {
		if seen[id] {
			return "", "", fmt.Errorf("supersede chain of memory %s loops at %s", m.ID, id)
		}
		seen[id] = true
		repl, ok, err := b.store.GetMemory(id)
		if err != nil {
			return "", "", err
		}
		if !ok {
			view, found, err := b.lookupEntity(ctx, id)
			if err != nil {
				return "", "", err
			}
			if !found {
				return "", "", fmt.Errorf("replacement memory %s is in neither the local store nor brainsrv", id)
			}
			return view.ID, firstNonEmpty(view.Scope, b.cfg.Scope), nil
		}
		if next := strings.TrimSpace(repl.SupersededBy); next != "" {
			id = next
			continue
		}
		newID, err := b.entityID(ctx, repl, useCache)
		if err != nil {
			return "", "", err
		}
		_ = b.markSynced(repl.ID)
		return newID, b.memoryScope(repl), nil
	}
	return "", "", fmt.Errorf("supersede chain of memory %s is longer than %d", m.ID, maxSupersedeHops)
}

// supersedeToHead supersedes oldID by newID. On a 409 it reads both
// entities: an old entity brainsrv already holds as superseded is out of
// recall, which is all the sync needs; a replacement superseded in brainsrv
// (by another machine, or before this retry) is followed to its head. Any
// other conflict is returned with a hint, and stays permanent (failed).
func (b *BrainsrvBackend) supersedeToHead(ctx context.Context, scope, oldID, newID, validFrom string) error {
	seen := map[string]bool{oldID: true}
	for hop := 0; hop < maxSupersedeHops; hop++ {
		seen[newID] = true
		err := b.supersede(ctx, scope, oldID, newID, validFrom)
		if !isConflict(err) {
			return err
		}
		old, found, gerr := b.entityView(ctx, scope, oldID)
		if gerr != nil {
			return fmt.Errorf("%w (and reading entity %s: %v)", err, oldID, gerr)
		}
		if found && old.State == "superseded" {
			return nil
		}
		repl, found, gerr := b.entityView(ctx, scope, newID)
		if gerr != nil {
			return fmt.Errorf("%w (and reading entity %s: %v)", err, newID, gerr)
		}
		next := ""
		if found && repl.State == "superseded" && repl.SupersededBy != nil {
			next = strings.TrimSpace(*repl.SupersededBy)
		}
		if next == "" || seen[next] {
			return fmt.Errorf("%w (supersede the memory again with a live replacement, then run `beacon memory brainsrv sync`)", err)
		}
		newID = next
	}
	return fmt.Errorf("brainsrv supersede chain from entity %s is longer than %d", oldID, maxSupersedeHops)
}

// PutMemory implements MemoryBackend: it syncs m and records the outcome in
// memory_sync. The returned error is informational; the caller has already
// committed the local write.
func (b *BrainsrvBackend) PutMemory(ctx context.Context, m asymptoteobserve.LearningMemoryV1) error {
	useCache := strings.TrimSpace(m.SupersededBy) != ""
	return b.syncAndRecord(ctx, m, useCache)
}

func (b *BrainsrvBackend) syncAndRecord(ctx context.Context, m asymptoteobserve.LearningMemoryV1, useCache bool) error {
	err := b.syncMemory(ctx, m, useCache)
	if err == nil {
		if rerr := b.markSynced(m.ID); rerr != nil {
			return fmt.Errorf("record sync state: %w", rerr)
		}
		return nil
	}
	state := SyncStatePending
	if isPermanent(err) {
		state = SyncStateFailed
	}
	if rerr := b.markError(m.ID, state, err); rerr != nil {
		return fmt.Errorf("%v (and record sync state: %v)", err, rerr)
	}
	return err
}

// ---- recall ----------------------------------------------------------------

// BrainsrvRecallHit is one ranked /v1/recall result.
type BrainsrvRecallHit struct {
	Table      string  `json:"table"`
	ID         string  `json:"id"`
	Text       string  `json:"text"`
	KnownAt    string  `json:"known_at"`
	SrcKind    string  `json:"src_kind"`
	EntityID   string  `json:"entity_id"`
	EntityType string  `json:"entity_type"`
	EntityName string  `json:"entity_name"`
	Category   string  `json:"category,omitempty"`
	Confidence float64 `json:"confidence,omitempty"`
}

type brainsrvRecallResponse struct {
	Results  []BrainsrvRecallHit `json:"results"`
	Degraded []string            `json:"degraded"`
}

func (b *BrainsrvBackend) recall(ctx context.Context, scope, query string, k int) (brainsrvRecallResponse, error) {
	var res brainsrvRecallResponse
	body := map[string]interface{}{"query": query, "k": k, "mode": "memories"}
	err := b.do(ctx, http.MethodPost, "/v1/recall", scope, "", body, &res)
	return res, err
}

// Recall runs a raw memories-mode recall at scope and returns brainsrv's
// ranked hits unfiltered, plus brainsrv's own degraded list. scope must be
// the base scope or sit under it (the experimental MCP recall_brain tool).
func (b *BrainsrvBackend) Recall(ctx context.Context, scope, query string, k int) ([]BrainsrvRecallHit, []string, error) {
	if !b.cfg.Covers(scope) {
		return nil, nil, fmt.Errorf("scope %q is not at or under the base scope %q", scope, b.cfg.Scope)
	}
	if strings.TrimSpace(query) == "" {
		return nil, nil, errors.New("query is required")
	}
	res, err := b.recall(ctx, scope, strings.TrimSpace(query), k)
	if err != nil {
		return nil, nil, err
	}
	return res.Results, res.Degraded, nil
}

// SearchMemories implements MemoryBackend.
func (b *BrainsrvBackend) SearchMemories(ctx context.Context, q Query) ([]asymptoteobserve.LearningMemoryV1, error) {
	out, _, err := b.searchWithHistory(ctx, q)
	return out, err
}

// Recall over-fetch. brainsrv ranks attribute rows, not entities, and does
// not dedupe per entity: one memory's summary, title and body each come back
// as separate hits. searchWithHistory therefore asks for recallOverfetch×
// the memories it needs and, when the distinct memories still fall short and
// brainsrv filled k, repeats with a larger k up to maxRecallK.
const (
	recallOverfetch = 4
	minRecallK      = 20
	maxRecallK      = 2000
)

func (b *BrainsrvBackend) searchWithHistory(ctx context.Context, q Query) ([]asymptoteobserve.LearningMemoryV1, []HistoryHit, error) {
	scope, projectID, ok, err := b.searchScope(q)
	if err != nil {
		return nil, nil, err
	}
	if !ok {
		return []asymptoteobserve.LearningMemoryV1{}, nil, nil
	}
	query := strings.TrimSpace(q.Q)
	limit := normalizeLimit(q.Limit)
	skip := offset(q)
	want := limit + skip
	k := want * recallOverfetch
	if k < minRecallK {
		k = minRecallK
	}
	if k > maxRecallK {
		k = maxRecallK
	}
	type mapped struct {
		m     asymptoteobserve.LearningMemoryV1
		found bool
	}
	cache := map[string]mapped{} // entity id → memory, across rounds
	var (
		memories []asymptoteobserve.LearningMemoryV1
		history  []HistoryHit
	)
	for {
		res, err := b.recall(ctx, scope, query, k)
		if err != nil {
			return nil, nil, err
		}
		memories, history = nil, nil
		seen := map[string]bool{}
		for _, hit := range res.Results {
			if hit.EntityType != BrainsrvMemoryEntityType {
				history = append(history, HistoryHit{Text: hit.Text, Table: hit.Table, KnownAt: hit.KnownAt, SrcKind: hit.SrcKind, Source: historySource})
				continue
			}
			if hit.EntityID == "" || seen[hit.EntityID] {
				continue
			}
			seen[hit.EntityID] = true
			got, cached := cache[hit.EntityID]
			if !cached {
				m, found, err := b.memoryForHit(ctx, scope, hit)
				if err != nil {
					return nil, nil, err
				}
				got = mapped{m: m, found: found}
				cache[hit.EntityID] = got
			}
			m := got.m
			if !got.found || (q.Kind != "" && m.Kind != q.Kind) {
				continue
			}
			// Same-label projects share a scope; keep upstream's project_id
			// isolation.
			if projectID != "" && m.Project.ID != projectID {
				continue
			}
			memories = append(memories, m)
		}
		if len(memories) >= want || len(res.Results) < k || k >= maxRecallK {
			break
		}
		k *= recallOverfetch
		if k > maxRecallK {
			k = maxRecallK
		}
	}
	unsynced, err := b.unsyncedMatches(Query{ProjectID: projectID, Kind: q.Kind, Q: query})
	if err != nil {
		return nil, nil, err
	}
	memories = mergeUnsynced(unsynced, memories)
	if skip >= len(memories) {
		memories = nil
	} else {
		memories = memories[skip:]
	}
	if len(memories) > limit {
		memories = memories[:limit]
	}
	if memories == nil {
		memories = []asymptoteobserve.LearningMemoryV1{}
	}
	return memories, history, nil
}

// unsyncedMatches returns the local memories matching q whose memory_sync
// row is pending or failed: approved (or changed) here, but not in brainsrv
// yet. Without them a successful recall would hide every such memory, with
// no degraded flag, until someone ran `beacon memory brainsrv sync` by hand.
// The match is upstream's own local text search. Memories with no row at all
// predate the backend and are backfilled by `sync --all`; they are not
// merged, so brainsrv's ranking is not overridden wholesale.
func (b *BrainsrvBackend) unsyncedMatches(q Query) ([]asymptoteobserve.LearningMemoryV1, error) {
	db, err := b.syncDB()
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT memory_id FROM memory_sync WHERE state IN (?, ?)`, SyncStatePending, SyncStateFailed)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	unsynced := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			_ = db.Close()
			return nil, err
		}
		unsynced[id] = true
	}
	err = rows.Err()
	_ = rows.Close()
	_ = db.Close()
	if err != nil || len(unsynced) == 0 {
		return nil, err
	}
	q.Limit = 500
	local, err := b.store.ListMemories(q)
	if err != nil {
		return nil, err
	}
	var out []asymptoteobserve.LearningMemoryV1
	for _, m := range local {
		if unsynced[m.ID] {
			out = append(out, m)
		}
	}
	return out, nil
}

// mergeUnsynced puts the unsynced local matches first (they are exact term
// matches, usually fresh approvals) followed by the recall results, dropping
// any memory that appears in both.
func mergeUnsynced(unsynced, recalled []asymptoteobserve.LearningMemoryV1) []asymptoteobserve.LearningMemoryV1 {
	if len(unsynced) == 0 {
		return recalled
	}
	seen := map[string]bool{}
	out := make([]asymptoteobserve.LearningMemoryV1, 0, len(unsynced)+len(recalled))
	for _, list := range [][]asymptoteobserve.LearningMemoryV1{unsynced, recalled} {
		for _, m := range list {
			if seen[m.ID] {
				continue
			}
			seen[m.ID] = true
			out = append(out, m)
		}
	}
	return out
}

// memoryForHit maps a beacon.memory hit to a memory: local SQLite by
// entity_name first, else the entity's current attributes from brainsrv.
func (b *BrainsrvBackend) memoryForHit(ctx context.Context, scope string, hit BrainsrvRecallHit) (asymptoteobserve.LearningMemoryV1, bool, error) {
	if hit.EntityName != "" {
		m, ok, err := b.store.GetMemory(hit.EntityName)
		if err != nil {
			return m, false, err
		}
		if ok {
			return m, true, nil
		}
	}
	m, err := b.getEntityMemory(ctx, scope, hit.EntityID)
	if isNotFound(err) {
		return m, false, nil
	}
	return m, err == nil, err
}

// ---- entity read ---------------------------------------------------------

type brainsrvEntityAttribute struct {
	Value     json.RawMessage `json:"value"`
	ValueType string          `json:"value_type"`
	ValidFrom *time.Time      `json:"valid_from"`
}

type brainsrvEntityView struct {
	ID           string                             `json:"id"`
	Type         string                             `json:"type"`
	Name         string                             `json:"name"`
	State        string                             `json:"state"`
	Scope        string                             `json:"scope"`
	SupersededBy *string                            `json:"superseded_by,omitempty"`
	Attributes   map[string]brainsrvEntityAttribute `json:"attributes"`
}

// entityView is GET /v1/entities/{id}; found=false on a 404.
func (b *BrainsrvBackend) entityView(ctx context.Context, scope, entityID string) (brainsrvEntityView, bool, error) {
	var view brainsrvEntityView
	err := b.do(ctx, http.MethodGet, "/v1/entities/"+url.PathEscape(entityID), scope, "", nil, &view)
	if isNotFound(err) {
		return brainsrvEntityView{}, false, nil
	}
	if err != nil {
		return brainsrvEntityView{}, false, err
	}
	return view, true, nil
}

func (b *BrainsrvBackend) getEntityMemory(ctx context.Context, scope, entityID string) (asymptoteobserve.LearningMemoryV1, error) {
	view, found, err := b.entityView(ctx, scope, entityID)
	if err != nil {
		return asymptoteobserve.LearningMemoryV1{}, err
	}
	if !found || view.Type != BrainsrvMemoryEntityType {
		return asymptoteobserve.LearningMemoryV1{}, &BrainsrvError{Method: http.MethodGet, Path: "/v1/entities/" + entityID, Status: http.StatusNotFound, Body: "not a " + BrainsrvMemoryEntityType}
	}
	return entityToMemory(view), nil
}

// entityToMemory maps a beacon.memory entity's current attributes onto
// LearningMemoryV1 (the inverse of rememberBody).
func entityToMemory(view brainsrvEntityView) asymptoteobserve.LearningMemoryV1 {
	text := func(pred string) string {
		a, ok := view.Attributes[pred]
		if !ok {
			return ""
		}
		var s string
		if err := json.Unmarshal(a.Value, &s); err == nil {
			return s
		}
		return string(a.Value)
	}
	decode := func(pred string, dest interface{}) {
		a, ok := view.Attributes[pred]
		if !ok {
			return
		}
		raw := []byte(a.Value)
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			raw = []byte(s) // tolerate JSON stored as a text value
		}
		_ = json.Unmarshal(raw, dest)
	}
	m := asymptoteobserve.LearningMemoryV1{
		SchemaVersion: asymptoteobserve.LearningSchemaVersion,
		ID:            view.Name,
		CandidateID:   text("candidate_id"),
		Kind:          text("kind"),
		Title:         text("title"),
		Body:          text("body"),
		Applicability: text("applicability"),
	}
	decode("tags", &m.Tags)
	decode("evidence", &m.Evidence)
	decode("project", &m.Project)
	for _, pred := range []string{"title", "summary", "body"} {
		if a, ok := view.Attributes[pred]; ok && a.ValidFrom != nil {
			m.CreatedAt = a.ValidFrom.UTC().Format(time.RFC3339Nano)
			m.UpdatedAt = m.CreatedAt
			break
		}
	}
	if len(m.Tags) == 0 {
		m.Tags = nil
	}
	return m
}

// lookupEntity finds the beacon.memory entity brainsrv holds for memory id:
// the entity id comes from memory_sync when this machine has one, else from
// a recall for the id restricted to beacon.memory entities named exactly id.
func (b *BrainsrvBackend) lookupEntity(ctx context.Context, id string) (brainsrvEntityView, bool, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return brainsrvEntityView{}, false, nil
	}
	scope := b.cfg.Scope
	entityID := ""
	if row, ok, err := b.syncRow(id); err == nil && ok {
		entityID = row.EntityID
	}
	if entityID == "" {
		res, err := b.recall(ctx, scope, id, 20)
		if err != nil {
			return brainsrvEntityView{}, false, err
		}
		for _, hit := range res.Results {
			if hit.EntityType == BrainsrvMemoryEntityType && hit.EntityName == id && hit.EntityID != "" {
				entityID = hit.EntityID
				break
			}
		}
	}
	if entityID == "" {
		return brainsrvEntityView{}, false, nil
	}
	view, found, err := b.entityView(ctx, scope, entityID)
	if err != nil || !found {
		return brainsrvEntityView{}, false, err
	}
	if view.Type != BrainsrvMemoryEntityType || view.Name != id {
		return brainsrvEntityView{}, false, nil
	}
	if view.ID == "" {
		view.ID = entityID
	}
	return view, true, nil
}

// GetMemory implements MemoryBackend for a memory missing from local SQLite
// (approved on another machine). A memory brainsrv holds as superseded comes
// back with SupersededBy set to its replacement's memory id, as a local
// superseded row would.
func (b *BrainsrvBackend) GetMemory(ctx context.Context, id string) (asymptoteobserve.LearningMemoryV1, bool, error) {
	view, found, err := b.lookupEntity(ctx, id)
	if err != nil || !found {
		return asymptoteobserve.LearningMemoryV1{}, false, err
	}
	m := entityToMemory(view)
	if view.State == "superseded" && view.SupersededBy != nil && *view.SupersededBy != "" {
		m.SupersededBy = *view.SupersededBy // the entity id, if its name is unreadable
		if repl, ok, err := b.entityView(ctx, b.cfg.Scope, *view.SupersededBy); err == nil && ok && repl.Name != "" {
			m.SupersededBy = repl.Name
		}
	}
	return m, true, nil
}

// ---- health / status / sync ----------------------------------------------

// BrainsrvHealth is GET /v1/ingest/beacon/health.
type BrainsrvHealth struct {
	OK        bool `json:"ok"`
	Endpoints []struct {
		Hostname          string `json:"hostname"`
		LastSeen          string `json:"last_seen"`
		LastBatchAccepted *int   `json:"last_batch_accepted,omitempty"`
	} `json:"endpoints"`
}

// Health checks the key against the base scope.
func (b *BrainsrvBackend) Health(ctx context.Context) (BrainsrvHealth, error) {
	var h BrainsrvHealth
	err := b.do(ctx, http.MethodGet, "/v1/ingest/beacon/health?scope="+url.QueryEscape(b.cfg.Scope), "", "", nil, &h)
	return h, err
}

// SyncReport is the outcome of Sync.
type SyncReport struct {
	Attempted int      `json:"attempted"`
	Synced    int      `json:"synced"`
	Pending   int      `json:"pending"`
	Failed    int      `json:"failed"`
	Errors    []string `json:"errors,omitempty"`
}

// Sync retries every pending or failed memory_sync row. With all, every
// memory in the local store is (re)sent first, ignoring cached entity ids,
// which backfills a new or reset brainsrv.
func (b *BrainsrvBackend) Sync(ctx context.Context, all bool) (SyncReport, error) {
	var report SyncReport
	ids, err := b.syncCandidates(all)
	if err != nil {
		return report, err
	}
	for _, id := range ids {
		m, ok, err := b.store.GetMemory(id)
		if err != nil {
			return report, err
		}
		report.Attempted++
		if !ok {
			report.Failed++
			report.Errors = append(report.Errors, fmt.Sprintf("%s: not in the local store", id))
			_ = b.markError(id, SyncStateFailed, errors.New("not in the local store"))
			continue
		}
		callCtx, cancel := context.WithTimeout(ctx, 3*backendTimeout)
		err = b.syncAndRecord(callCtx, m, !all)
		cancel()
		switch {
		case err == nil:
			report.Synced++
		case isPermanent(err):
			report.Failed++
			report.Errors = append(report.Errors, fmt.Sprintf("%s: %v", id, err))
		default:
			report.Pending++
			report.Errors = append(report.Errors, fmt.Sprintf("%s: %v", id, err))
		}
	}
	return report, nil
}

// syncCandidates lists the memory ids to sync: every local memory with all
// (live ones first, so replacements precede the memories they supersede),
// else the pending and failed rows.
func (b *BrainsrvBackend) syncCandidates(all bool) ([]string, error) {
	db, err := b.syncDB()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	query := `SELECT memory_id FROM memory_sync WHERE state IN ('pending', 'failed') ORDER BY updated_at, memory_id`
	if all {
		query = `SELECT id FROM memories ORDER BY (superseded_by IS NOT NULL AND superseded_by <> ''), created_at, id`
	}
	rows, err := db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// SyncCounts returns the number of memory_sync rows per state.
func (b *BrainsrvBackend) SyncCounts() (map[string]int, error) {
	db, err := b.syncDB()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	counts := map[string]int{SyncStatePending: 0, SyncStateSynced: 0, SyncStateFailed: 0}
	rows, err := db.Query(`SELECT state, COUNT(*) FROM memory_sync GROUP BY state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var state string
		var n int
		if err := rows.Scan(&state, &n); err != nil {
			return nil, err
		}
		counts[state] = n
	}
	return counts, rows.Err()
}

// ---- memory_sync ---------------------------------------------------------

// ensureSyncSchema creates afferent's memory_sync table (PLAN B-3). It never
// touches PRAGMA user_version, which belongs to upstream's schema.
func ensureSyncSchema(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS memory_sync (
		memory_id TEXT PRIMARY KEY,
		state TEXT NOT NULL,
		attempts INTEGER NOT NULL DEFAULT 0,
		last_error TEXT,
		updated_at TEXT NOT NULL,
		entity_id TEXT
	)`)
	return err
}

func (b *BrainsrvBackend) syncDB() (*sql.DB, error) {
	db, err := b.store.db()
	if err != nil {
		return nil, err
	}
	if err := ensureSyncSchema(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// SyncRow is one memory_sync row.
type SyncRow struct {
	MemoryID  string `json:"memory_id"`
	State     string `json:"state"`
	Attempts  int    `json:"attempts"`
	LastError string `json:"last_error,omitempty"`
	UpdatedAt string `json:"updated_at"`
	EntityID  string `json:"entity_id,omitempty"`
}

func (b *BrainsrvBackend) syncRow(memoryID string) (SyncRow, bool, error) {
	db, err := b.syncDB()
	if err != nil {
		return SyncRow{}, false, err
	}
	defer db.Close()
	var row SyncRow
	var lastErr, entity sql.NullString
	err = db.QueryRow(`SELECT memory_id, state, attempts, last_error, updated_at, entity_id FROM memory_sync WHERE memory_id = ?`, memoryID).
		Scan(&row.MemoryID, &row.State, &row.Attempts, &lastErr, &row.UpdatedAt, &entity)
	if err == sql.ErrNoRows {
		return SyncRow{}, false, nil
	}
	if err != nil {
		return SyncRow{}, false, err
	}
	row.LastError, row.EntityID = lastErr.String, entity.String
	return row, true, nil
}

// SyncRow returns the memory_sync row for a memory.
func (b *BrainsrvBackend) SyncRow(memoryID string) (SyncRow, bool, error) { return b.syncRow(memoryID) }

func (b *BrainsrvBackend) execSync(query string, args ...interface{}) error {
	db, err := b.syncDB()
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.Exec(query, args...)
	return err
}

// setEntityID records the brainsrv entity id without changing the state; a
// new row starts pending until the whole sync completes.
func (b *BrainsrvBackend) setEntityID(memoryID, entityID string) error {
	return b.execSync(`INSERT INTO memory_sync (memory_id, state, attempts, updated_at, entity_id) VALUES (?, ?, 0, ?, ?)
		ON CONFLICT(memory_id) DO UPDATE SET entity_id = excluded.entity_id`,
		memoryID, SyncStatePending, nowString(), entityID)
}

func (b *BrainsrvBackend) markSynced(memoryID string) error {
	return b.execSync(`INSERT INTO memory_sync (memory_id, state, attempts, last_error, updated_at) VALUES (?, ?, 1, NULL, ?)
		ON CONFLICT(memory_id) DO UPDATE SET state = excluded.state, attempts = memory_sync.attempts + 1, last_error = NULL, updated_at = excluded.updated_at`,
		memoryID, SyncStateSynced, nowString())
}

func (b *BrainsrvBackend) markError(memoryID, state string, cause error) error {
	msg := cause.Error()
	if len(msg) > 1024 {
		msg = msg[:1024]
	}
	return b.execSync(`INSERT INTO memory_sync (memory_id, state, attempts, last_error, updated_at) VALUES (?, ?, 1, ?, ?)
		ON CONFLICT(memory_id) DO UPDATE SET state = excluded.state, attempts = memory_sync.attempts + 1, last_error = excluded.last_error, updated_at = excluded.updated_at`,
		memoryID, state, msg, nowString())
}
