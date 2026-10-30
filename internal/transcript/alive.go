package transcript

import "github.com/zhoushoujianwork/offload/internal/proc"

// processAlive reports whether pid is still the process that started at start. Without a start
// token the answer is false: a recycled PID must not keep a dead capture "running".
func processAlive(pid int, start string) bool {
	if pid <= 1 || start == "" {
		return false
	}
	return proc.StartToken(pid) == start
}
