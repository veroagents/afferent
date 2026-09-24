// Package history backfills existing Claude Code and Codex session history
// into the Beacon runtime log, so the forwarder ships it (PLAN D7, `afferent
// sync`). It runs Beacon's own collectors (claudesession / codexsession
// CollectOnce, the code behind `beacon endpoint claude sync` and `beacon
// endpoint codex sync`) by import.
//
// Cursors: it uses Beacon's own cursor files (~/.beacon/endpoint/state/
// claude.json and codex.json in user mode), not afferent-owned ones. The
// runtime log is shared with Beacon, so separate cursors would map and write
// every session a second time whenever the user also runs Beacon's sync; with
// one set of cursors a session is written to the log once, whoever sweeps.
package history

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/claudesession"
	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/codexsession"
	endpointconfig "github.com/asymptote-labs/agent-beacon/cli/beacon/internal/endpoint/config"
	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/endpoint/writer"
)

// Harnesses whose history can be synced.
const (
	Claude = "claude"
	Codex  = "codex"
)

// All lists them.
var All = []string{Claude, Codex}

// Options for Sync.
type Options struct {
	Home     string // the user's home (sessions and cursors live under it)
	LogPath  string // the Beacon runtime log to append to
	UserMode bool
	// Since, when > 0, skips sessions last written longer ago than this.
	// They are marked collected in the cursor file, so they are not
	// backfilled later either (and a resumed one continues from its end).
	Since time.Duration
	Now   func() time.Time
}

// Report is one harness's sweep.
type Report struct {
	Harness          string `json:"harness"`
	StatePath        string `json:"state_path"`
	Sessions         int    `json:"sessions"`
	SessionsChanged  int    `json:"sessions_changed"`
	Events           int    `json:"events"`
	SkippedOld       int    `json:"skipped_old"`
	Errors           int    `json:"errors"`
	RetentionLimited bool   `json:"retention_limited"`
	SessionsPending  int    `json:"sessions_pending"`
}

// StatePath is Beacon's cursor file for harness h, as `beacon endpoint <h>
// sync` resolves it by default.
func StatePath(home, h string, userMode bool) string {
	if userMode {
		return filepath.Join(home, ".beacon", "endpoint", "state", h+".json")
	}
	return filepath.Join(endpointconfig.BaseDir(false), "state", h+".json")
}

// Present reports whether h has a session directory under home.
func Present(home, h string) bool {
	switch h {
	case Claude:
		s, err := claudesession.NewStore(filepath.Join(home, ".claude", "projects"))
		return err == nil && s.Exists()
	case Codex:
		s, err := codexsession.NewStore(filepath.Join(home, ".codex"))
		return err == nil && s.Exists()
	}
	return false
}

// Sync runs one sweep for h. A sweep that stopped because writing more would
// rotate its own output out of the log reports RetentionLimited (with
// SessionsPending) and no error: run it again after the forwarder drains.
func Sync(h string, o Options) (Report, error) {
	if o.LogPath == "" || o.Home == "" {
		return Report{}, errors.New("history: Home and LogPath are required")
	}
	rep := Report{Harness: h, StatePath: StatePath(o.Home, h, o.UserMode)}
	switch h {
	case Claude:
		dir := filepath.Join(o.Home, ".claude", "projects")
		if o.Since > 0 {
			n, err := markOldClaude(dir, rep.StatePath, cutoff(o))
			if err != nil {
				return rep, err
			}
			rep.SkippedOld = n
		}
		s, err := claudesession.CollectOnce(claudesession.CollectOptions{
			ProjectsDir: dir, StatePath: rep.StatePath, Write: true, LogPath: o.LogPath, UserMode: o.UserMode, Out: io.Discard,
		})
		rep.Sessions, rep.SessionsChanged, rep.Events, rep.Errors = s.Sessions, s.SessionsChanged, s.EventsEmitted, s.Errors
		rep.RetentionLimited, rep.SessionsPending = s.RetentionLimited, s.SessionsPending
		return rep, retentionOK(err, s.RetentionLimited)
	case Codex:
		dir := filepath.Join(o.Home, ".codex")
		if o.Since > 0 {
			n, err := markOldCodex(dir, rep.StatePath, cutoff(o))
			if err != nil {
				return rep, err
			}
			rep.SkippedOld = n
		}
		s, err := codexsession.CollectOnce(codexsession.CollectOptions{
			CodexDir: dir, StatePath: rep.StatePath, Write: true, LogPath: o.LogPath, UserMode: o.UserMode, Out: io.Discard,
		})
		rep.Sessions, rep.SessionsChanged, rep.Events, rep.Errors = s.Sessions, s.SessionsChanged, s.EventsEmitted, s.Errors
		// The Codex collector has no retention guard of its own.
		return rep, err
	}
	return rep, fmt.Errorf("unsupported harness %q (supported: claude, codex)", h)
}

// retentionOK drops the "stopped at the retention window" error, which
// RetentionLimited reports; other errors joined with it stay.
func retentionOK(err error, limited bool) error {
	if err == nil || !limited {
		return err
	}
	var rest []error
	var walk func(error)
	walk = func(e error) {
		if j, ok := e.(interface{ Unwrap() []error }); ok {
			for _, x := range j.Unwrap() {
				walk(x)
			}
			return
		}
		if !errors.Is(e, writer.ErrRetentionWindowFull) {
			rest = append(rest, e)
		}
	}
	walk(err)
	return errors.Join(rest...)
}

func cutoff(o Options) int64 {
	now := time.Now
	if o.Now != nil {
		now = o.Now
	}
	return now().Add(-o.Since).UnixMilli()
}

// markOldClaude marks sessions not written since cutoff, and not yet in the
// cursor file, as collected up to their current end.
func markOldClaude(dir, statePath string, cutoff int64) (int, error) {
	store, err := claudesession.NewStore(dir)
	if err != nil {
		return 0, err
	}
	refs, err := store.List()
	if err != nil {
		return 0, err
	}
	state, err := claudesession.LoadState(statePath)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, ref := range refs {
		if ref.ModTimeUnixMS >= cutoff || state.Files[ref.Path] != nil {
			continue
		}
		_, stats, err := store.Read(ref)
		if err != nil {
			continue // left for the sweep, which reports it
		}
		state.Files[ref.Path] = &claudesession.Cursor{LastLine: stats.Lines, SizeBytes: ref.SizeBytes, ModTimeUnixMS: ref.ModTimeUnixMS, Started: true}
		n++
	}
	if n == 0 {
		return 0, nil
	}
	return n, state.Save(statePath)
}

func markOldCodex(dir, statePath string, cutoff int64) (int, error) {
	store, err := codexsession.NewStore(dir)
	if err != nil {
		return 0, err
	}
	refs, err := store.List()
	if err != nil {
		return 0, err
	}
	state, err := codexsession.LoadState(statePath)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, ref := range refs {
		if ref.ModTimeUnixMS >= cutoff || state.Files[ref.Path] != nil {
			continue
		}
		_, stats, err := store.Read(ref)
		if err != nil {
			continue
		}
		state.Files[ref.Path] = &codexsession.Cursor{LastLine: stats.Lines, SizeBytes: ref.SizeBytes, ModTimeUnixMS: ref.ModTimeUnixMS, Started: true}
		n++
	}
	if n == 0 {
		return 0, nil
	}
	return n, state.Save(statePath)
}
