//go:build unix

package worker

import (
	"errors"
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"
)

type slowOutput struct{ data strings.Builder }

func (s *slowOutput) Write(p []byte) (int, error) {
	time.Sleep(100 * time.Millisecond)
	return s.data.Write(p)
}

func TestOwnedStdoutDrainsAfterSlowCallback(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "printf complete")
	out := &slowOutput{}
	pipe, err := newOwnedStdout(cmd, out, 10*time.Millisecond, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer pipe.close()
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	err = cmd.Wait()
	pipe.processExited()
	if copyErr := <-pipe.copied; err != nil || copyErr != nil || out.data.String() != "complete" {
		t.Fatalf("%v %v %q", err, copyErr, out.data.String())
	}
}

func TestOwnedStdoutBoundsInheritedPipe(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "sleep 1 & exit 0")
	pipe, err := newOwnedStdout(cmd, io.Discard, 10*time.Millisecond, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer pipe.close()
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err = cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	pipe.processExited()
	if err = <-pipe.copied; !errors.Is(err, exec.ErrWaitDelay) {
		t.Fatalf("%v", err)
	}
}

func TestHostCallbacksRetainLargeAndUnterminatedEvents(t *testing.T) {
	w := newActivityWriter(nil)
	w.callbackLimit, w.lineLimit, w.callbackTail, w.strictCallbacks = 1<<20, 1<<20, true, true
	var seen []string
	w.onLine = func(p []byte) error { seen = append(seen, string(p)); return nil }
	large := strings.Repeat("x", 300<<10)
	if _, err := w.Write([]byte(large + "\ntail")); err != nil {
		t.Fatal(err)
	}
	if err := w.finish(); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || seen[0] != large || seen[1] != "tail" {
		t.Fatal("event lost")
	}
	if _, err := w.Write([]byte(strings.Repeat("x", (1<<20)+1))); err == nil {
		t.Fatal("oversized host event silently dropped")
	}
}
