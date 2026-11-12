package worker

import (
	"slices"
	"testing"

	"github.com/zhoushoujianwork/offload/internal/config"
)

func TestClaudeEffortArgs(t *testing.T) {
	for _, native := range []*NativeControls{nil, {}} {
		for _, resume := range []string{"", "session"} {
			for _, effort := range []string{"", "low", "medium", "high", "xhigh", "max"} {
				args := buildArgs(Opts{Profile: config.Profile{ReasoningEffort: effort}, Native: native, Resume: resume}, "prompt")
				at := slices.Index(args, "--effort")
				if effort == "" {
					if at >= 0 {
						t.Fatal(args)
					}
					continue
				}
				if at < 0 || args[at+1] != effort {
					t.Fatal(args)
				}
			}
		}
	}
}

func TestCodexAppServerEffectiveSettings(t *testing.T) {
	for _, tier := range []string{"default", "priority"} {
		args := buildCodexAppServerArgs(Opts{Profile: config.Profile{ReasoningEffort: "ultra", ServiceTier: tier}})
		for _, want := range []string{`model_reasoning_effort="ultra"`, `service_tier="` + tier + `"`} {
			if !slices.Contains(args, want) {
				t.Fatalf("missing %q: %q", want, args)
			}
		}
	}
}
