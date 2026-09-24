package history

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/codexsession"
	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/endpoint/writer"
)

// errNoRoom stops a Codex sweep that would rotate undelivered data out of
// the runtime log.
var errNoRoom = errors.New("the runtime log has no room for more history until the forwarder sends what it holds")

// codexSweep is codexsession.CollectOnce (same store, mapping, cursor file
// and writer) with a limit: Beacon's Codex collector has no retention guard,
// so a large history (or one written after a Claude sweep filled the log)
// would rotate unsent history out of the log while the cursors already say
// it was collected. Before each event it checks room (bytes the log can take
// before a rotation deletes undelivered data) and stops, leaving the rest
// for the next sweep, when the event does not fit.
func codexSweep(dir, statePath, logPath string, userMode bool, room func() (int64, error), rep *Report) error {
	store, err := codexsession.NewStore(dir)
	if err != nil {
		return err
	}
	refs, err := store.List()
	if err != nil {
		return err
	}
	rep.Sessions = len(refs)
	if len(refs) == 0 {
		return nil
	}
	state, err := codexsession.LoadState(statePath)
	if err != nil {
		return err
	}
	var left int64 = -1 // unknown until the first event
	fits := func(size int64) (bool, error) {
		if room == nil {
			return true, nil
		}
		if left >= size {
			return true, nil
		}
		// Ask again: the forwarder may have sent some meanwhile.
		r, err := room()
		if err != nil {
			return false, err
		}
		left = r
		return left >= size, nil
	}
	var errs []error
	for i, ref := range refs {
		changed, err := sweepCodexSession(store, ref, state, logPath, userMode, fits, &left, rep)
		if changed {
			rep.SessionsChanged++
		}
		if errors.Is(err, errNoRoom) {
			rep.RetentionLimited = true
			rep.SessionsPending = 1 + codexChanged(refs[i+1:], state)
			break
		}
		if err != nil {
			rep.Errors++
			errs = append(errs, fmt.Errorf("Codex session %s: %w", ref.ID, err))
		}
	}
	if err := state.Save(statePath); err != nil {
		errs = append(errs, fmt.Errorf("save Codex collector state: %w", err))
	}
	return errors.Join(errs...)
}

func sweepCodexSession(store *codexsession.Store, ref codexsession.SessionRef, state *codexsession.State, logPath string, userMode bool,
	fits func(int64) (bool, error), left *int64, rep *Report) (bool, error) {
	if state.Files == nil {
		state.Files = map[string]*codexsession.Cursor{}
	}
	cursor := state.Files[ref.Path]
	if cursor == nil {
		cursor = &codexsession.Cursor{}
		state.Files[ref.Path] = cursor
	}
	if ref.SizeBytes < cursor.SizeBytes {
		cursor.LastLine = 0
		cursor.Started = false
	}
	if ref.SizeBytes == cursor.SizeBytes && ref.ModTimeUnixMS == cursor.ModTimeUnixMS {
		return false, nil
	}
	records, stats, err := store.Read(ref)
	if err != nil {
		return false, err
	}
	mapped := codexsession.MapSession(ref, records, codexsession.MapOptions{MinLine: cursor.LastLine, SkipSessionStarted: cursor.Started})
	if len(mapped) == 0 {
		advanceCodex(cursor, ref, stats.Lines)
		return false, nil
	}
	for i, item := range mapped {
		size := int64(writer.MaxEventBytes)
		if b, err := json.Marshal(item.Event); err == nil && len(b)+1 < writer.MaxEventBytes {
			size = int64(len(b) + 1)
		}
		ok, err := fits(size)
		if err == nil && !ok {
			err = errNoRoom
		}
		if err == nil {
			_, err = writer.AppendEvent(item.Event, writer.Options{Path: logPath, UserMode: userMode})
		}
		if err != nil {
			partialCodex(cursor, mapped, i)
			return i > 0, err
		}
		*left -= size
		rep.Events++
		if item.Event.Event.Action == "session.started" {
			cursor.Started = true
		}
	}
	advanceCodex(cursor, ref, stats.Lines)
	return true, nil
}

func advanceCodex(c *codexsession.Cursor, ref codexsession.SessionRef, lines int) {
	c.LastLine, c.SizeBytes, c.ModTimeUnixMS = lines, ref.SizeBytes, ref.ModTimeUnixMS
}

// partialCodex moves the cursor past the source lines whose events were all
// written before mapped[failed], so the next sweep starts at the first line
// with an unwritten event (a line can map to several events).
func partialCodex(c *codexsession.Cursor, mapped []codexsession.MappedEvent, failed int) {
	for i := failed - 1; i >= 0; i-- {
		if mapped[i].SourceLine != mapped[failed].SourceLine {
			c.LastLine = mapped[i].SourceLine
			return
		}
	}
}

// codexChanged counts refs with records the cursors have not reached.
func codexChanged(refs []codexsession.SessionRef, state *codexsession.State) int {
	n := 0
	for _, ref := range refs {
		c := state.Files[ref.Path]
		if c == nil || ref.SizeBytes != c.SizeBytes || ref.ModTimeUnixMS != c.ModTimeUnixMS {
			n++
		}
	}
	return n
}
