package config

import (
	"slices"
	"testing"
)

func TestApplyEnvAddsDefaultsAndKeepsExplicitSettings(t *testing.T) {
	l := Limits{}
	got := l.ApplyEnv([]string{"PATH=/bin"})
	for _, want := range []string{"GOMAXPROCS=2", "GOFLAGS=-p=1"} {
		if !slices.Contains(got, want) {
			t.Errorf("missing %s in %v", want, got)
		}
	}
	// Explicit caller settings win; unrelated GOFLAGS are kept.
	got = l.ApplyEnv([]string{"GOMAXPROCS=6", "GOFLAGS=-mod=vendor -p=4"})
	if !slices.Contains(got, "GOMAXPROCS=6") || !slices.Contains(got, "GOFLAGS=-mod=vendor -p=4") {
		t.Errorf("explicit settings overridden: %v", got)
	}
	got = l.ApplyEnv([]string{"GOFLAGS=-mod=vendor"})
	if !slices.Contains(got, "GOFLAGS=-mod=vendor -p=1") {
		t.Errorf("-p not appended to existing GOFLAGS: %v", got)
	}
	// Opt out per knob.
	got = Limits{GoMaxProcs: -1, GoParallelism: -1}.ApplyEnv([]string{"PATH=/bin"})
	if len(got) != 1 {
		t.Errorf("disabled knobs still set env: %v", got)
	}
	if got := (Limits{GoMaxProcs: 3, GoParallelism: 2}).ApplyEnv(nil); !slices.Contains(got, "GOMAXPROCS=3") || !slices.Contains(got, "GOFLAGS=-p=2") {
		t.Errorf("configured values: %v", got)
	}
}

func TestApplyEnvDoesNotModifyInput(t *testing.T) {
	in := []string{"A=1"}
	Limits{}.ApplyEnv(in)
	if len(in) != 1 || in[0] != "A=1" {
		t.Fatal(in)
	}
}

func TestEffectiveMaxWorkers(t *testing.T) {
	t.Setenv(MaxWorkersEnv, "")
	if n, err := (Limits{}).EffectiveMaxWorkers(); err != nil || n != DefaultMaxWorkers() || n < 1 {
		t.Fatalf("default: %d %v", n, err)
	}
	if n, _ := (Limits{MaxWorkers: 3}).EffectiveMaxWorkers(); n != 3 {
		t.Fatal(n)
	}
	if n, _ := (Limits{MaxWorkers: -1}).EffectiveMaxWorkers(); n != 0 {
		t.Fatalf("-1 should disable the limit, got %d", n)
	}
	t.Setenv(MaxWorkersEnv, "5")
	if n, _ := (Limits{MaxWorkers: 3}).EffectiveMaxWorkers(); n != 5 {
		t.Fatalf("env should override config, got %d", n)
	}
	for _, bad := range []string{"0", "-2", "many"} {
		t.Setenv(MaxWorkersEnv, bad)
		if _, err := (Limits{}).EffectiveMaxWorkers(); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
