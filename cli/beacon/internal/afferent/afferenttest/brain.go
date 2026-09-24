package afferenttest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Brain is a fake brainsrv serving the read API `afferent ui` uses:
// /v1/whoami, /v1/overview, /v1/graph, /v1/recall, /v1/entities/{id},
// /v1/sessions and /v1/sessions/{id}/turns, over a deterministic data set
// (several repos and harnesses, 40 entities, 60 relations) under Member.
type Brain struct {
	Server *httptest.Server
	Member string
	// Context is the X-Context every request must carry.
	Context string
	// Now anchors the data set's times.
	Now time.Time
	// EchoAuth makes every answer echo the Authorization header (a hostile
	// brainsrv), to test that the token never reaches the browser.
	EchoAuth bool
	// Redirect answers every data call with a 307 to RedirectTo.
	RedirectTo string
	// RejectTokens are refused with 401.
	RejectTokens map[string]bool
	// MaxChildren, when positive, cuts every overview node's children to
	// the first MaxChildren by turns and marks that node truncated, as
	// brainsrv does at 200 (the cut children's turns stay in the parent's).
	MaxChildren int

	mu       sync.Mutex
	Requests []BrainRequest

	sessions []fakeSession
	turns    map[string][]map[string]any
	entities []fakeEntity
	edges    []map[string]any
}

// BrainRequest is one recorded request.
type BrainRequest struct {
	Method, Path, Query, Scope, Context, Auth string
	Body                                      string
}

type fakeSession struct {
	ID, Scope, Channel string
	Started, Last      time.Time
	Turns              int
}

type fakeEntity struct {
	ID, Name, Type, Scope string
	Degree                int
	Facts                 map[string]any
	LastSeen              time.Time
}

// FakeID makes a deterministic UUID.
func FakeID(kind byte, n int) string {
	return fmt.Sprintf("%08x-%04x-4000-8000-%012x", n, int(kind), n)
}

// NewBrain starts a fake brainsrv; it is closed when the test ends.
func NewBrain(t testing.TB, member string) *Brain {
	b := &Brain{Member: member, Context: "afferent-poc", Now: time.Now().UTC()}
	b.build()
	b.Server = httptest.NewServer(http.HandlerFunc(b.serve))
	t.Cleanup(b.Server.Close)
	return b
}

// URL is the base URL.
func (b *Brain) URL() string { return b.Server.URL }

// Last returns the most recent recorded request to path (or nil).
func (b *Brain) Last(path string) *BrainRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i := len(b.Requests) - 1; i >= 0; i-- {
		if b.Requests[i].Path == path {
			r := b.Requests[i]
			return &r
		}
	}
	return nil
}

var (
	fakeRepos     = []string{"afferent", "brainsrv", "authsrv", "vero_mobile", "gitgraph", "_norepo"}
	fakeHarnesses = []string{"claude", "codex", "cursor"}
	fakeTypes     = []string{"repo", "service", "file", "concept", "person", "tool", "library"}
	fakeNames     = []string{
		"afferent", "brainsrv", "authsrv", "Keychain", "launchd", "forwarder", "device flow", "refresh token",
		"X-Scope", "ltree", "recall", "treemap", "d3", "Drew", "MCP proxy", "Codex", "Claude Code", "Cursor",
		"Postgres", "pgvector", "extraction", "reconcile", "grant template", "whoami", "status.json", "runtime.jsonl",
		"Beacon", "OAuth", "JWT", "CSP", "vero-local", "gitgraph", "session", "turn", "entity", "relation",
		"sync", "backfill", "systemd", "Homebrew",
	}
	fakePredicates = []string{"uses", "depends_on", "calls", "owns", "mentions", "part_of", "configures", "stores"}
)

func (b *Brain) build() {
	b.turns = map[string][]map[string]any{}
	// Deterministic pseudo-random numbers.
	seed := uint32(7)
	rnd := func(n int) int {
		seed = seed*1664525 + 1013904223
		return int(seed>>8) % n
	}
	n := 0
	for ri, repo := range fakeRepos {
		for hi, h := range fakeHarnesses {
			if (ri+hi)%4 == 3 {
				continue // not every repo has every harness
			}
			scope := b.Member + "." + repo + "." + h
			count := 1 + rnd(6) + (len(fakeRepos)-ri)*2/(hi+1)
			for i := 0; i < count; i++ {
				n++
				age := time.Duration(ri*ri*6+hi*3+i*9+rnd(12)) * time.Hour
				if ri == 0 && hi == 0 {
					age = time.Duration(i*7+2) * time.Minute
				}
				s := fakeSession{ID: FakeID('s', n), Scope: scope, Channel: h, Last: b.Now.Add(-age), Turns: 2 + rnd(40)}
				s.Started = s.Last.Add(-time.Duration(s.Turns) * 3 * time.Minute)
				b.sessions = append(b.sessions, s)
				var ts []map[string]any
				for k := 0; k < s.Turns; k++ {
					role := "user"
					text := fmt.Sprintf("Can you look at %s in %s? Step %d.", fakeNames[rnd(len(fakeNames))], repo, k+1)
					if k%2 == 1 {
						role = "assistant"
						text = fmt.Sprintf("I read %s and updated %s; the tests pass now.", fakeNames[rnd(len(fakeNames))], fakeNames[rnd(len(fakeNames))])
					}
					at := s.Started.Add(time.Duration(k) * 3 * time.Minute)
					ts = append(ts, map[string]any{
						"id": FakeID('t', n*100+k), "seq": k + 1, "role": role, "text": text, "kind": "message",
						"extracted": k < s.Turns-1, "occurred_at": at, "created_at": at,
					})
				}
				b.turns[s.ID] = ts
			}
		}
	}
	sort.Slice(b.sessions, func(i, j int) bool { return b.sessions[i].Last.After(b.sessions[j].Last) })

	for i, name := range fakeNames {
		repo := fakeRepos[i%len(fakeRepos)]
		h := fakeHarnesses[(i/len(fakeRepos))%len(fakeHarnesses)]
		e := fakeEntity{
			ID: FakeID('e', i+1), Name: name, Type: fakeTypes[(i*5+i/3)%len(fakeTypes)],
			Scope: b.Member + "." + repo + "." + h, LastSeen: b.Now.Add(-time.Duration(i*i) * time.Hour / 3),
			Facts: map[string]any{},
		}
		for k := 0; k < 1+rnd(4); k++ {
			e.Facts[[]string{"description", "language", "owner", "status", "path", "version"}[k]] = map[string]any{
				"value":      fmt.Sprintf("%s %s", name, []string{"is central to the brain", "Go", "drew", "active", "cli/beacon", "v0.3"}[k]),
				"value_type": "text", "valid_from": e.LastSeen.Format(time.RFC3339), "confidence": 0.6 + float64(rnd(40))/100,
			}
		}
		b.entities = append(b.entities, e)
	}
	seen := map[string]bool{}
	for len(b.edges) < 60 {
		// Hubs: the first few entities get most of the edges.
		a := rnd(len(b.entities))
		if rnd(3) > 0 {
			a = rnd(8)
		}
		c := rnd(len(b.entities))
		key := fmt.Sprint(a, c)
		if a == c || seen[key] {
			continue
		}
		seen[key] = true
		b.entities[a].Degree++
		b.entities[c].Degree++
		b.edges = append(b.edges, map[string]any{
			"id": FakeID('r', len(b.edges)+1), "src": b.entities[a].ID, "dst": b.entities[c].ID,
			"predicate": fakePredicates[rnd(len(fakePredicates))], "confidence": 0.5 + float64(rnd(50))/100,
		})
	}
}

// Clear empties the data set (an empty brain).
func (b *Brain) Clear() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sessions, b.entities, b.edges = nil, nil, nil
	b.turns = map[string][]map[string]any{}
}

// Reject makes brainsrv refuse tok with 401.
func (b *Brain) Reject(tok string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.RejectTokens == nil {
		b.RejectTokens = map[string]bool{}
	}
	b.RejectTokens[tok] = true
}

// Recorded returns a copy of the recorded requests.
func (b *Brain) Recorded() []BrainRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]BrainRequest(nil), b.Requests...)
}

func (b *Brain) record(r *http.Request, body string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.Requests = append(b.Requests, BrainRequest{
		Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Scope: r.Header.Get("X-Scope"),
		Context: r.Header.Get("X-Context"), Auth: r.Header.Get("Authorization"), Body: body,
	})
}

func (b *Brain) under(scope, base string) bool {
	return scope == base || strings.HasPrefix(scope, base+".")
}

func brainJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (b *Brain) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	b.record(r, string(body))
	b.mu.Lock()
	defer b.mu.Unlock()
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") || r.Header.Get("X-Context") != b.Context {
		brainJSON(w, 401, map[string]string{"error": "unauthorized"})
		return
	}
	if b.RejectTokens[strings.TrimPrefix(auth, "Bearer ")] {
		brainJSON(w, 401, map[string]string{"error": "token rejected"})
		return
	}
	if r.URL.Path == "/v1/whoami" {
		brainJSON(w, 200, map[string]any{"principal_id": "p-1", "kind": "user", "context": b.Context, "subject": "user-1",
			"grants": []map[string]any{{"scope": b.Member, "verbs": []string{"read", "write", "forget"}}}})
		return
	}
	if b.RedirectTo != "" {
		http.Redirect(w, r, b.RedirectTo+r.URL.Path, http.StatusTemporaryRedirect)
		return
	}
	scope := r.Header.Get("X-Scope")
	if scope == "" || !b.under(scope, b.Member) {
		brainJSON(w, 403, map[string]string{"error": "denied"})
		return
	}
	echo := func(m map[string]any) map[string]any {
		if b.EchoAuth {
			m["debug_authorization"] = auth
		}
		return m
	}
	q := r.URL.Query()
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	switch {
	case r.Method == "GET" && r.URL.Path == "/v1/overview":
		depth, _ := strconv.Atoi(q.Get("depth"))
		if depth == 0 {
			depth = 3
		}
		brainJSON(w, 200, echo(b.overview(scope, depth)))
	case r.Method == "GET" && r.URL.Path == "/v1/graph":
		limit, _ := strconv.Atoi(q.Get("limit"))
		if limit == 0 {
			limit = 150
		}
		if s := q.Get("scope"); s != "" {
			if !b.under(s, scope) {
				brainJSON(w, 403, map[string]string{"error": "denied"})
				return
			}
			scope = s
		}
		brainJSON(w, 200, echo(b.graph(scope, limit)))
	case r.Method == "POST" && r.URL.Path == "/v1/recall":
		var in struct {
			Query string `json:"query"`
			K     int    `json:"k"`
			Mode  string `json:"mode"`
		}
		if json.Unmarshal(body, &in) != nil || in.Query == "" {
			brainJSON(w, 400, map[string]string{"error": "bad recall"})
			return
		}
		brainJSON(w, 200, echo(b.recall(scope, in.Query, in.K)))
	case r.Method == "GET" && len(parts) == 3 && parts[1] == "entities":
		for _, e := range b.entities {
			if e.ID == parts[2] && b.under(e.Scope, scope) {
				brainJSON(w, 200, echo(map[string]any{"id": e.ID, "type": e.Type, "name": e.Name, "state": "active", "scope": e.Scope, "attributes": e.Facts}))
				return
			}
		}
		brainJSON(w, 404, map[string]string{"error": "entity not found"})
	case r.Method == "GET" && r.URL.Path == "/v1/sessions":
		limit, _ := strconv.Atoi(q.Get("limit"))
		if limit == 0 {
			limit = 100
		}
		out := []map[string]any{}
		for _, s := range b.sessions {
			if b.under(s.Scope, scope) && len(out) < limit {
				out = append(out, map[string]any{"id": s.ID, "scope": s.Scope, "principal_id": FakeID('p', 1),
					"channel": s.Channel, "started_at": s.Started, "closed_at": nil, "last_activity": s.Last, "turns": s.Turns})
			}
		}
		if b.EchoAuth {
			out = append(out, map[string]any{"id": auth})
		}
		brainJSON(w, 200, out)
	case r.Method == "GET" && len(parts) == 4 && parts[1] == "sessions" && parts[3] == "turns":
		ts, ok := b.turns[parts[2]]
		if !ok {
			brainJSON(w, 404, map[string]string{"error": "session not found"})
			return
		}
		brainJSON(w, 200, echo(map[string]any{"session_id": parts[2], "turns": ts}))
	default:
		brainJSON(w, 404, map[string]string{"error": "not found"})
	}
}

func (b *Brain) overview(scope string, depth int) map[string]any {
	type agg struct {
		sessions, turns, entities, facts, relations int
		last                                        time.Time
		kids                                        map[string]*agg
		scope                                       string
	}
	root := &agg{kids: map[string]*agg{}, scope: scope}
	pending := 0
	walk := func(s string, f func(a *agg)) {
		if !b.under(s, scope) {
			return
		}
		f(root)
		rest := strings.TrimPrefix(strings.TrimPrefix(s, scope), ".")
		cur := root
		if rest == "" {
			return
		}
		for i, l := range strings.Split(rest, ".") {
			if i >= depth {
				break
			}
			k, ok := cur.kids[l]
			if !ok {
				k = &agg{kids: map[string]*agg{}, scope: cur.scope + "." + l}
				cur.kids[l] = k
			}
			f(k)
			cur = k
		}
	}
	for _, s := range b.sessions {
		walk(s.Scope, func(a *agg) {
			a.sessions++
			a.turns += s.Turns
			if s.Last.After(a.last) {
				a.last = s.Last
			}
		})
		pending++
	}
	for _, e := range b.entities {
		walk(e.Scope, func(a *agg) { a.entities++; a.facts += len(e.Facts) })
	}
	byID := map[string]fakeEntity{}
	for _, e := range b.entities {
		byID[e.ID] = e
	}
	for _, ed := range b.edges {
		walk(byID[ed["src"].(string)].Scope, func(a *agg) { a.relations++ })
	}
	var render func(a *agg) ([]map[string]any, bool)
	render = func(a *agg) ([]map[string]any, bool) {
		out := []map[string]any{}
		for label, k := range a.kids {
			var last any
			if !k.last.IsZero() {
				last = k.last
			}
			kids, cut := render(k)
			n := map[string]any{"scope": k.scope, "label": label, "sessions": k.sessions, "turns": k.turns,
				"entities": k.entities, "facts": k.facts, "relations": k.relations, "last_activity": last, "children": kids}
			if cut {
				n["truncated"] = true
			}
			out = append(out, n)
		}
		sort.Slice(out, func(i, j int) bool {
			ti, tj := out[i]["turns"].(int), out[j]["turns"].(int)
			if ti != tj {
				return ti > tj
			}
			return out[i]["label"].(string) < out[j]["label"].(string)
		})
		if b.MaxChildren > 0 && len(out) > b.MaxChildren {
			return out[:b.MaxChildren], true
		}
		return out, false
	}
	kids, cut := render(root)
	out := map[string]any{
		"scope": scope, "generated_at": b.Now,
		"totals": map[string]int{"sessions": root.sessions, "turns": root.turns, "entities": root.entities,
			"facts": root.facts, "relations": root.relations, "pending_extraction": pending},
		"children": kids,
	}
	if cut {
		out["truncated"] = true
	}
	return out
}

func (b *Brain) graph(scope string, limit int) map[string]any {
	var es []fakeEntity
	for _, e := range b.entities {
		if b.under(e.Scope, scope) {
			es = append(es, e)
		}
	}
	sort.SliceStable(es, func(i, j int) bool {
		if es[i].Degree != es[j].Degree {
			return es[i].Degree > es[j].Degree
		}
		return es[i].LastSeen.After(es[j].LastSeen)
	})
	truncated := len(es) > limit
	if truncated {
		es = es[:limit]
	}
	in := map[string]bool{}
	nodes := []map[string]any{}
	for _, e := range es {
		in[e.ID] = true
		nodes = append(nodes, map[string]any{"id": e.ID, "name": e.Name, "type": e.Type, "scope": e.Scope,
			"degree": e.Degree, "facts": len(e.Facts), "last_seen": e.LastSeen})
	}
	edges := []map[string]any{}
	for _, ed := range b.edges {
		if in[ed["src"].(string)] && in[ed["dst"].(string)] {
			edges = append(edges, ed)
		}
	}
	return map[string]any{"scope": scope, "nodes": nodes, "edges": edges, "truncated": truncated}
}

func (b *Brain) recall(scope, query string, k int) map[string]any {
	q := strings.ToLower(query)
	out := []map[string]any{}
	for _, e := range b.entities {
		if !b.under(e.Scope, scope) {
			continue
		}
		for pred, f := range e.Facts {
			v := f.(map[string]any)["value"].(string)
			if strings.Contains(strings.ToLower(v), q) || strings.Contains(strings.ToLower(pred), q) {
				out = append(out, map[string]any{"table": "attributes", "id": FakeID('a', len(out)+1), "text": pred + ": " + v,
					"entity_id": e.ID, "entity_type": e.Type, "entity_name": e.Name, "known_at": e.LastSeen,
					"category": "fact", "confidence": f.(map[string]any)["confidence"]})
				break
			}
		}
		if len(out) >= k && k > 0 {
			break
		}
	}
	return map[string]any{"tier": 1, "results": out}
}
