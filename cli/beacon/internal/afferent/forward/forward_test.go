package forward

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestFirstRunStartsAtEndThenDeliversNewLines(t *testing.T) {
	e := newEnv(t)
	e.write(e.log, 3)
	e.appendRaw(e.log, `{"id":"half`) // a line being written right now
	e.mustRun(e.opts())
	if e.brain.reqs() != 0 {
		t.Fatalf("first run sent %d requests; it should start at the end of the log", e.brain.reqs())
	}
	// The first run must not start mid-line: the completed line is new.
	e.appendRaw(e.log, `-way","kind":"x"}`+"\n")
	ids := e.write(e.log, 2)
	e.mustRun(e.opts())
	e.wantIDs(append([]string{"half-way"}, ids...))

	h := e.brain.headers[0]
	if h.Get("Authorization") != "Bearer tok-1" || h.Get("X-Context") != "afferent-poc" || h.Get("X-Scope") != "ws.t.people.u.harness" {
		t.Fatalf("headers %v", h)
	}
	s := e.status()
	if s.LinesSent != 3 || s.Accepted != 3 || s.LagBytes != 0 || s.LastSuccessAt.IsZero() || s.State != StateStopped || !s.LogFound {
		t.Fatalf("status %+v", s)
	}
	fi, err := os.Stat(e.state + "/" + CheckpointsFile)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("checkpoints file %v %v", err, fi)
	}
}

func TestBackfillStartsFromOldestArchive(t *testing.T) {
	e := newEnv(t)
	var want []string
	want = append(want, e.write(e.log, 2)...)
	e.rotate()
	want = append(want, e.write(e.log, 2)...)
	e.rotate()
	want = append(want, e.write(e.log, 3)...)

	// A normal first run skips all of it...
	e.mustRun(e.opts())
	if e.brain.reqs() != 0 {
		t.Fatal("first run without --backfill sent history")
	}
	// ...--backfill sends it all, oldest archive first.
	o := e.opts()
	o.Backfill = true
	e.mustRun(o)
	e.wantIDs(want)
	e.wantEachOnce(want)

	// A second backfill re-sends everything; brainsrv's dedupe makes that
	// harmless.
	e.mustRun(o)
	e.wantIDs(want)
	for _, id := range want {
		if e.brain.receipts(id) != 2 {
			t.Fatalf("%s: %d receipts after two backfills", id, e.brain.receipts(id))
		}
	}
	if s := e.status(); s.Duplicate != int64(len(want)) {
		t.Fatalf("duplicates %d", s.Duplicate)
	}
}

func TestRestartResumesWithoutResending(t *testing.T) {
	e := newEnv(t)
	e.primeEmpty()
	a := e.write(e.log, 4)
	e.mustRun(e.opts())
	reqs := e.brain.reqs()
	// Nothing new: a restart sends nothing.
	e.mustRun(e.opts())
	if e.brain.reqs() != reqs {
		t.Fatal("restart re-sent acknowledged lines")
	}
	b := e.write(e.log, 3)
	e.mustRun(e.opts())
	e.wantIDs(append(a, b...))
	e.wantEachOnce(append(a, b...))
}

func TestPartialLineWaitsForNewline(t *testing.T) {
	e := newEnv(t)
	e.primeEmpty()
	ids := e.write(e.log, 1)
	e.appendRaw(e.log, `{"id":"e-late","kind":"pro`)
	e.mustRun(e.opts())
	e.wantIDs(ids)
	st, _ := ReadCheckpoints(e.state)
	var off int64
	for _, cp := range st.Files {
		off = cp.Offset
	}
	if fi, _ := os.Stat(e.log); off >= fi.Size() {
		t.Fatalf("checkpoint %d moved past the partial line (size %d)", off, fi.Size())
	}
	e.appendRaw(e.log, `mpt"}`+"\n")
	e.mustRun(e.opts())
	e.wantIDs(append(ids, "e-late"))
	e.wantEachOnce(append(ids, "e-late"))
}

func TestRotationFinishesOldFileThenNewFromZero(t *testing.T) {
	e := newEnv(t)
	e.primeEmpty()
	a := e.write(e.log, 3)
	e.mustRun(e.opts())
	// Lines land in the live file, then it rotates before the forwarder
	// reads them, and more lines go to the new live file.
	b := e.write(e.log, 2)
	e.rotate()
	c := e.write(e.log, 2)
	e.mustRun(e.opts())
	all := append(append(append([]string{}, a...), b...), c...)
	e.wantIDs(all)
	e.wantEachOnce(all)
}

// Rotation between reading a batch and flushing it (the flush timer is
// pending), and again while brainsrv is failing: every line arrives once,
// in order.
func TestRotationMidBatch(t *testing.T) {
	e := newEnv(t)
	e.primeEmpty()
	a := e.write(e.log, 3)
	var b, c []string
	var mu sync.Mutex
	fail := 1
	e.brain.respond = func(n int, _ *http.Request, _ []string) (int, string) {
		mu.Lock()
		defer mu.Unlock()
		if fail > 0 {
			fail--
			return 503, "warming up"
		}
		return 0, ""
	}
	o := e.opts()
	_, err := e.loop(o, func(i int, d time.Duration) bool {
		switch i {
		case 1: // pending batch waiting for the flush interval
			b = e.write(e.log, 2)
			e.rotate()
			c = e.write(e.log, 2)
		case 7: // after the 503 backoff: rotate again with the batch unsent
			e.rotate()
		}
		return len(e.brain.ids()) == 7
	})
	if err != nil {
		t.Fatal(err)
	}
	all := append(append(append([]string{}, a...), b...), c...)
	e.wantIDs(all)
	// The 503 batch was not stored by the fake, so each id arrived once.
	e.wantEachOnce(all)
}

func TestTruncationRestartsAtZero(t *testing.T) {
	e := newEnv(t)
	e.primeEmpty()
	a := e.write(e.log, 5)
	e.mustRun(e.opts())
	if err := os.Truncate(e.log, 0); err != nil {
		t.Fatal(err)
	}
	b := e.write(e.log, 1)
	e.mustRun(e.opts())
	e.wantIDs(append(a, b...))
	if !e.logs.contains("truncated") {
		t.Fatalf("truncation not logged: %v", e.logs.lines)
	}
}

// The file is truncated and rewritten past the old offset between two scans
// (same inode, larger size): the head fingerprint catches it.
func TestReplacedContentIsReadFromStart(t *testing.T) {
	e := newEnv(t)
	e.primeEmpty()
	a := e.write(e.log, 2)
	e.mustRun(e.opts())
	f, err := os.OpenFile(e.log, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	b := e.write(e.log, 6)
	e.mustRun(e.opts())
	e.wantIDs(append(a, b...))
	if !e.logs.contains("replaced") {
		t.Fatalf("replacement not logged: %v", e.logs.lines)
	}
}

func TestBatchLimits(t *testing.T) {
	e := newEnv(t)
	e.primeEmpty()
	ids := e.write(e.log, 7)
	o := e.opts()
	o.MaxLines = 3
	e.mustRun(o)
	e.wantIDs(ids)
	if got := e.brain.lineCount; len(got) != 3 || got[0] != 3 || got[1] != 3 || got[2] != 1 {
		t.Fatalf("lines per request %v", got)
	}
	// Byte limit: each event line is ~50 bytes.
	e2 := newEnv(t)
	e2.primeEmpty()
	ids = e2.write(e2.log, 4)
	o = e2.opts()
	o.MaxBatchBytes = 120
	e2.mustRun(o)
	e2.wantIDs(ids)
	for _, n := range e2.brain.lineCount {
		if n > 2 {
			t.Fatalf("byte limit ignored: %v", e2.brain.lineCount)
		}
	}
}

func TestOversizedLineIsSkipped(t *testing.T) {
	e := newEnv(t)
	e.primeEmpty()
	a := e.write(e.log, 1)
	e.appendRaw(e.log, `{"id":"huge","pad":"`+strings.Repeat("x", 5000)+`"}`+"\n")
	b := e.write(e.log, 1)
	o := e.opts()
	o.MaxLineBytes = 1000
	e.mustRun(o)
	e.wantIDs(append(a, b...))
	if s := e.status(); s.LinesSkipped != 1 || s.LagBytes != 0 {
		t.Fatalf("status %+v", s)
	}
	if !e.logs.contains("skipped a line over 1000 bytes") {
		t.Fatalf("skip not logged: %v", e.logs.lines)
	}
}

func TestOnlyOversizedLinesAdvanceWithoutSending(t *testing.T) {
	e := newEnv(t)
	e.primeEmpty()
	e.appendRaw(e.log, strings.Repeat("y", 3000)+"\n\n")
	o := e.opts()
	o.MaxLineBytes = 1000
	e.mustRun(o)
	if e.brain.reqs() != 0 {
		t.Fatal("sent a request with no lines")
	}
	if s := e.status(); s.LinesSkipped != 1 || s.LagBytes != 0 {
		t.Fatalf("status %+v", s)
	}
}

func Test401RefreshesOnceAndRetries(t *testing.T) {
	e := newEnv(t)
	e.primeEmpty()
	e.brain.validToken = func(tok string) bool { return tok == "tok-2" }
	ids := e.write(e.log, 2)
	e.mustRun(e.opts())
	e.wantIDs(ids)
	if e.tokens.forced != 1 {
		t.Fatalf("forced refreshes %d", e.tokens.forced)
	}
	if h := e.brain.headers[len(e.brain.headers)-1]; h.Get("Authorization") != "Bearer tok-2" {
		t.Fatalf("retried with %q", h.Get("Authorization"))
	}
}

func Test401ThenLoginRequiredPausesAndResumes(t *testing.T) {
	e := newEnv(t)
	e.primeEmpty()
	e.brain.validToken = func(tok string) bool { return tok == "good" }
	e.tokens.set("stale", nil, errLogin)
	ids := e.write(e.log, 2)

	// --once reports it.
	err := e.run(e.opts())
	var se *sendError
	if !errors.As(err, &se) || se.reason != ReasonLoginRequired {
		t.Fatalf("once: %v", err)
	}

	o := e.opts()
	o.PauseRetry = 30 * time.Second
	_, err = e.loop(o, func(i int, d time.Duration) bool {
		if d == o.PauseRetry && i < 20 {
			s := e.status()
			if s.State != StatePaused || s.PausedReason != ReasonLoginRequired || !strings.Contains(s.LastError, "afferent login") {
				t.Errorf("paused status %+v", s)
			}
			if len(e.brain.ids()) != 0 {
				t.Error("delivered while logged out")
			}
			// The user logs in again.
			e.tokens.set("good", nil, nil)
		}
		return len(e.brain.ids()) == 2
	})
	if err != nil {
		t.Fatal(err)
	}
	e.wantIDs(ids)
	if s := e.status(); s.PausedReason != "" {
		t.Fatalf("still paused: %+v", s)
	}
}

func TestLoggedOutBeforeSendingPauses(t *testing.T) {
	e := newEnv(t)
	e.primeEmpty()
	e.tokens.set("", errLogin, nil)
	e.write(e.log, 1)
	err := e.run(e.opts())
	var se *sendError
	if !errors.As(err, &se) || se.reason != ReasonLoginRequired || e.brain.reqs() != 0 {
		t.Fatalf("got %v, %d requests", err, e.brain.reqs())
	}
}

func Test403PausesWithoutDropping(t *testing.T) {
	e := newEnv(t)
	e.primeEmpty()
	denied := true
	e.brain.respond = func(int, *http.Request, []string) (int, string) {
		if denied {
			return 403, `{"error":"denied: write not granted on derived scope ws.x"}`
		}
		return 0, ""
	}
	ids := e.write(e.log, 2)
	o := e.opts()
	_, err := e.loop(o, func(i int, d time.Duration) bool {
		if d == DefaultPauseRetry && denied {
			s := e.status()
			if s.State != StatePaused || s.PausedReason != ReasonScopeDenied || !strings.Contains(s.LastError, "write not granted") {
				t.Errorf("status %+v", s)
			}
			denied = false // an admin fixes the grant
		}
		return len(e.brain.ids()) == 2
	})
	if err != nil {
		t.Fatal(err)
	}
	e.wantIDs(ids)
}

func Test403AsksForTheScopeAgain(t *testing.T) {
	e := newEnv(t)
	e.primeEmpty()
	e.brain.respond = func(_ int, r *http.Request, _ []string) (int, string) {
		if r.Header.Get("X-Scope") != "ws.t.people.new.harness" {
			return 403, "denied"
		}
		return 0, ""
	}
	ids := e.write(e.log, 1)
	o := e.opts()
	o.Rescope = func(context.Context) (string, error) { return "ws.t.people.new.harness", nil }
	e.mustRun(o)
	e.wantIDs(ids)
	if s := e.status(); s.Scope != "ws.t.people.new.harness" {
		t.Fatalf("status scope %q", s.Scope)
	}
}

func Test413SplitsBatch(t *testing.T) {
	e := newEnv(t)
	e.primeEmpty()
	e.brain.respond = func(_ int, _ *http.Request, ids []string) (int, string) {
		if len(ids) > 2 {
			return 413, "batch exceeds limit"
		}
		return 0, ""
	}
	ids := e.write(e.log, 5)
	e.mustRun(e.opts())
	e.wantIDs(ids)
	e.wantEachOnce(ids)
}

func Test413OnSingleLineSkipsIt(t *testing.T) {
	e := newEnv(t)
	e.primeEmpty()
	e.brain.respond = func(_ int, _ *http.Request, ids []string) (int, string) {
		for _, id := range ids {
			if id == "e-002" {
				return 413, "line too large"
			}
		}
		return 0, ""
	}
	ids := e.write(e.log, 3)
	e.mustRun(e.opts())
	e.wantIDs([]string{ids[0], ids[2]})
	if s := e.status(); s.LinesSkipped != 1 || s.LagBytes != 0 {
		t.Fatalf("status %+v", s)
	}
}

func Test5xxBacksOffExponentiallyAndCaps(t *testing.T) {
	e := newEnv(t)
	e.primeEmpty()
	failures := 12
	e.brain.respond = func(int, *http.Request, []string) (int, string) {
		if failures > 0 {
			failures--
			return 500, "db down"
		}
		return 0, ""
	}
	ids := e.write(e.log, 2)
	o := e.opts()
	o.BaseBackoff = time.Second
	o.MaxBackoff = 5 * time.Minute
	var backoffs []time.Duration
	_, err := e.loop(o, func(i int, d time.Duration) bool {
		if d != o.PollInterval && d != DefaultPoll && d != DefaultFlush {
			backoffs = append(backoffs, d)
		}
		if len(backoffs) == 1 {
			if s := e.status(); s.State != StateBackoff || !strings.Contains(s.LastError, "db down") || s.NextRetryAt.IsZero() {
				t.Errorf("backoff status %+v", s)
			}
		}
		return len(e.brain.ids()) == 2
	})
	if err != nil {
		t.Fatal(err)
	}
	e.wantIDs(ids)
	// Rand is 0.5, so each wait is 3/4 of the step: 1s, 2s, 4s … capped at 5m.
	if len(backoffs) != 12 {
		t.Fatalf("backoffs %v", backoffs)
	}
	if backoffs[0] != 750*time.Millisecond || backoffs[1] != 1500*time.Millisecond || backoffs[2] != 3*time.Second {
		t.Fatalf("backoffs %v", backoffs)
	}
	if last := backoffs[len(backoffs)-1]; last != 225*time.Second {
		t.Fatalf("cap: last backoff %v", last)
	}
}

// brainsrv commits the batch but the response is lost (a 502 from a proxy):
// the forwarder re-sends, and dedupe on event.id turns the re-send into
// duplicates.
func TestLostAckResendIsDeduped(t *testing.T) {
	e := newEnv(t)
	e.primeEmpty()
	e.brain.storeFirst = true
	lose := 1
	e.brain.respond = func(int, *http.Request, []string) (int, string) {
		if lose > 0 {
			lose--
			return 502, "bad gateway"
		}
		return 0, ""
	}
	ids := e.write(e.log, 3)
	_, err := e.loop(e.opts(), func(int, time.Duration) bool { return e.brain.reqs() == 2 })
	if err != nil {
		t.Fatal(err)
	}
	e.wantIDs(ids)
	for _, id := range ids {
		if e.brain.receipts(id) != 2 {
			t.Fatalf("%s: %d receipts", id, e.brain.receipts(id))
		}
	}
	// And it is acknowledged now: a restart sends nothing more.
	e.brain.storeFirst = false
	e.mustRun(e.opts())
	if e.brain.reqs() != 2 {
		t.Fatalf("requests %d after restart", e.brain.reqs())
	}
}

func TestNetworkErrorRetries(t *testing.T) {
	e := newEnv(t)
	e.primeEmpty()
	ids := e.write(e.log, 2)
	o := e.opts()
	down := 2
	var mu sync.Mutex
	o.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		if down > 0 {
			down--
			return nil, errors.New("connection refused")
		}
		return http.DefaultTransport.RoundTrip(r)
	})}
	_, err := e.loop(o, func(int, time.Duration) bool { return len(e.brain.ids()) == 2 })
	if err != nil {
		t.Fatal(err)
	}
	e.wantIDs(ids)
	if s := e.status(); !strings.Contains(s.LastError, "connection refused") {
		t.Fatalf("last error %q", s.LastError)
	}
	// --once returns network errors instead of retrying.
	e.write(e.log, 1)
	o.Once = true
	down = 1
	if err := e.run(o); err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("once: %v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestOther4xxBacksOffWithBody(t *testing.T) {
	e := newEnv(t)
	e.primeEmpty()
	bad := true
	e.brain.respond = func(int, *http.Request, []string) (int, string) {
		if bad {
			return 400, "invalid gzip body"
		}
		return 0, ""
	}
	ids := e.write(e.log, 1)
	if err := e.run(e.opts()); err == nil || !strings.Contains(err.Error(), "HTTP 400: invalid gzip body") {
		t.Fatalf("once: %v", err)
	}
	st, _ := ReadCheckpoints(e.state)
	for _, cp := range st.Files {
		if cp.Offset != 0 {
			t.Fatal("checkpoint advanced past a 400")
		}
	}
	_, err := e.loop(e.opts(), func(i int, d time.Duration) bool {
		if i == 2 {
			bad = false
		}
		return len(e.brain.ids()) == 1
	})
	if err != nil {
		t.Fatal(err)
	}
	e.wantIDs(ids)
}

func TestFlushIntervalBatchesTrickle(t *testing.T) {
	e := newEnv(t)
	e.primeEmpty()
	e.write(e.log, 1)
	o := e.opts()
	o.FlushInterval = 5 * time.Second
	o.PollInterval = time.Second
	st, err := e.loop(o, func(i int, d time.Duration) bool {
		if i <= 3 {
			e.write(e.log, 1) // more arrives while the first line waits
		}
		return len(e.brain.ids()) == 4
	})
	if err != nil {
		t.Fatal(err)
	}
	if e.brain.reqs() != 1 {
		t.Fatalf("%d requests; lines within one flush interval should share a batch", e.brain.reqs())
	}
	var waited time.Duration
	for _, d := range st.sleeps[:5] {
		waited += d
	}
	if waited != 5*time.Second {
		t.Fatalf("flushed after %v (sleeps %v)", waited, st.sleeps)
	}
}

func TestSingleInstance(t *testing.T) {
	e := newEnv(t)
	e.primeEmpty()
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	o := e.opts()
	o.Once = false
	o.Sleep = func(ctx context.Context, d time.Duration) error {
		select {
		case <-started:
		default:
			close(started)
		}
		<-release
		return context.Canceled
	}
	fw, _ := New(o)
	go func() { done <- fw.Run(context.Background()) }()
	<-started
	err := e.run(e.opts())
	if !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second forwarder: %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("first forwarder: %v", err)
	}
	// Free again.
	e.mustRun(e.opts())
}

func TestCancelIsClean(t *testing.T) {
	e := newEnv(t)
	e.primeEmpty()
	o := e.opts()
	o.Once = false
	ctx, cancel := context.WithCancel(context.Background())
	o.Sleep = func(ctx context.Context, d time.Duration) error { cancel(); return ctx.Err() }
	fw, _ := New(o)
	if err := fw.Run(ctx); err != nil {
		t.Fatalf("run: %v", err)
	}
	if s := e.status(); s.State != StateStopped {
		t.Fatalf("state %q", s.State)
	}
}

func TestMissingLogIsNotAnError(t *testing.T) {
	e := newEnv(t)
	e.mustRun(e.opts())
	if s := e.status(); s.LogFound {
		t.Fatal("log reported found")
	}
	// The log appears later (Beacon installed after afferent): read from 0,
	// because the first run already happened.
	ids := e.write(e.log, 2)
	e.mustRun(e.opts())
	e.wantIDs(ids)
}

func TestLagReportsBytesBehind(t *testing.T) {
	e := newEnv(t)
	e.primeEmpty()
	e.write(e.log, 3)
	e.brain.respond = func(int, *http.Request, []string) (int, string) { return 503, "down" }
	_ = e.run(e.opts())
	fi, _ := os.Stat(e.log)
	s := e.status()
	if s.LagBytes != fi.Size() || len(s.Lag) != 1 || s.Lag[0].Behind != fi.Size() {
		t.Fatalf("lag %+v (size %d)", s, fi.Size())
	}
}

func TestScopeLookupWaitsForLogin(t *testing.T) {
	e := newEnv(t)
	e.primeEmpty()
	ids := e.write(e.log, 1)
	signedIn := false
	o := e.opts()
	o.Scope = ""
	o.Rescope = func(context.Context) (string, error) {
		if !signedIn {
			return "", errLogin
		}
		return "ws.t.people.u.harness", nil
	}
	// --once fails fast.
	if err := e.run(o); !errors.Is(err, errLogin) {
		t.Fatalf("once: %v", err)
	}
	_, err := e.loop(o, func(i int, d time.Duration) bool {
		if d == DefaultPauseRetry {
			if s := e.status(); s.State != StatePaused || s.PausedReason != ReasonLoginRequired {
				t.Errorf("status %+v", s)
			}
			signedIn = true
		}
		return len(e.brain.ids()) == 1
	})
	if err != nil {
		t.Fatal(err)
	}
	e.wantIDs(ids)
}
