//go:build !(darwin || linux || freebsd || netbsd || openbsd || dragonfly)

// Package proc owns the process group a worker runs in. Process groups are not available on
// this platform: only the worker process itself can be stopped, and descendants are not tracked.
package proc

import (
	"os"
	"os/exec"
)

// Supported reports whether worker process groups are available on this platform.
const Supported = false

// Group identifies the process group created for one worker process.
type Group struct {
	PID   int    `json:"pid"`
	PGID  int    `json:"pgid"`
	Start string `json:"start,omitempty"`

	p *os.Process
}

func Prepare(*exec.Cmd)           {}
func Started(cmd *exec.Cmd) Group { return Group{PID: cmd.Process.Pid, p: cmd.Process} }
func StartToken(int) string       { return "" }

// Exists and Alive report false: descendants cannot be observed here.
func (g Group) Exists() bool { return false }
func (g Group) Alive() bool  { return false }

// Term and Kill stop only the worker process itself.
func (g Group) Term() { g.Kill() }
func (g Group) Kill() {
	if g.p != nil {
		_ = g.p.Kill()
	}
}
