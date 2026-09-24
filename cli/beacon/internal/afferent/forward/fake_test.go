package forward

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/afferent/auth"
)

// fakeBrain is an in-process brainsrv ingest endpoint. Like the real one it
// dedupes on event id, so a re-sent batch shows up as duplicates and never as
// a second copy.
type fakeBrain struct {
	t   *testing.T
	srv *httptest.Server

	mu        sync.Mutex
	order     []string       // event ids in first-arrival order
	seen      map[string]int // receipts per id
	requests  int
	lineCount []int // lines per request
	headers   []http.Header
	// respond, when set, may answer instead of the default 200. Returning 0
	// falls through to the default. It runs after the body is parsed but
	// before the events are stored, unless storeFirst is set.
	respond    func(n int, r *http.Request, ids []string) (int, string)
	storeFirst bool
	validToken func(tok string) bool
}

func newFakeBrain(t *testing.T) *fakeBrain {
	b := &fakeBrain{t: t, seen: map[string]int{}}
	b.srv = httptest.NewServer(http.HandlerFunc(b.handle))
	t.Cleanup(b.srv.Close)
	return b
}

func (b *fakeBrain) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != IngestPath {
		http.Error(w, "not found", 404)
		return
	}
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	b.mu.Lock()
	valid := b.validToken
	b.mu.Unlock()
	if valid != nil && !valid(tok) {
		http.Error(w, `{"error":"invalid token"}`, 401)
		return
	}
	if r.Header.Get("Content-Encoding") != "gzip" || r.Header.Get("Content-Type") != "application/x-ndjson" {
		http.Error(w, "bad encoding", 415)
		return
	}
	zr, err := gzip.NewReader(r.Body)
	if err != nil {
		http.Error(w, "bad gzip", 400)
		return
	}
	raw, _ := io.ReadAll(zr)
	var ids []string
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 1<<20), 4<<20)
	for sc.Scan() {
		var ev struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil || ev.ID == "" {
			b.t.Errorf("fake brainsrv got a line that is not a complete event: %q", sc.Text())
			continue
		}
		ids = append(ids, ev.ID)
	}
	b.mu.Lock()
	b.requests++
	n := b.requests
	b.lineCount = append(b.lineCount, len(ids))
	b.headers = append(b.headers, r.Header.Clone())
	respond, storeFirst := b.respond, b.storeFirst
	b.mu.Unlock()

	store := func() (acc, dup int) {
		b.mu.Lock()
		defer b.mu.Unlock()
		for _, id := range ids {
			if b.seen[id] == 0 {
				b.order = append(b.order, id)
				acc++
			} else {
				dup++
			}
			b.seen[id]++
		}
		return
	}
	if storeFirst {
		store()
	}
	if respond != nil {
		if code, body := respond(n, r, ids); code != 0 {
			http.Error(w, body, code)
			return
		}
	}
	acc, dup := 0, 0
	if !storeFirst {
		acc, dup = store()
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"accepted":%d,"duplicate":%d,"rejected":0,"sessions_touched":1}`, acc, dup)
}

func (b *fakeBrain) ids() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.order...)
}

func (b *fakeBrain) receipts(id string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.seen[id]
}

func (b *fakeBrain) reqs() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.requests
}

// fakeTokens is a Tokens whose current token and refresh outcome tests set.
type fakeTokens struct {
	mu      sync.Mutex
	cur     string
	next    string // what ForceRefresh rotates to
	err     error  // returned by Token
	refErr  error  // returned by ForceRefresh
	forced  int
	fetched int
}

func (f *fakeTokens) Token(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fetched++
	if f.err != nil {
		return "", f.err
	}
	return f.cur, nil
}

func (f *fakeTokens) ForceRefresh(_ context.Context, rejected string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.forced++
	if f.refErr != nil {
		return "", f.refErr
	}
	if f.cur == rejected && f.next != "" {
		f.cur = f.next
	}
	return f.cur, nil
}

func (f *fakeTokens) set(cur string, err, refErr error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cur, f.err, f.refErr = cur, err, refErr
}

var errLogin = fmt.Errorf("session ended: %w", auth.ErrLoginRequired)

// env is one test's log, state dir and fakes.
type env struct {
	t      *testing.T
	dir    string
	log    string
	state  string
	brain  *fakeBrain
	tokens *fakeTokens
	logs   *logSink
	seq    int
}

type logSink struct {
	mu    sync.Mutex
	lines []string
}

func (l *logSink) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logSink) contains(s string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, x := range l.lines {
		if strings.Contains(x, s) {
			return true
		}
	}
	return false
}

func newEnv(t *testing.T) *env {
	dir := t.TempDir()
	e := &env{
		t:      t,
		dir:    dir,
		log:    filepath.Join(dir, "logs", "runtime.jsonl"),
		state:  filepath.Join(dir, "state"),
		brain:  newFakeBrain(t),
		tokens: &fakeTokens{cur: "tok-1", next: "tok-2"},
		logs:   &logSink{},
	}
	if err := os.MkdirAll(filepath.Dir(e.log), 0o755); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) opts() Options {
	return Options{
		LogPath:  e.log,
		StateDir: e.state,
		URL:      e.brain.srv.URL,
		Context:  "afferent-poc",
		Scope:    "ws.t.people.u.harness",
		Tokens:   e.tokens,
		Once:     true,
		Logf:     e.logs.logf,
		Rand:     func() float64 { return 0.5 },
	}
}

func (e *env) run(o Options) error {
	e.t.Helper()
	fw, err := New(o)
	if err != nil {
		e.t.Fatal(err)
	}
	return fw.Run(context.Background())
}

func (e *env) mustRun(o Options) {
	e.t.Helper()
	if err := e.run(o); err != nil {
		e.t.Fatalf("run: %v", err)
	}
}

// event returns a complete NDJSON line for a new event id.
func (e *env) event() (string, string) {
	e.seq++
	id := fmt.Sprintf("e-%03d", e.seq)
	return id, fmt.Sprintf(`{"id":%q,"kind":"prompt","message":"hello %d"}`+"\n", id, e.seq)
}

// write appends n events to path and returns their ids.
func (e *env) write(path string, n int) []string {
	e.t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		e.t.Fatal(err)
	}
	defer f.Close()
	var ids []string
	for i := 0; i < n; i++ {
		id, line := e.event()
		if _, err := f.WriteString(line); err != nil {
			e.t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

func (e *env) appendRaw(path, s string) {
	e.t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		e.t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		e.t.Fatal(err)
	}
}

// rotate does what Beacon's writer does: shift the archives up by one and
// rename the live file to .1. The next write creates a new live file.
func (e *env) rotate() {
	e.t.Helper()
	os.Remove(e.log + ".5")
	for i := 4; i >= 1; i-- {
		if err := os.Rename(fmt.Sprintf("%s.%d", e.log, i), fmt.Sprintf("%s.%d", e.log, i+1)); err != nil && !os.IsNotExist(err) {
			e.t.Fatal(err)
		}
	}
	if err := os.Rename(e.log, e.log+".1"); err != nil {
		e.t.Fatal(err)
	}
}

// primeEmpty makes the first run happen on an empty log, so later writes are
// "new" and get delivered (a first run starts at the end of the log).
func (e *env) primeEmpty() {
	e.t.Helper()
	e.appendRaw(e.log, "")
	e.mustRun(e.opts())
}

func (e *env) status() *Status {
	e.t.Helper()
	s, err := ReadStatus(e.state)
	if err != nil {
		e.t.Fatal(err)
	}
	return s
}

func (e *env) wantIDs(want []string) {
	e.t.Helper()
	got := e.brain.ids()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		e.t.Fatalf("brainsrv has\n  %v\nwant\n  %v", got, want)
	}
}

func (e *env) wantEachOnce(ids []string) {
	e.t.Helper()
	for _, id := range ids {
		if n := e.brain.receipts(id); n != 1 {
			e.t.Fatalf("%s received %d times, want once", id, n)
		}
	}
}

// stepper drives a non-Once Run: every Sleep calls step with the sleep
// count, advances a fake clock, and never blocks. step returns true to stop.
type stepper struct {
	mu     sync.Mutex
	now    time.Time
	sleeps []time.Duration
}

func (s *stepper) Now() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.now
}

func (e *env) loop(o Options, step func(i int, d time.Duration) bool) (*stepper, error) {
	e.t.Helper()
	st := &stepper{now: time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	o.Once = false
	o.Now = st.Now
	o.Sleep = func(_ context.Context, d time.Duration) error {
		st.mu.Lock()
		st.sleeps = append(st.sleeps, d)
		i := len(st.sleeps)
		st.now = st.now.Add(d)
		st.mu.Unlock()
		if i > 500 {
			e.t.Errorf("loop did not finish after %d sleeps", i)
			cancel()
			return ctx.Err()
		}
		if step(i, d) {
			cancel()
			return ctx.Err()
		}
		return nil
	}
	fw, err := New(o)
	if err != nil {
		e.t.Fatal(err)
	}
	return st, fw.Run(ctx)
}
