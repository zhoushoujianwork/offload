//go:build !(darwin || linux || freebsd || netbsd || openbsd || dragonfly)

package slots

// Supported is false here: no cross-process locking is available, so admission control and
// session locks are no-ops.
const Supported = false

func openLock(string) (fileLock, bool, error) { return nil, true, nil }
