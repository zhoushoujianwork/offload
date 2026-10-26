package worker

import (
	"slices"
	"strings"
	"testing"

	"github.com/zhoushoujianwork/offload/internal/config"
)

func TestCodexStreamProgressAndTruncatedTail(t *testing.T) {
	s := parseCodexStream([]byte(`{"type":"thread.started","thread_id":"thread-1"}
{"type":"item.started","item":{"id":"cmd","type":"command_execution","command":"rg retry"}}
{"type":"item.completed","item":{"id":"cmd","type":"command_execution"}}
{"type":"item.completed","item":{"id":"mcp","type":"mcp_tool_call","tool":"search"}}
{"type":"item.completed","item":{"id":"msg","type":"agent_message","text":"partial report"}}
{"type":"turn.comp`))
	if s.Session != "thread-1" || s.ToolCalls != 2 || s.Result != nil || s.LastText != "partial report" || len(s.LastTools) != 2 {
		t.Fatalf("%+v", s)
	}
	if !strings.Contains(s.resumeHint(), "thread-1") {
		t.Fatal(s.resumeHint())
	}
}

func TestCodexCompletionRequiresReport(t *testing.T) {
	s := parseCodexStream([]byte("{\"type\":\"error\",\"message\":\"reconnecting\"}\n{\"type\":\"turn.completed\"}"))
	if s.Result == nil || !s.Result.IsError {
		t.Fatalf("%+v", s)
	}
}

func TestCodexBundleArgs(t *testing.T) {
	for _, write := range []bool{false, true} {
		for _, resume := range []string{"", "session"} {
			p := config.Profile{ReasoningEffort: "xhigh", ServiceTier: "priority"}
			args := buildCodexArgs(Opts{Profile: p, Model: "override", Write: write, Resume: resume})
			want := []string{"exec", "--dangerously-bypass-approvals-and-sandbox", "-c", `model_reasoning_effort="xhigh"`, "-c", `service_tier="priority"`}
			if resume != "" {
				want = append(want, "resume")
			}
			want = append(want, "--json", "--skip-git-repo-check", "-m", "override")
			if resume != "" {
				want = append(want, resume)
			}
			want = append(want, "-")
			if !slices.Equal(args, want) {
				t.Fatalf("%q != %q", args, want)
			}
		}
	}
	args := strings.Join(buildCodexArgs(Opts{}), " ")
	if strings.Contains(args, "model_reasoning_effort") || strings.Contains(args, "service_tier") || strings.Contains(args, "-m ") {
		t.Fatal(args)
	}
}
