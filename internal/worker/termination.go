package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/zhoushoujianwork/offload/internal/history"
	"github.com/zhoushoujianwork/offload/internal/proc"
)

// classifyTermination explains why a worker ended without a result, or returns nil when the end
// is already described by another stop reason (idle limit, timeout, event-log failure) or the
// worker was not signalled at all.
//
//   - canceled: the stop was requested, by Ctrl-C (SIGINT) or by cancelling the caller's context.
//   - interrupted: offload received SIGTERM, the signal a host shutdown, service stop, upgrade or
//     supervising wrapper sends, or a shutdown signal reached the worker directly. The session is
//     intact and can be resumed.
//   - killed: the worker was ended by a signal nobody asked offload to relay. SIGKILL that offload
//     did not send is what the kernel's out-of-memory killer and a forced shutdown deliver.
//
// session is the session a resume would continue; the resume command is set only when it is known.
func classifyTermination(a attempt, session string) *history.Termination {
	if errors.Is(a.cause, errIdle) || errors.Is(a.cause, context.DeadlineExceeded) || errors.Is(a.cause, errEventsIO) {
		return nil
	}
	workerSig, signalled := proc.ExitSignal(a.runErr)
	t := &history.Termination{WorkerSignal: workerSig}
	switch {
	case errors.Is(a.cause, ErrSignal):
		t.SupervisorRequested = true
		t.Class = history.ClassInterrupted
		var se *SignalError
		if errors.As(a.cause, &se) && se.Signal != nil {
			t.Signal = proc.SignalName(se.Signal)
			if se.Signal == os.Interrupt {
				t.Class = history.ClassCanceled
			}
		}
	case errors.Is(a.cause, context.Canceled):
		t.SupervisorRequested = true
		t.Class = history.ClassCanceled
	case a.cause == nil && signalled:
		// Nobody asked offload to stop the worker, so the signal came from outside.
		switch workerSig {
		case "SIGTERM", "SIGHUP", "SIGINT":
			t.Class = history.ClassInterrupted
		default:
			t.Class = history.ClassKilled
			t.SystemKilled = workerSig == "SIGKILL"
		}
	default:
		return nil
	}
	t.GraceExpired = t.SupervisorRequested && a.forceKilled
	if session != "" {
		t.ResumeCommand = resumeCommand(session)
	}
	return t
}

// describeTermination is the sentence added to a run's error text.
func describeTermination(t *history.Termination) string {
	var s string
	switch {
	case t.Class == history.ClassCanceled && t.Signal != "":
		s = fmt.Sprintf("canceled: stop requested (%s)", t.Signal)
	case t.Class == history.ClassCanceled:
		s = "canceled: stop requested"
	case t.Class == history.ClassInterrupted && t.Signal != "":
		s = fmt.Sprintf("interrupted: offload received %s (host or supervisor shutdown) and stopped the worker", t.Signal)
	case t.Class == history.ClassInterrupted:
		s = fmt.Sprintf("interrupted: the worker received %s from outside offload (host or supervisor shutdown)", t.WorkerSignal)
	case t.SystemKilled:
		s = "killed: the worker was ended by SIGKILL that offload did not send (for example the out-of-memory killer)"
	default:
		s = fmt.Sprintf("killed: the worker was ended by %s that offload did not send", t.WorkerSignal)
	}
	if t.GraceExpired {
		s += fmt.Sprintf("; it did not exit within the %s grace period and was force-killed", StopGrace.Round(time.Millisecond))
	}
	return s
}
