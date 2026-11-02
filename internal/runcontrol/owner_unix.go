//go:build unix

package runcontrol

import (
	"errors"
	"os"
	"sync"
	"syscall"
	"time"
)

func owned(st os.FileInfo) bool {
	s, ok := st.Sys().(*syscall.Stat_t)
	return ok && int(s.Uid) == os.Geteuid()
}
func Lease(id string) (func(), error) { return lease(id, true) }
func lease(id string, nonblock bool) (func(), error) {
	if _, err := Read(id); err != nil {
		return nil, err
	}
	fd, err := syscall.Open(Path(id)+".lock", syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), "resume lease")
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || !private(st) {
		f.Close()
		return nil, errors.New("unsafe resume lease")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if nonblock || (err != syscall.EWOULDBLOCK && err != syscall.EAGAIN) || time.Now().After(deadline) {
			f.Close()
			return nil, err
		}
		time.Sleep(10 * time.Millisecond)
	}
	var once sync.Once
	return func() { once.Do(func() { _ = syscall.Flock(fd, syscall.LOCK_UN); _ = f.Close() }) }, nil
}
