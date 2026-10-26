package config

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
)

// Limits caps how much of the machine offload workers may use. Zero or unset selects the
// default; -1 turns the limit off.
//
// These are admission and environment defaults, not CPU or memory limits: they do not constrain
// xcodebuild or other non-Go workloads, and a worker can still override the environment.
type Limits struct {
	// MaxWorkers is how many workers may run at once for this user, across all terminals and
	// projects. Excess runs wait in a queue before any backend starts.
	MaxWorkers int `json:"max_workers,omitempty"`
	// GoMaxProcs is exported to the worker as GOMAXPROCS unless the caller already set it.
	GoMaxProcs int `json:"go_max_procs,omitempty"`
	// GoParallelism is added to the worker's GOFLAGS as -p=N unless GOFLAGS already sets -p.
	GoParallelism int `json:"go_parallelism,omitempty"`
}

// Defaults for the Go build knobs. MaxWorkers has no fixed default; see DefaultMaxWorkers.
const (
	DefaultGoMaxProcs    = 2
	DefaultGoParallelism = 1
	// MaxWorkersEnv overrides the configured worker limit for one invocation.
	MaxWorkersEnv = "OFFLOAD_MAX_WORKERS"
)

// DefaultMaxWorkers scales with the machine: a quarter of the logical CPUs, at least one.
// An 8-CPU workstation gets two.
func DefaultMaxWorkers() int { return max(1, runtime.NumCPU()/4) }

// EffectiveMaxWorkers resolves OFFLOAD_MAX_WORKERS, then the config, then the default. It
// returns 0 when admission control is off.
func (l Limits) EffectiveMaxWorkers() (int, error) {
	n := l.MaxWorkers
	if v := strings.TrimSpace(os.Getenv(MaxWorkersEnv)); v != "" {
		parsed, err := strconv.Atoi(v)
		if err != nil || parsed < -1 || parsed == 0 {
			return 0, fmt.Errorf("%s=%q: want a positive number, or -1 for no limit", MaxWorkersEnv, v)
		}
		n = parsed
	}
	switch {
	case n == -1:
		return 0, nil
	case n > 0:
		return n, nil
	}
	return DefaultMaxWorkers(), nil
}

func knob(v, def int) int {
	switch {
	case v == -1:
		return 0
	case v > 0:
		return v
	}
	return def
}

// EffectiveGoMaxProcs is the GOMAXPROCS default for workers, or 0 for none.
func (l Limits) EffectiveGoMaxProcs() int { return knob(l.GoMaxProcs, DefaultGoMaxProcs) }

// EffectiveGoParallelism is the go build/test -p default for workers, or 0 for none.
func (l Limits) EffectiveGoParallelism() int { return knob(l.GoParallelism, DefaultGoParallelism) }

// ApplyEnv adds the build defaults to a worker environment. Settings the caller already made win:
// an existing GOMAXPROCS is kept, an existing -p in GOFLAGS is kept, and other GOFLAGS stay.
func (l Limits) ApplyEnv(env []string) []string {
	out := make([]string, 0, len(env)+2)
	out = append(out, env...)
	if n := l.EffectiveGoMaxProcs(); n > 0 && envValue(env, "GOMAXPROCS") == "" {
		out = setEnv(out, "GOMAXPROCS", strconv.Itoa(n))
	}
	if n := l.EffectiveGoParallelism(); n > 0 {
		flags := envValue(env, "GOFLAGS")
		if !hasParallelismFlag(flags) {
			out = setEnv(out, "GOFLAGS", strings.TrimSpace(flags+" -p="+strconv.Itoa(n)))
		}
	}
	return out
}

// Summary describes the effective limits for status output.
func (l Limits) Summary(maxWorkers int) string {
	parts := []string{"max_workers=" + limitText(maxWorkers)}
	parts = append(parts, "GOMAXPROCS="+limitText(l.EffectiveGoMaxProcs()), "go_p="+limitText(l.EffectiveGoParallelism()))
	return strings.Join(parts, " ")
}

func limitText(n int) string {
	if n <= 0 {
		return "off"
	}
	return strconv.Itoa(n)
}

func hasParallelismFlag(goflags string) bool {
	for _, f := range strings.Fields(goflags) {
		if strings.HasPrefix(f, "-p=") || f == "-p" || strings.HasPrefix(f, "--p=") {
			return true
		}
	}
	return false
}

func envValue(env []string, key string) string {
	v := ""
	for _, kv := range env {
		if k, val, ok := strings.Cut(kv, "="); ok && k == key {
			v = val // like the process environment, the last entry wins
		}
	}
	return v
}

func setEnv(env []string, key, value string) []string {
	kept := env[:0:0]
	for _, kv := range env {
		if k, _, _ := strings.Cut(kv, "="); k != key {
			kept = append(kept, kv)
		}
	}
	return append(kept, key+"="+value)
}
