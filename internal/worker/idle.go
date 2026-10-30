package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/zhoushoujianwork/offload/internal/proc"
)

// 空闲间隔使用单调时钟，避免系统校时导致误判。
var activityEpoch = time.Now()

var ErrSignal = errors.New("worker interrupted by signal")

// SignalError is the cancellation cause for a signal that offload itself received. It matches
// ErrSignal, so the stop reason stays "signal", and keeps which signal arrived so the run can be
// classified as a requested cancel (SIGINT) or a shutdown interruption (SIGTERM).
type SignalError struct{ Signal os.Signal }

func (e *SignalError) Error() string {
	return "worker interrupted by signal " + proc.SignalName(e.Signal)
}

func (e *SignalError) Is(target error) bool { return target == ErrSignal }

func stopReason(cause error) string {
	switch {
	case errors.Is(cause, errIdle):
		return "idle"
	case errors.Is(cause, context.DeadlineExceeded):
		return "deadline"
	case errors.Is(cause, ErrSignal):
		return "signal"
	case errors.Is(cause, context.Canceled):
		return "canceled"
	default:
		return ""
	}
}

func (o Opts) idleLimit() (time.Duration, error) {
	limit := IdleLimit
	if o.ExecutionPolicy == "managed-development" {
		limit = 30 * time.Minute
	}
	if o.IdleTimeout != nil {
		limit = *o.IdleTimeout
	} else if o.Profile.IdleTimeout != "" {
		var err error
		limit, err = time.ParseDuration(o.Profile.IdleTimeout)
		if err != nil {
			return 0, fmt.Errorf("invalid idle_timeout: %w", err)
		}
	}
	if limit < 0 {
		return 0, errors.New("idle timeout must not be negative")
	}
	if limit > o.Timeout {
		limit = max(0, o.Timeout)
	}
	return limit, nil
}

type livenessWriter struct {
	w    io.Writer
	last *atomic.Int64
}

func (w *livenessWriter) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	if n > 0 {
		w.last.Store(time.Since(activityEpoch).Nanoseconds())
	}
	return n, err
}

func watchIdle(ctx context.Context, cancel context.CancelCauseFunc, last *atomic.Int64, limit time.Duration, grew func() bool) {
	if limit <= 0 {
		return
	}
	t := time.NewTicker(max(time.Nanosecond, min(time.Second, limit/10)))
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if grew != nil && grew() {
				last.Store(time.Since(activityEpoch).Nanoseconds())
			}
			if time.Since(activityEpoch.Add(time.Duration(last.Load()))) >= limit {
				// 总时限与空闲时限同时到达时，总时限优先。
				if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
					cancel(context.DeadlineExceeded)
				} else {
					cancel(errIdle)
				}
				return
			}
		}
	}
}

// 只观察当前会话的原生日志，不把其他 worker 的写入计作存活。
type sessionActivity struct {
	root  string
	codex bool
	sizes map[string]int64
}

func newSessionActivity(o Opts) *sessionActivity {
	home, _ := os.UserHomeDir()
	a := &sessionActivity{codex: o.Profile.BackendName() == "codex", sizes: map[string]int64{}}
	if a.codex {
		root := os.Getenv("CODEX_HOME")
		if root == "" {
			root = filepath.Join(home, ".codex")
		}
		a.root = filepath.Join(root, "sessions")
	} else {
		root := os.Getenv("CLAUDE_CONFIG_DIR")
		if root == "" {
			root = filepath.Join(home, ".claude")
		}
		a.root = filepath.Join(root, "projects")
	}
	a.grew(o.Resume)
	return a
}

func (a *sessionActivity) grew(session string) bool {
	// 会话标识来自子进程，禁止路径及 glob 元字符。
	if session == "" || strings.ContainsAny(session, "/\\*?[]") || session == "." || session == ".." {
		return false
	}
	pattern := filepath.Join(a.root, "*", session+".jsonl")
	if a.codex {
		pattern = filepath.Join(a.root, "*", "*", "*", "*-"+session+".jsonl")
	}
	paths, _ := filepath.Glob(pattern)
	grew := false
	for _, path := range paths {
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if info.Size() > a.sizes[path] {
			grew = true
		}
		a.sizes[path] = info.Size()
	}
	return grew
}
