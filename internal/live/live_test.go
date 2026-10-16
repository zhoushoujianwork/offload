package live

import (
	"path/filepath"
	"testing"
	"time"
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
