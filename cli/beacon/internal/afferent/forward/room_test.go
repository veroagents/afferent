package forward

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/endpoint/writer"
)

func TestRoomCountsRotationsBeforeUndeliveredData(t *testing.T) {
	const (
		R = int64(writer.DefaultRotateBytes)
		A = writer.DefaultRotateArchives
		M = int64(writer.MaxEventBytes)
	)
	want := func(live int64, oldest int) int64 {
		n := int64(A - oldest)
		return max((R-live)+n*R-n*M-roomMargin, 0)
	}
	e := newEnv(t)
	room := func() int64 {
		t.Helper()
		r, err := Room(e.state, e.log)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	size := func(p string) int64 {
		fi, err := os.Stat(p)
		if err != nil {
			return 0
		}
		return fi.Size()
	}

	// Nothing yet: all of it, less the margins.
	if got := room(); got != want(0, 0) {
		t.Fatalf("empty: %d want %d", got, want(0, 0))
	}
	// Everything delivered: only the live file's size counts.
	e.primeEmpty()
	e.write(e.log, 3)
	e.mustRun(e.opts())
	if got := room(); got != want(size(e.log), 0) {
		t.Fatalf("delivered: %d want %d", got, want(size(e.log), 0))
	}
	// Undelivered lines rotated to .2: two fewer rotations of room.
	e.write(e.log, 2)
	e.rotate()
	e.write(e.log, 1)
	e.rotate()
	e.write(e.log, 1)
	if got := room(); got != want(size(e.log), 2) {
		t.Fatalf("undelivered in .2: %d want %d", got, want(size(e.log), 2))
	}
	// Delivered again: back to the full window.
	e.mustRun(e.opts())
	if got := room(); got != want(size(e.log), 0) {
		t.Fatalf("drained: %d want %d", got, want(size(e.log), 0))
	}
	// Undelivered in the oldest archive: only until the next rotation.
	for i := 0; i < A; i++ {
		e.write(e.log, 1)
		e.rotate()
	}
	e.write(e.log, 1)
	if got := room(); got != want(size(e.log), A) || got >= R {
		t.Fatalf("undelivered in .%d: %d want %d", A, got, want(size(e.log), A))
	}
}

// Following another log for a while must not throw away this log's
// checkpoints: switching back resumes, it does not start at the end.
func TestCheckpointsSurviveALogSwitch(t *testing.T) {
	e := newEnv(t)
	e.primeEmpty()
	first := e.write(e.log, 2)
	e.mustRun(e.opts())

	other := filepath.Join(e.dir, "system", "runtime.jsonl")
	if err := os.MkdirAll(filepath.Dir(other), 0o755); err != nil {
		t.Fatal(err)
	}
	e.write(other, 1)
	if _, err := Prime(e.state, other); err != nil { // e.g. `afferent sync --system`
		t.Fatal(err)
	}
	o := e.opts()
	o.LogPath = other
	e.mustRun(o)

	// Written to the first log while the other one was followed.
	missed := e.write(e.log, 3)
	e.mustRun(e.opts())
	e.wantIDs(append(first, missed...))

	// And back again: the other log's checkpoints were kept too.
	later := e.write(other, 1)
	e.mustRun(o)
	e.wantIDs(append(append(first, missed...), later...))
}
