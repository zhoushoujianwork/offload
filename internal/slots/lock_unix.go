//go:build darwin || linux || freebsd || netbsd || openbsd || dragonfly

package slots

import (
	"errors"
	"io"
	"os"
	"syscall"
)

// Supported reports whether cross-process admission is available on this platform.
const Supported = true

type flock struct{ f *os.File }

// openLock opens path and tries to take an exclusive lock without blocking.
func openLock(path string) (fileLock, bool, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, false, err
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return &flock{f}, true, nil
}

func (l *flock) readAll() ([]byte, error) {
	if _, err := l.f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	return io.ReadAll(l.f)
}

func (l *flock) writeAll(b []byte) error {
	if err := l.f.Truncate(0); err != nil {
		return err
	}
	_, err := l.f.WriteAt(b, 0)
	return err
}

func (l *flock) unlock() {
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	l.f.Close()
}
