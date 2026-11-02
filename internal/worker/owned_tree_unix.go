//go:build unix

package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Native Bash tools may create their own session, outside the native process
// group. Freeze the owned tree before killing it so these children cannot be
// reparented when the native CLI exits. Never select processes by command text.
func stopOwnedTree(pid int) error {
	if err := syscall.Kill(-pid, syscall.SIGSTOP); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	owned := map[int]bool{pid: true}
	order := []int{}
	var scanErr error
	for pass := 0; pass < 8; pass++ {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		raw, err := exec.CommandContext(ctx, "/bin/ps", "-axo", "pid=,ppid=").Output()
		cancel()
		if err != nil {
			scanErr = fmt.Errorf("inspect native descendants before cancel: %w", err)
			break
		}
		parents := map[int]int{}
		for _, line := range strings.Split(string(raw), "\n") {
			fields := strings.Fields(line)
			if len(fields) != 2 {
				continue
			}
			child, e1 := strconv.Atoi(fields[0])
			parent, e2 := strconv.Atoi(fields[1])
			if e1 == nil && e2 == nil && child > 1 {
				parents[child] = parent
			}
		}
		before := len(order)
		for changed := true; changed; {
			changed = false
			for child, parent := range parents {
				if !owned[child] && owned[parent] {
					owned[child] = true
					order = append(order, child)
					_ = syscall.Kill(child, syscall.SIGSTOP)
					changed = true
				}
			}
		}
		if len(order) == before {
			break
		}
	}
	for i := len(order) - 1; i >= 0; i-- {
		_ = syscall.Kill(order[i], syscall.SIGKILL)
	}
	err := syscall.Kill(-pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		err = nil
	}
	return errors.Join(scanErr, err)
}
