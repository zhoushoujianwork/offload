package runcontrol

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhoushoujianwork/offload/internal/history"
)

func TestStopIsAuthenticatedAndIsolated(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	a, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	b, cancelB := context.WithCancel(context.Background())
	defer cancelB()
	first, err := Register(Info{RunID: strings.Repeat("a", 32)}, cancelA)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Finish(history.Record{Status: "error"})
	second, err := Register(Info{RunID: strings.Repeat("b", 32)}, cancelB)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Finish(history.Record{Status: "ok"})
	m, _ := Read(first.m.RunID)
	response, err := http.Post("http://"+m.Address+"/stop", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 403 || a.Err() != nil {
		t.Fatal("unauthenticated stop accepted")
	}
	if err := Stop(t.Context(), first.m.RunID); err != nil {
		t.Fatal(err)
	}
	if a.Err() != context.Canceled || b.Err() != nil {
		t.Fatal("stop crossed execution boundaries")
	}
	m, _ = Read(first.m.RunID)
	if m.Phase != "stopping" || !m.Active() {
		t.Fatal(m.Phase)
	}
	first.Finish(history.Record{Status: "error", ErrorKind: "canceled", Session: "native"})
	m, _ = Read(first.m.RunID)
	if m.Active() || m.Status != "canceled" || m.Session != "native" {
		t.Fatal(m)
	}
	if err := Stop(t.Context(), first.m.RunID); err == nil {
		t.Fatal("terminal run accepted stop")
	}
}

func TestResumeLeaseAndLineage(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	id, next := strings.Repeat("c", 32), strings.Repeat("d", 32)
	r, err := Register(Info{RunID: id}, func() {})
	if err != nil {
		t.Fatal(err)
	}
	r.Finish(history.Record{Status: "error", ErrorKind: "canceled", Session: "native"})
	release, err := Lease(id)
	if err != nil {
		t.Fatal(err)
	}
	if other, err := Lease(id); err == nil {
		other()
		t.Fatal("duplicate lease accepted")
	}
	if err := MarkResuming(id, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	release()
	child, err := Register(Info{RunID: next, ResumedFrom: id}, func() {})
	if err != nil {
		t.Fatal(err)
	}
	defer child.Finish(history.Record{Status: "ok"})
	m, _ := Read(id)
	if m.ResumedBy != next || m.Status != "canceled" {
		t.Fatal(m)
	}
	if err := SetResumedBy(id, strings.Repeat("e", 32)); err == nil {
		t.Fatal("second resume accepted")
	}
}

func TestRejectUnsafeControlStorageAndResume(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	id := strings.Repeat("f", 32)
	if err := Validate(Info{RunID: id, ResumeCommand: []string{"sh", "-c", "echo bad"}, ResumeDirectory: t.TempDir()}); err == nil {
		t.Fatal("relative entry point accepted")
	}
	if err := os.MkdirAll(filepath.Dir(Dir()), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), Dir()); err != nil {
		t.Fatal(err)
	}
	if _, err := Register(Info{RunID: id}, func() {}); err == nil {
		t.Fatal("symlink storage accepted")
	}
}
