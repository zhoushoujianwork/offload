package slots

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zhoushoujianwork/offload/internal/proc"
)

func fast(t *testing.T) {
	t.Helper()
	orig := PollEvery
	PollEvery = 10 * time.Millisecond
	t.Cleanup(func() { PollEvery = orig })
}

func TestLimitIsNeverExceeded(t *testing.T) {
	if !Supported {
		t.Skip("no cross-process locking on this platform")
	}
	fast(t)
	dir := t.TempDir()
	var cur, peak atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := Acquire(context.Background(), Options{Dir: dir, Max: 2})
			if err != nil {
				t.Error(err)
				return
			}
			n := cur.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			time.Sleep(30 * time.Millisecond)
			cur.Add(-1)
			s.Release()
		}()
	}
	wg.Wait()
	if peak.Load() != 2 {
		t.Fatalf("peak concurrency %d, want exactly the limit 2", peak.Load())
	}
}

func TestQueuedAcquireIsCancellable(t *testing.T) {
	if !Supported {
		t.Skip("no cross-process locking on this platform")
	}
	fast(t)
	dir := t.TempDir()
	held, err := Acquire(context.Background(), Options{Dir: dir, Max: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	var waits atomic.Int32
	s, err := Acquire(ctx, Options{Dir: dir, Max: 1, OnWait: func(w Wait) {
		waits.Add(1)
		if w.InUse != 1 || w.Max != 1 {
			t.Errorf("wait: %+v", w)
		}
	}})
	if s != nil || !errors.Is(err, context.DeadlineExceeded) || waits.Load() == 0 {
		t.Fatalf("slot=%v err=%v waits=%d", s, err, waits.Load())
	}
}

func TestDisabledAdmissionNeverBlocks(t *testing.T) {
	for i := 0; i < 5; i++ {
		s, err := Acquire(context.Background(), Options{Dir: t.TempDir(), Max: 0})
		if err != nil || s.Index() != -1 || !s.Release() {
			t.Fatalf("%v %v", s, err)
		}
	}
}

// detached starts a process that leads its own group, like a worker.
func detached(t *testing.T) proc.Group {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	proc.Prepare(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	g := proc.Started(cmd)
	// Reap it as soon as it dies: a zombie still counts as a member of its process group.
	go cmd.Wait()
	t.Cleanup(g.Kill)
	return g
}

func writeSlot(t *testing.T, dir string, i int, info Info) {
	t.Helper()
	b, _ := json.Marshal(info)
	if err := os.WriteFile(filepath.Join(dir, "slot-"+string(rune('0'+i))+".lock"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// A crashed owner frees its lock, but its worker's processes may live on. The slot must stay
// unavailable and say why, instead of being handed to a new worker.
func TestSlotWithLiveOrphanedGroupIsNotReused(t *testing.T) {
	if !proc.Supported || !Supported {
		t.Skip("needs process groups")
	}
	fast(t)
	dir := t.TempDir()
	g := detached(t)
	writeSlot(t, dir, 0, Info{OwnerPID: 999999, Group: &g})

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	var last Wait
	if s, err := Acquire(ctx, Options{Dir: dir, Max: 1, OnWait: func(w Wait) { last = w }}); s != nil || err == nil {
		t.Fatalf("slot reused while its worker group is alive: %v %v", s, err)
	}
	if len(last.Unresolved) != 1 || last.InUse != 0 {
		t.Fatalf("wait should report unresolved processes, not a running owner: %+v", last)
	}

	g.Kill()
	deadline := time.Now().Add(5 * time.Second)
	for g.Alive() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	s, err := Acquire(ctx2, Options{Dir: dir, Max: 1})
	if err != nil || s.Index() != 0 {
		t.Fatalf("slot must be reusable once the group is gone: %v %v", s, err)
	}
	s.Release()
}

// A recycled PID must not make a stale slot look busy forever, nor a live group look gone.
func TestStaleRecordWithRecycledPIDDoesNotBlock(t *testing.T) {
	if !proc.Supported || !Supported || proc.StartToken(os.Getpid()) == "" {
		t.Skip("needs process groups and ps")
	}
	fast(t)
	dir := t.TempDir()
	g := detached(t)
	real := g.Start
	if real == "" {
		t.Skip("start time unavailable")
	}
	stale := g
	stale.Start = "Thu Jan  1 00:00:00 1970" // the recorded process is not the one now using this pid
	writeSlot(t, dir, 0, Info{OwnerPID: 999999, Group: &stale})
	ctx1, cancel1 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel1()
	s, err := Acquire(ctx1, Options{Dir: dir, Max: 1})
	if err != nil || s == nil {
		t.Fatalf("recycled pid blocked the slot: %v", err)
	}
	s.Release()

	live := g
	live.Start = real
	writeSlot(t, dir, 0, Info{OwnerPID: 999999, Group: &live})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if s, _ := Acquire(ctx, Options{Dir: dir, Max: 1}); s != nil {
		t.Fatal("the genuine live group was ignored")
	}
}

func TestReleaseKeepsSlotUnavailableWhileGroupAlive(t *testing.T) {
	if !proc.Supported || !Supported {
		t.Skip("needs process groups")
	}
	fast(t)
	dir := t.TempDir()
	s, err := Acquire(context.Background(), Options{Dir: dir, Max: 1})
	if err != nil {
		t.Fatal(err)
	}
	g := detached(t)
	s.Track(g)
	if s.Release() {
		t.Fatal("Release reported clean while the tracked group is alive")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if again, _ := Acquire(ctx, Options{Dir: dir, Max: 1}); again != nil {
		t.Fatal("capacity was published as free with live worker processes")
	}
}

func TestSessionLockIsExclusive(t *testing.T) {
	if !Supported {
		t.Skip("no cross-process locking on this platform")
	}
	dir := t.TempDir()
	var winners atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	locks := make(chan *SessionLock, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			l, _, err := LockSession(dir, "s-1")
			if err == nil {
				winners.Add(1)
				locks <- l
			} else if !errors.Is(err, ErrSessionBusy) {
				t.Error(err)
			}
		}()
	}
	close(start)
	wg.Wait()
	if winners.Load() != 1 {
		t.Fatalf("%d simultaneous resumes got the lock", winners.Load())
	}
	if _, _, err := LockSession(dir, "s-2"); err != nil {
		t.Fatalf("other sessions must be unaffected: %v", err)
	}
	(<-locks).Release()
	l, _, err := LockSession(dir, "s-1")
	if err != nil {
		t.Fatalf("released session still locked: %v", err)
	}
	l.Release()
}
