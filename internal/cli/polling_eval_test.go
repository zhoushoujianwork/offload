package cli

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/zhoushoujianwork/offload/internal/live"
)

// This file is a reproducible, headless comparison of how a parent agent can wait for one
// long-running worker. A "parent turn" is one offload CLI invocation made on the parent's behalf
// (status or watch); the harness's own waiting for the worker to start is not counted. Timings
// are scaled down so the comparison runs in CI, so only the turn counts and the visibility of
// outcomes are meaningful, not the absolute seconds.
//
// Run it with: go test ./internal/cli -run TestPollingCoordinationComparison -v
//
// It measures parent polling turns, elapsed time, exit codes, and whether the failure or stall
// was visible. It does not measure parent or worker tokens (cached or noncached) or cost; those
// are reported as unavailable, and nothing here supports a token or cost saving claim.

var outcomeLine = regexp.MustCompile(`(?:done|finished) status=(\w+) exit_code=(\d+)`)

type armResult struct {
	arm       string
	turns     int
	elapsed   time.Duration
	exitCodes []int
	outcome   string // status=... exit_code=... as the parent saw it, "" if never seen
	stalled   bool
	output    string
}

func (r armResult) row() string {
	return fmt.Sprintf("%-28s turns=%d elapsed=%.1fs exit_codes=%v outcome_seen=%q stalled_seen=%v parent_tokens=unavailable worker_tokens=unavailable cost=unknown",
		r.arm, r.turns, r.elapsed.Seconds(), r.exitCodes, r.outcome, r.stalled)
}

// launchWorker runs a worker in the background and returns once it has published an active state.
func launchWorker(t *testing.T, work string, args ...string) <-chan struct{} {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); runCLI(append([]string{"-d", work}, args...)...) }()
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		states, _ := live.List()
		for _, s := range states {
			if s.Active() {
				return done
			}
		}
	}
	t.Fatal("worker never published an active state")
	return nil
}

// parentLoop calls step as the parent would, once per turn, until the outcome is seen.
func parentLoop(t *testing.T, arm string, done <-chan struct{}, pause time.Duration, step func() (int, string)) armResult {
	t.Helper()
	r := armResult{arm: arm}
	begin := time.Now()
	for r.turns < 200 {
		code, out := step()
		r.turns++
		r.exitCodes = append(r.exitCodes, code)
		r.output += out
		r.stalled = r.stalled || strings.Contains(out, "STALLED")
		if m := outcomeLine.FindStringSubmatch(out); m != nil {
			r.outcome = fmt.Sprintf("status=%s exit_code=%s", m[1], m[2])
			break
		}
		time.Sleep(pause)
	}
	r.elapsed = time.Since(begin)
	<-done
	return r
}

func shortPoll(t *testing.T, work string, every time.Duration, args ...string) armResult {
	done := launchWorker(t, work, args...)
	return parentLoop(t, fmt.Sprintf("short poll (status/%s)", every), done, every, func() (int, string) {
		code, out, _ := runCLI("status")
		return code, out
	})
}

func singleWatch(t *testing.T, work string, args ...string) armResult {
	done := launchWorker(t, work, args...)
	return parentLoop(t, "one long watch (-t 30s)", done, 0, func() (int, string) {
		code, out, _ := runCLI("watch", "-t", "30s")
		return code, out
	})
}

// boundedWatches re-issues a short bounded watch while it returns 124, as a host with a
// responsiveness limit would.
func boundedWatches(t *testing.T, work string, bound string, args ...string) armResult {
	done := launchWorker(t, work, args...)
	return parentLoop(t, "bounded watches (-t "+bound+")", done, 0, func() (int, string) {
		code, out, _ := runCLI("watch", "-t", bound)
		return code, out
	})
}

func TestPollingCoordinationComparison(t *testing.T) {
	fastWatch(t)
	watchEvery = 100 * time.Millisecond
	heartbeat = time.Hour

	t.Run("long task", func(t *testing.T) {
		work, _ := setup(t, okReply)
		t.Setenv("FAKE_SLEEP", "3")
		poll := shortPoll(t, work, 400*time.Millisecond, "long task")
		one := singleWatch(t, work, "long task")
		bounded := boundedWatches(t, work, "1s", "long task")
		for _, r := range []armResult{poll, one, bounded} {
			t.Log(r.row())
			if r.outcome != "status=ok exit_code=0" {
				t.Errorf("%s: parent never saw the outcome: %q\n%s", r.arm, r.outcome, r.output)
			}
		}
		if one.turns != 1 {
			t.Errorf("a single long watch should need one parent turn, got %d", one.turns)
		}
		if one.turns >= poll.turns || bounded.turns >= poll.turns {
			t.Errorf("waiting should take fewer parent turns than short polling: poll=%d watch=%d bounded=%d", poll.turns, one.turns, bounded.turns)
		}
		// A bounded wait that ends with workers still active is not a worker failure.
		if bounded.turns > 1 && bounded.exitCodes[0] != 124 {
			t.Errorf("bounded watch with an active worker should exit 124, got %v", bounded.exitCodes)
		}
	})

	t.Run("failed worker stays visible", func(t *testing.T) {
		work, _ := setup(t, `{"type":"result","result":"API Error: 401","is_error":true,"session_id":"s-e"}`)
		t.Setenv("FAKE_SLEEP", "1")
		for _, r := range []armResult{
			shortPoll(t, work, 300*time.Millisecond, "failing task"),
			singleWatch(t, work, "failing task"),
		} {
			t.Log(r.row())
			if r.outcome != "status=error exit_code=1" {
				t.Errorf("%s hid the failure: %q\n%s", r.arm, r.outcome, r.output)
			}
		}
	})

	t.Run("timeout stays visible", func(t *testing.T) {
		work, _ := setup(t, okReply)
		t.Setenv("FAKE_PREFIX", `{"type":"system","subtype":"init","session_id":"s-to"}`)
		t.Setenv("FAKE_SLEEP", "8")
		t.Setenv("FAKE_SALVAGE", "PARTIAL")
		r := singleWatch(t, work, "-t", "1s", "slow task")
		t.Log(r.row())
		if r.outcome != "status=timeout exit_code=124" {
			t.Errorf("watch hid the timeout: %q\n%s", r.outcome, r.output)
		}
	})

	t.Run("stall stays visible", func(t *testing.T) {
		stallAfter = 700 * time.Millisecond
		work, _ := setup(t, okReply)
		t.Setenv("FAKE_PREFIX", `{"type":"system","subtype":"init","session_id":"s-stall"}`)
		t.Setenv("FAKE_SLEEP", "3")
		r := singleWatch(t, work, "quiet task")
		t.Log(r.row())
		if !r.stalled || r.outcome != "status=ok exit_code=0" {
			t.Errorf("a single long watch must still report the stall and the end: stalled=%v outcome=%q\n%s", r.stalled, r.outcome, r.output)
		}
	})
}

// A worker that ends between two bounded watches must still be reported by the next one.
func TestWatchReportsWorkerThatFinishedBetweenBoundedWatches(t *testing.T) {
	work, _ := setup(t, okReply)
	if code, _, e := runCLI("-d", work, "quick"); code != 0 {
		t.Fatal(e)
	}
	code, out, _ := runCLI("watch", "-t", "5s")
	if code != 0 || !strings.Contains(out, "recently finished status=ok exit_code=0") || !strings.Contains(out, "no active workers") {
		t.Fatalf("outcome lost between watches: code=%d\n%s", code, out)
	}
}
