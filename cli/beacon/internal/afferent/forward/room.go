package forward

import (
	"errors"
	"os"

	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/endpoint/writer"
)

// roomMargin is kept free for the hooks and the collector, which append to
// the same log while a backfill runs.
const roomMargin = 1 << 20

// Room is about how many bytes can be appended to logPath before a rotation
// would delete a file that still holds data the forwarder (with checkpoints
// in stateDir) has not delivered. A backfill (afferent sync) writes at most
// this much, then lets the forwarder drain: Beacon's session cursors mark
// history collected once it is in the log, so history rotated out before it
// is sent is lost for good.
//
// The log keeps the live file and writer.DefaultRotateArchives archives, and
// each rotation (when the live file would pass writer.DefaultRotateBytes)
// deletes the oldest. A file now at index k (live = 0, .1 = 1, …) survives
// archives-k more rotations. What is written from now on is itself
// undelivered and starts in the live file, so k is never taken below 0.
func Room(stateDir, logPath string) (int64, error) {
	st, err := loadState(stateDir, logPath)
	if err != nil {
		return 0, err
	}
	const (
		rotate   = int64(writer.DefaultRotateBytes)
		archives = writer.DefaultRotateArchives
	)
	var live int64
	oldest := 0
	for i, p := range writer.RetainedLogPaths(logPath) {
		fi, err := os.Stat(p)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return 0, err
		}
		if i == 0 {
			live = fi.Size()
		}
		key, err := fileKey(fi)
		if err != nil {
			return 0, err
		}
		cp := st.Files[key]
		// A file the checkpoints do not know is new data once they are
		// initialized (before that, a first run starts past it).
		undelivered := (cp == nil && st.Initialized) || (cp != nil && cp.Offset < fi.Size())
		if undelivered && i > oldest {
			oldest = i
		}
	}
	n := int64(archives - oldest)
	// Each rotation can leave up to one event's worth of a file unused.
	room := (rotate - live) + n*rotate - n*int64(writer.MaxEventBytes) - roomMargin
	return max(room, 0), nil
}
