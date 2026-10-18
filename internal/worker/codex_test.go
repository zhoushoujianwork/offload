package worker

import (
	"strings"
	"testing"
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
