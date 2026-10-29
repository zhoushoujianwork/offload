package live

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/zhoushoujianwork/offload/internal/proc"
)

func setState(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "state"))
	origStale, origExpire := StaleAfter, ExpireAfter
	t.Cleanup(func() { StaleAfter, ExpireAfter = origStale, origExpire })
}

func TestPublishAndList(t *testing.T) {
	setState(t)
	if rs, err := List(); err != nil || len(rs) != 0 {
		t.Fatalf("empty dir: %v %v", rs, err)
	}
	now := time.Now()
	for _, s := range []State{
		{PID: 2, Phase: Running, ToolCalls: 7, StartedAt: now},
		{PID: 1, Phase: Done, Status: "ok", StartedAt: now.Add(-time.Minute)},
	} {
		if err := Publish(s); err != nil {
			t.Fatal(err)
		}
	}
	rs, err := List()
	if err != nil || len(rs) != 2 || rs[0].PID != 1 || rs[1].PID != 2 || rs[1].ToolCalls != 7 || !rs[1].Active() || rs[0].Active() {
		t.Fatalf("list: %+v err=%v", rs, err)
	}
}

func TestStaleActiveStateIsLostAndOldFilesExpire(t *testing.T) {
	setState(t)
	Publish(State{PID: 5, Phase: Running, StartedAt: time.Now()})
	Publish(State{PID: 6, Phase: Done, StartedAt: time.Now()})
	StaleAfter = 20 * time.Millisecond
	time.Sleep(60 * time.Millisecond)
	rs, _ := List()
	phases := map[int]string{}
	for _, s := range rs {
		phases[s.PID] = s.Phase
	}
	if phases[5] != Lost || phases[6] != Done {
		t.Fatalf("only an active state goes lost: %v", phases)
	}
	ExpireAfter = 20 * time.Millisecond
	if rs, _ := List(); len(rs) != 0 {
		t.Fatalf("expired states must be dropped: %+v", rs)
	}
}

// An expired heartbeat does not prove the worker's processes are gone. While the recorded group is
// alive the state is reported with Unresolved set and is not expired; a group that is gone, or a
// recycled pid, is not reported as running.
func TestStaleStateChecksWorkerProcessGroup(t *testing.T) {
	if !proc.Supported {
		t.Skip("needs process groups")
	}
	setState(t)
	cmd := exec.Command("sleep", "60")
	proc.Prepare(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	g := proc.Started(cmd)
	go cmd.Wait()
	t.Cleanup(g.Kill)

	now := time.Now()
	Publish(State{PID: 11, Phase: Running, StartedAt: now, PGID: g.PGID, PGIDStart: g.Start})
	Publish(State{PID: 12, Phase: Running, StartedAt: now, PGID: 2147480000, PGIDStart: "x"})
	Publish(State{PID: 13, Phase: Done, Status: "ok", StartedAt: now, PGID: g.PGID, PGIDStart: g.Start})
	StaleAfter = 20 * time.Millisecond
	time.Sleep(60 * time.Millisecond)
	got := map[int]State{}
	rs, _ := List()
	for _, s := range rs {
		got[s.PID] = s
	}
	if got[11].Phase != Lost || !got[11].Unresolved {
		t.Errorf("lost owner with a live group must be flagged: %+v", got[11])
	}
	if got[12].Phase != Lost || got[12].Unresolved {
		t.Errorf("a group that does not exist must not be flagged: %+v", got[12])
	}
	if got[13].Phase != Done || got[13].Unresolved {
		t.Errorf("finished runs are not rechecked: %+v", got[13])
	}

	ExpireAfter = 20 * time.Millisecond
	rs, _ = List()
	kept := map[int]bool{}
	for _, s := range rs {
		kept[s.PID] = true
	}
	if !kept[11] || kept[12] || kept[13] {
		t.Errorf("only the state with live processes may outlive expiry: %v", kept)
	}

	g.Kill()
	deadline := time.Now().Add(5 * time.Second)
	for g.Alive() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if rs, _ := List(); len(rs) != 0 {
		t.Errorf("state should expire once the processes are gone: %+v", rs)
	}
}

func TestCallerInLiveState(t *testing.T) {
	setState(t)
	now := time.Now()
	if err := Publish(State{PID: 1, Phase: Running, Caller: "agh", StartedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := Publish(State{PID: 2, Phase: Running, StartedAt: now.Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	// A state file written before the field existed.
	legacy := []byte(`{"pid":3,"phase":"running","dir":"d","task":"t","profile":"p","model":"m","started_at":"` + now.Add(2*time.Second).Format(time.RFC3339Nano) + `","updated_at":"` + now.Format(time.RFC3339Nano) + `","last_event_at":"` + now.Format(time.RFC3339Nano) + `","tool_calls":0,"permission_denials":0}`)
	if err := os.WriteFile(filepath.Join(Dir(), "3.json"), legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	rs, err := List()
	if err != nil || len(rs) != 3 || rs[0].Caller != "agh" || rs[1].Caller != "unspecified" || rs[2].Caller != "unspecified" {
		t.Fatalf("%+v %v", rs, err)
	}
	if b, _ := os.ReadFile(filepath.Join(Dir(), "3.json")); string(b) != string(legacy) {
		t.Fatalf("legacy state rewritten: %s", b)
	}
}
