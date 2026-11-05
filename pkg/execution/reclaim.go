package execution

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/zhoushoujianwork/offload/internal/proc"
	"github.com/zhoushoujianwork/offload/internal/slots"
	"github.com/zhoushoujianwork/offload/internal/worker"
)

// ReclaimOutcome says what ReclaimRun established about a run.
type ReclaimOutcome string

const (
	// ReclaimTerminated: leftover native processes were found and are now gone.
	ReclaimTerminated ReclaimOutcome = "terminated"
	// ReclaimAlreadyEnded: nothing was left to stop. Repeating the call returns this too.
	ReclaimAlreadyEnded ReclaimOutcome = "already_ended"
	// ReclaimUnconfirmed: the run's processes could not be confirmed gone. The
	// call also returns an error, and the worker slot stays unresolved.
	ReclaimUnconfirmed ReclaimOutcome = "unconfirmed"
)

var (
	// ErrRunStillOwned means the invoking process of the run is still alive.
	ErrRunStillOwned = errors.New("run is still owned by a live caller; stop it through that caller")
	// ErrRunNotFound means no worker slot records this run ID, so its state
	// cannot be established. This is not success.
	ErrRunNotFound = errors.New("no worker slot records this run; its native processes cannot be confirmed")
)

// ReclaimRun ends the leftover native process group of a run whose caller no
// longer exists (crash, kill -9, OOM), using the run ID from the start event.
// It needs no profile or credentials and does not depend on the original caller.
//
// The recorded group is signalled only while its leader's start time still
// matches the record, so a recycled PID or process group ID is never
// signalled. The group gets SIGTERM, then SIGKILL after the stop grace period,
// and the call returns only after the group is confirmed gone. On success the
// worker slot is released from its unresolved state. The call is idempotent.
// Processes that left the native process group and session are not tracked.
//
// It returns ErrRunStillOwned while the caller is alive and ErrRunNotFound when
// the run is unknown (including runs started with worker admission disabled).
func ReclaimRun(ctx context.Context, runID string) (ReclaimOutcome, error) {
	claim, err := slots.ClaimRun(slots.Dir(), runID)
	switch {
	case errors.Is(err, slots.ErrRunActive):
		return ReclaimUnconfirmed, ErrRunStillOwned
	case errors.Is(err, slots.ErrRunUnknown):
		return ReclaimUnconfirmed, ErrRunNotFound
	case err != nil:
		return ReclaimUnconfirmed, err
	}
	g := claim.Group()
	if g == nil || !g.Alive() {
		claim.Finish(true)
		return ReclaimAlreadyEnded, nil
	}
	gone := stopRecorded(ctx, *g)
	claim.Finish(gone)
	if !gone {
		return ReclaimUnconfirmed, fmt.Errorf("native process group %d is still alive after SIGKILL", g.PGID)
	}
	return ReclaimTerminated, nil
}

func stopRecorded(ctx context.Context, g proc.Group) bool {
	wait := func(d time.Duration) bool {
		deadline := time.Now().Add(d)
		for g.Alive() {
			if ctx.Err() != nil || time.Now().After(deadline) {
				return !g.Alive()
			}
			time.Sleep(20 * time.Millisecond)
		}
		return true
	}
	g.Term()
	if wait(worker.StopGrace) {
		return true
	}
	g.Kill()
	return wait(worker.KillWait)
}
