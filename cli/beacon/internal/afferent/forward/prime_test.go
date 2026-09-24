package forward

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// History appended after Prime (afferent sync before the forwarder ever
// ran) is new data and is delivered; what was there before is not.
func TestPrimeThenBackfilledHistoryIsDelivered(t *testing.T) {
	e := newEnv(t)
	e.write(e.log, 3) // before afferent: not sent
	placed, err := Prime(e.state, e.log)
	if err != nil || !placed || !Primed(e.state, e.log) {
		t.Fatalf("Prime %v %v", placed, err)
	}
	if placed, err := Prime(e.state, e.log); err != nil || placed {
		t.Fatalf("second Prime %v %v", placed, err)
	}
	synced := e.write(e.log, 4)
	e.mustRun(e.opts())
	e.wantIDs(synced)
}

func TestPrimeRefusesWhileAForwarderRuns(t *testing.T) {
	e := newEnv(t)
	if err := os.MkdirAll(e.state, 0o700); err != nil {
		t.Fatal(err)
	}
	release, ok, err := tryLock(filepath.Join(e.state, LockFileName))
	if err != nil || !ok {
		t.Fatalf("lock %v %v", ok, err)
	}
	defer release()
	if _, err := Prime(e.state, e.log); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("got %v", err)
	}
}

// A forwarder started while signed out places its checkpoints before it
// waits, so events written while it waits are delivered after sign-in.
func TestSignedOutForwarderPlacesCheckpointsFirst(t *testing.T) {
	e := newEnv(t)
	e.write(e.log, 2) // history before the service started
	e.tokens.set("tok-1", nil, nil)
	o := e.opts()
	o.Scope = ""
	calls := 0
	var during []string
	o.Rescope = func(context.Context) (string, error) {
		calls++
		if calls == 1 {
			if !Primed(e.state, e.log) {
				t.Error("checkpoints not placed before waiting for the scope")
			}
			during = e.write(e.log, 2) // captured while signed out
			return "", errLogin
		}
		return "ws.t.people.u.harness", nil
	}
	_, err := e.loop(o, func(i int, d time.Duration) bool { return i > 3 && len(e.brain.ids()) >= 2 })
	if err != nil {
		t.Fatal(err)
	}
	e.wantIDs(during)
}
