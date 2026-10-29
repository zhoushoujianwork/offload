//go:build unix

package procutil

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

func configure(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	// The child becomes the leader of a new group (pgid == pid) before exec.
	cmd.SysProcAttr.Setpgid = true
	cmd.Cancel = func() error { return killGroup(cmd.Process) }
}

// killGroup sends SIGKILL to the process group led by p. It refuses pids that could address
// something other than the group this package created.
func killGroup(p *os.Process) error {
	if p == nil {
		return errors.New("procutil: process not started")
	}
	return killGroupPID(p.Pid)
}

func killGroupPID(pid int) error {
	if pid <= 1 {
		return errors.New("procutil: refusing to signal invalid process group")
	}
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	return nil
}
