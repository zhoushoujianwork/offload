package worker

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"time"
)

// ownedStdout bounds idle pipe reads after exit without timing out a caller's
// event callback. exec.Cmd's WaitDelay also counts time spent in the callback.
type ownedStdout struct {
	reader, writer *os.File
	exited         chan struct{}
	copied         chan error
	grace          time.Duration
}

func newOwnedStdout(cmd *exec.Cmd, out io.Writer, grace time.Duration, onError func()) (*ownedStdout, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	s := &ownedStdout{reader: r, writer: w, exited: make(chan struct{}), copied: make(chan error, 1), grace: grace}
	cmd.Stdout = w
	go func() {
		buf := make([]byte, 32<<10)
		for {
			select {
			case <-s.exited:
				_ = r.SetReadDeadline(time.Now().Add(grace))
			default:
			}
			n, err := r.Read(buf)
			if n > 0 {
				if _, e := out.Write(buf[:n]); e != nil {
					if onError != nil {
						onError()
					}
					s.copied <- e
					return
				}
			}
			if err != nil {
				if errors.Is(err, io.EOF) {
					err = nil
				} else if errors.Is(err, os.ErrDeadlineExceeded) {
					err = exec.ErrWaitDelay
				}
				s.copied <- err
				return
			}
		}
	}()
	return s, nil
}

func (s *ownedStdout) processExited() {
	_ = s.writer.Close()
	close(s.exited)
	_ = s.reader.SetReadDeadline(time.Now().Add(s.grace))
}

func (s *ownedStdout) close() { _ = s.writer.Close(); _ = s.reader.Close() }
