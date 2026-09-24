// Package forward is afferent's built-in forwarder. It tails Beacon's runtime
// log (runtime.jsonl and its rotated archives .1–.5) and posts the lines to
// brainsrv's Beacon ingest as gzipped NDJSON, as the signed-in user.
//
// Delivery is at least once. A checkpoint per file identity (device and
// inode) records how far brainsrv has acknowledged; it only moves after a 200
// for the batch that held those bytes, and it is written atomically. brainsrv
// dedupes on event.id, so a batch re-sent after a crash or a lost response is
// harmless. Nothing is dropped on an error: a batch is retried until brainsrv
// takes it, except a single line brainsrv refuses as too large (413) and a line
// over MaxLineBytes, which are skipped and counted.
package forward

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/afferent/auth"
	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/afferent/config"
)

// Defaults.
const (
	DefaultMaxLines      = 5000
	DefaultMaxBatchBytes = 4 << 20
	DefaultMaxLineBytes  = 1 << 20
	DefaultFlush         = 5 * time.Second
	DefaultPoll          = time.Second
	DefaultBaseBackoff   = time.Second
	DefaultMaxBackoff    = 5 * time.Minute
	DefaultPauseRetry    = time.Minute
)

// ErrAlreadyRunning means another forwarder holds the state directory.
var ErrAlreadyRunning = errors.New("another afferent forwarder is already running with this state directory")

// Tokens hands out access tokens. auth.TokenSource implements it.
type Tokens interface {
	Token(ctx context.Context) (string, error)
	// ForceRefresh returns a token other than rejected, refreshing if needed.
	ForceRefresh(ctx context.Context, rejected string) (string, error)
}

// Options configure a Forwarder. Zero values take the defaults.
type Options struct {
	LogPath  string // Beacon runtime log (runtime.jsonl)
	StateDir string // checkpoints, status, lock

	URL     string // brainsrv base URL
	Context string // X-Context
	// Scope is X-Scope, the member base scope. When empty, Rescope is asked
	// for it at start (and again until it answers, while paused).
	Scope  string
	Tokens Tokens
	HTTP   *http.Client
	// Rescope, when set, looks the scope up (brainsrv /v1/whoami). It is
	// asked at start when Scope is empty and again after a 403, in case the
	// member's grant moved. It is nil when the scope was configured.
	Rescope func(ctx context.Context) (string, error)

	// Backfill starts from the oldest retained archive instead of resuming
	// (or, on a first run, instead of starting at the end of the live log).
	Backfill bool
	// Once drains what is in the log now and returns; any delivery error is
	// returned instead of retried.
	Once bool

	MaxLines      int
	MaxBatchBytes int
	MaxLineBytes  int
	FlushInterval time.Duration
	PollInterval  time.Duration
	BaseBackoff   time.Duration
	MaxBackoff    time.Duration
	PauseRetry    time.Duration

	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error
	Rand  func() float64 // jitter in [0,1)
	Logf  func(format string, args ...any)
}

// Forwarder tails the runtime log and delivers it to brainsrv.
type Forwarder struct {
	opts   Options
	scope  string
	state  *State
	status Status
	// failures counts consecutive failed deliveries, for the backoff.
	failures       int
	lastStatusSave time.Time
}

// New validates opts and fills in defaults.
func New(opts Options) (*Forwarder, error) {
	if opts.LogPath == "" || opts.StateDir == "" {
		return nil, errors.New("forward: LogPath and StateDir are required")
	}
	if opts.URL == "" || opts.Context == "" {
		return nil, errors.New("forward: brainsrv URL and Context are required")
	}
	if opts.Scope == "" && opts.Rescope == nil {
		return nil, errors.New("forward: a Scope or a way to look it up (Rescope) is required")
	}
	if opts.Tokens == nil {
		return nil, errors.New("forward: Tokens is required")
	}
	opts.URL = strings.TrimRight(opts.URL, "/")
	def := func(v *int, d int) {
		if *v <= 0 {
			*v = d
		}
	}
	defd := func(v *time.Duration, d time.Duration) {
		if *v <= 0 {
			*v = d
		}
	}
	def(&opts.MaxLines, DefaultMaxLines)
	def(&opts.MaxBatchBytes, DefaultMaxBatchBytes)
	def(&opts.MaxLineBytes, DefaultMaxLineBytes)
	defd(&opts.FlushInterval, DefaultFlush)
	defd(&opts.PollInterval, DefaultPoll)
	defd(&opts.BaseBackoff, DefaultBaseBackoff)
	defd(&opts.MaxBackoff, DefaultMaxBackoff)
	defd(&opts.PauseRetry, DefaultPauseRetry)
	if opts.HTTP == nil {
		opts.HTTP = &http.Client{Timeout: 2 * time.Minute}
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Sleep == nil {
		opts.Sleep = sleepCtx
	}
	if opts.Rand == nil {
		opts.Rand = rand.Float64
	}
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}
	return &Forwarder{opts: opts, scope: opts.Scope}, nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (fw *Forwarder) logf(format string, args ...any) { fw.opts.Logf(format, args...) }

// Run forwards until ctx is cancelled (nil is returned then), or, with Once,
// until the log is drained. It holds an exclusive lock on the state
// directory for its whole run.
func (fw *Forwarder) Run(ctx context.Context) error {
	if err := config.EnsureDir(fw.opts.StateDir); err != nil {
		return err
	}
	release, ok, err := tryLock(filepath.Join(fw.opts.StateDir, LockFileName))
	if err != nil {
		return fmt.Errorf("lock state directory: %w", err)
	}
	if !ok {
		return fmt.Errorf("%w (%s)", ErrAlreadyRunning, fw.opts.StateDir)
	}
	defer release()

	st, err := loadState(fw.opts.StateDir, fw.opts.LogPath)
	if err != nil {
		return err
	}
	if fw.opts.Backfill {
		fw.logf("backfill: starting from the oldest retained archive of %s", fw.opts.LogPath)
		st = &State{Version: stateVersion, LogPath: fw.opts.LogPath, Files: map[string]*Checkpoint{}}
	}
	fw.state = st
	fw.initStatus()
	defer func() {
		fw.status.State = StateStopped
		fw.status.NextRetryAt = time.Time{}
		fw.saveStatus(true)
	}()
	fw.saveStatus(true)
	if fw.scope == "" {
		if err := fw.resolveScope(ctx); err != nil || ctx.Err() != nil {
			return err
		}
	}
	return fw.loop(ctx)
}

// resolveScope asks Rescope until it answers. Being signed out or brainsrv
// being unreachable pauses rather than exits, so a service started before
// `afferent login` picks up once the user signs in.
func (fw *Forwarder) resolveScope(ctx context.Context) error {
	for {
		s, err := fw.opts.Rescope(ctx)
		if err == nil && s != "" {
			fw.scope = s
			fw.logf("forwarding %s to %s (context %s, scope %s)", fw.opts.LogPath, fw.opts.URL, fw.opts.Context, s)
			return nil
		}
		if ctx.Err() != nil {
			return nil
		}
		if err == nil {
			err = errors.New("brainsrv returned an empty scope")
		}
		if fw.opts.Once {
			fw.recordError(err)
			return err
		}
		reason := "scope unknown"
		if errors.Is(err, auth.ErrLoginRequired) {
			reason = ReasonLoginRequired
		}
		fw.recordError(err)
		fw.status.State, fw.status.PausedReason = StatePaused, reason
		fw.status.NextRetryAt = fw.opts.Now().Add(fw.opts.PauseRetry).UTC()
		fw.saveStatus(true)
		fw.logf("paused (%s): %v; retrying in %s", reason, err, fw.opts.PauseRetry)
		if fw.wait(ctx, fw.opts.PauseRetry) {
			return nil
		}
	}
}

func (fw *Forwarder) initStatus() {
	prev, _ := ReadStatus(fw.opts.StateDir)
	s := Status{}
	if prev != nil {
		// Keep the lifetime counters and the last success.
		s.Batches, s.LinesSent, s.BytesSent = prev.Batches, prev.LinesSent, prev.BytesSent
		s.Accepted, s.Duplicate, s.Rejected, s.LinesSkipped = prev.Accepted, prev.Duplicate, prev.Rejected, prev.LinesSkipped
		s.LastSuccessAt = prev.LastSuccessAt
	}
	s.PID = os.Getpid()
	s.StartedAt = fw.opts.Now().UTC()
	s.State = StateRunning
	s.LogPath, s.BrainsrvURL, s.Context, s.Scope = fw.opts.LogPath, fw.opts.URL, fw.opts.Context, fw.scope
	fw.status = s
}

func (fw *Forwarder) saveStatus(force bool) {
	now := fw.opts.Now()
	if !force && now.Sub(fw.lastStatusSave) < 10*time.Second {
		return
	}
	fw.lastStatusSave = now
	fw.status.UpdatedAt = now.UTC()
	fw.status.Scope = fw.scope
	if err := writeStatus(fw.opts.StateDir, &fw.status); err != nil {
		fw.logf("could not write the status file: %v", err)
	}
}

func (fw *Forwarder) loop(ctx context.Context) error {
	var pendingSince time.Time
	for {
		if ctx.Err() != nil {
			return nil
		}
		b, err := fw.scan()
		if err != nil {
			if fw.opts.Once {
				fw.recordError(err)
				return err
			}
			fw.recordError(err)
			fw.logf("scan %s: %v", fw.opts.LogPath, err)
			if fw.wait(ctx, fw.opts.PollInterval) {
				return nil
			}
			continue
		}
		if len(b.entries) == 0 {
			pendingSince = time.Time{}
			fw.saveStatus(false)
			if fw.opts.Once {
				return nil
			}
			if fw.wait(ctx, fw.opts.PollInterval) {
				return nil
			}
			continue
		}
		if !b.hasData() {
			// Only blank or oversized lines: nothing to send, just move on.
			if err := fw.commit(b.entries, nil); err != nil {
				fw.recordError(err)
				if fw.opts.Once {
					return err
				}
				if fw.wait(ctx, fw.backoff()) {
					return nil
				}
			}
			continue
		}
		if !fw.opts.Once && !b.full {
			now := fw.opts.Now()
			if pendingSince.IsZero() {
				pendingSince = now
			}
			if rest := fw.opts.FlushInterval - now.Sub(pendingSince); rest > 0 {
				if fw.wait(ctx, min(rest, fw.opts.PollInterval)) {
					return nil
				}
				continue
			}
		}
		pendingSince = time.Time{}

		err = fw.deliver(ctx, b.entries)
		if err == nil {
			fw.failures = 0
			if fw.status.State != StateRunning {
				fw.logf("delivering again")
			}
			fw.status.State, fw.status.PausedReason, fw.status.NextRetryAt = StateRunning, "", time.Time{}
			fw.saveStatus(true)
			continue
		}
		if ctx.Err() != nil {
			return nil
		}
		var se *sendError
		if !errors.As(err, &se) {
			// Local failure (checkpoint write): not something a retry fixes
			// by itself, but not a reason to exit a service either.
			if fw.opts.Once {
				fw.recordError(err)
				return err
			}
			fw.recordError(err)
			if fw.wait(ctx, fw.backoff()) {
				return nil
			}
			continue
		}
		if se.kind == kindPause && se.reason == ReasonScopeDenied && fw.opts.Rescope != nil {
			if ns, rerr := fw.opts.Rescope(ctx); rerr == nil && ns != "" && ns != fw.scope {
				fw.logf("scope %s was denied; brainsrv now grants %s, switching", fw.scope, ns)
				fw.scope = ns
				continue
			}
		}
		fw.recordError(err)
		if se.kind == kindPause {
			fw.status.PausedReason = se.reason
		}
		if fw.opts.Once {
			return err
		}
		var d time.Duration
		if se.kind == kindPause {
			fw.status.State, fw.status.PausedReason = StatePaused, se.reason
			d = fw.opts.PauseRetry
			fw.logf("paused (%s): %v; retrying in %s", se.reason, err, d)
		} else {
			fw.status.State, fw.status.PausedReason = StateBackoff, ""
			d = fw.backoff()
			fw.logf("delivery failed: %v; retrying in %s", err, d.Round(time.Millisecond))
		}
		fw.status.NextRetryAt = fw.opts.Now().Add(d).UTC()
		fw.saveStatus(true)
		if fw.wait(ctx, d) {
			return nil
		}
	}
}

// wait sleeps d and reports whether ctx ended.
func (fw *Forwarder) wait(ctx context.Context, d time.Duration) bool {
	if err := fw.opts.Sleep(ctx, d); err != nil {
		return true
	}
	return ctx.Err() != nil
}

func (fw *Forwarder) recordError(err error) {
	fw.status.LastError = err.Error()
	fw.status.LastErrorAt = fw.opts.Now().UTC()
}

// backoff is exponential with jitter: half the step plus a random half,
// capped at MaxBackoff.
func (fw *Forwarder) backoff() time.Duration {
	step := fw.opts.BaseBackoff
	for i := 0; i < fw.failures && step < fw.opts.MaxBackoff; i++ {
		step *= 2
	}
	if step > fw.opts.MaxBackoff {
		step = fw.opts.MaxBackoff
	}
	fw.failures++
	return step/2 + time.Duration(fw.opts.Rand()*float64(step/2))
}

// deliver sends entries and commits them after a 200. A 413 splits the batch
// in half and delivers each half; a single line refused as too large is
// skipped.
func (fw *Forwarder) deliver(ctx context.Context, entries []entry) error {
	raw, lines := encode(entries)
	if lines == 0 {
		return fw.commit(entries, nil)
	}
	res, err := fw.post(ctx, raw)
	if err == nil {
		fw.status.Batches++
		fw.status.LinesSent += int64(lines)
		fw.status.BytesSent += int64(len(raw))
		fw.status.LastSuccessAt = fw.opts.Now().UTC()
		return fw.commit(entries, res)
	}
	var se *sendError
	if !errors.As(err, &se) || se.kind != kindTooLarge {
		return err
	}
	if lines == 1 {
		fw.logf("brainsrv refused a single %d-byte line as too large (413: %s); skipping it", len(raw), se.msg)
		for i := range entries {
			if entries[i].line != nil {
				entries[i].line, entries[i].skipped = nil, true
			}
		}
		return fw.commit(entries, nil)
	}
	// Split on the middle data line; blank entries ride with their half.
	half, seen := 0, 0
	for i, e := range entries {
		if e.line != nil {
			seen++
			if seen > lines/2 {
				half = i
				break
			}
		}
	}
	fw.logf("brainsrv refused a %d-line batch as too large (413); splitting it", lines)
	if err := fw.deliver(ctx, entries[:half]); err != nil {
		return err
	}
	return fw.deliver(ctx, entries[half:])
}

// commit moves the checkpoints past entries and persists them.
func (fw *Forwarder) commit(entries []entry, res *IngestResult) error {
	for _, e := range entries {
		cp := fw.state.Files[e.key]
		if cp == nil {
			continue
		}
		if e.end > cp.Offset {
			cp.Offset = e.end
		}
		if e.skipped {
			fw.status.LinesSkipped++
			fw.logf("skipped a line over %d bytes in %s ending at offset %d", fw.opts.MaxLineBytes, cp.Path, e.end)
		}
	}
	if res != nil {
		fw.status.Accepted += int64(res.Accepted)
		fw.status.Duplicate += int64(res.Duplicate)
		fw.status.Rejected += int64(res.Rejected)
		if res.Rejected > 0 {
			first := ""
			if len(res.Errors) > 0 {
				first = fmt.Sprintf(" (line %d: %s)", res.Errors[0].Line, res.Errors[0].Error)
			}
			fw.logf("brainsrv rejected %d line(s) of the batch%s", res.Rejected, first)
		}
	}
	if err := saveState(fw.opts.StateDir, fw.state, fw.opts.Now()); err != nil {
		return fmt.Errorf("save checkpoints: %w", err)
	}
	fw.refreshLag()
	return nil
}

func (fw *Forwarder) refreshLag() {
	fw.status.Lag, fw.status.LagBytes = nil, 0
	for _, cp := range fw.state.Files {
		if cp.Missing > 0 {
			continue
		}
		behind := cp.Size - cp.Offset
		if behind < 0 {
			behind = 0
		}
		fw.status.Lag = append(fw.status.Lag, FileLag{Path: cp.Path, Size: cp.Size, Offset: cp.Offset, Behind: behind})
		fw.status.LagBytes += behind
	}
	sortLag(fw.status.Lag)
}

// scan reconciles the checkpoints with the files on disk and reads the next
// batch.
func (fw *Forwarder) scan() (*batch, error) {
	views, err := openViews(fw.opts.LogPath)
	if err != nil {
		return nil, err
	}
	defer closeViews(views)
	fw.status.LogFound = len(views) > 0
	changed, err := fw.reconcile(views)
	if err != nil {
		return nil, err
	}
	if changed {
		if err := saveState(fw.opts.StateDir, fw.state, fw.opts.Now()); err != nil {
			return nil, fmt.Errorf("save checkpoints: %w", err)
		}
	}
	fw.refreshLag()
	b := &batch{}
	for _, v := range views {
		cp := fw.state.Files[v.key]
		if err := fw.readLines(v, cp.Offset, b); err != nil {
			return nil, fmt.Errorf("read %s: %w", v.path, err)
		}
		if b.full {
			break
		}
	}
	return b, nil
}

// reconcile places a checkpoint for every file, handles truncation and
// replaced files, and forgets files that rotated away.
func (fw *Forwarder) reconcile(views []*view) (bool, error) {
	st := fw.state
	changed := false
	present := map[string]bool{}
	for _, v := range views {
		present[v.key] = true
		cp := st.Files[v.key]
		if cp == nil {
			cp = &Checkpoint{Path: v.path}
			if !st.Initialized && !fw.opts.Backfill {
				// First run: start at the end, never mid-line.
				end, err := lastLineEnd(v.f, v.size)
				if err != nil {
					return false, err
				}
				cp.Offset = end
			}
			st.Files[v.key] = cp
			changed = true
		}
		if cp.Path != v.path || cp.Size != v.size || cp.Missing != 0 {
			cp.Path, cp.Size, cp.Missing = v.path, v.size, 0
			changed = true
		}
		if v.size < cp.Offset {
			fw.logf("%s shrank to %d bytes below the checkpoint %d (truncated); reading it again from the start", v.path, v.size, cp.Offset)
			cp.Offset, cp.HeadLen, cp.HeadSHA = 0, 0, ""
			changed = true
		}
		if cp.HeadLen > 0 && v.size >= cp.HeadLen {
			h, err := headHash(v.f, cp.HeadLen)
			if err != nil {
				return false, err
			}
			if h != cp.HeadSHA {
				fw.logf("%s was replaced (its first bytes changed); reading it again from the start", v.path)
				cp.Offset, cp.HeadLen, cp.HeadSHA = 0, 0, ""
				changed = true
			}
		}
		if n := min(cp.Offset, headMax); n > cp.HeadLen {
			h, err := headHash(v.f, n)
			if err != nil {
				return false, err
			}
			cp.HeadLen, cp.HeadSHA = n, h
			changed = true
		}
	}
	if !st.Initialized {
		st.Initialized = true
		changed = true
	}
	for k, cp := range st.Files {
		if present[k] {
			continue
		}
		cp.Missing++
		changed = true
		if cp.Missing >= 2 {
			if cp.Offset < cp.Size {
				fw.logf("%s rotated away with %d bytes never delivered", cp.Path, cp.Size-cp.Offset)
			}
			delete(st.Files, k)
		}
	}
	return changed, nil
}
