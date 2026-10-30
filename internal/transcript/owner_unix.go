//go:build unix

package transcript

import (
	"os"
	"syscall"
)

func ownedByUs(st os.FileInfo) bool {
	s, ok := st.Sys().(*syscall.Stat_t)
	return !ok || int(s.Uid) == os.Getuid()
}

// otherAccess reports whether group or other users have any access.
func otherAccess(st os.FileInfo) bool { return st.Mode().Perm()&0o077 != 0 }
