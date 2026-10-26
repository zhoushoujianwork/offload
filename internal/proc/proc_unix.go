//go:build darwin || linux || freebsd || netbsd || openbsd || dragonfly

// Package proc owns the process group a worker runs in, so offload can stop the worker's
// build and test children and later check that they are gone.
package proc

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// Supported reports whether worker process groups are available on this platform.
const Supported = true

// Group identifies the process group created for one worker process.
type Group struct {
	PID   int    `json:"pid"`             // group leader: the worker process offload started
	PGID  int    `json:"pgid"`            // equals PID for a group created with Setpgid
	Start string `json:"start,omitempty"` // leader start time, to tell a recycled PID from the original
}

// Prepare makes cmd the leader of a new process group, so its descendants can be signalled together.
func Prepare(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

// Started describes the group of a started cmd.
func Started(cmd *exec.Cmd) Group {
	pid := cmd.Process.Pid
	return Group{PID: pid, PGID: pid, Start: StartToken(pid)}
}

// StartToken returns the process start time as ps prints it, or "" if it cannot be read.
func StartToken(pid int) string {
	c := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(pid))
	c.Env = append(os.Environ(), "LC_ALL=C")
	out, err := c.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// Exists is a cheap check that some process is still in the group. A zombie counts until reaped.
func (g Group) Exists() bool {
	if g.PGID <= 1 {
		return false
	}
	err := syscall.Kill(-g.PGID, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// Alive reports whether the group offload started still has processes. The kernel does not hand
// out a PID that is still a process group id, so a live leader PID with a different start time
// proves the original group is gone and the id now belongs to someone else.
func (g Group) Alive() bool {
	if !g.Exists() {
		return false
	}
	if g.Start != "" && g.PID > 0 {
		if cur := StartToken(g.PID); cur != "" && cur != g.Start {
			return false
		}
	}
	return true
}

// Term asks every process in the group to exit.
func (g Group) Term() { signal(g, syscall.SIGTERM) }

// Kill force-stops every process in the group.
func (g Group) Kill() { signal(g, syscall.SIGKILL) }

func signal(g Group, sig syscall.Signal) {
	if g.PGID > 1 {
		_ = syscall.Kill(-g.PGID, sig)
	}
}
