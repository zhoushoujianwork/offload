package transcript

import (
	"bytes"
	"encoding/json"
	"strings"
)

// Line maps one complete line of backend output to transcript events. It is called in stream order
// by the worker's output reader. It never returns an error: whatever cannot be captured becomes a
// gap or a counter in the summary.
func (r *Recorder) Line(line []byte) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return
	}
	var head struct {
		Type      string `json:"type"`
		SessionID string `json:"session_id"`
		ThreadID  string `json:"thread_id"`
	}
	if json.Unmarshal(line, &head) != nil || head.Type == "" {
		r.gapLocked("unparseable_backend_line", len(line))
		return
	}
	if head.SessionID != "" {
		r.setSession(head.SessionID)
	}
	if head.ThreadID != "" {
		r.setSession(head.ThreadID)
	}
	switch r.id.Backend {
	case "codex":
		r.codexLine(head.Type, line)
	default:
		r.claudeLine(head.Type, line)
	}
}

// Claude Code `-p --output-format stream-json --verbose` prints one complete event per content
// block group: assistant events carry text, thinking and tool_use blocks, user events carry
// tool_result blocks. The worker is not started with --include-partial-messages, so no text deltas
// exist to capture.
func (r *Recorder) claudeLine(kind string, line []byte) {
	switch kind {
	case "system", "result", "rate_limit_event", "stream_event":
	case "assistant", "user":
	default:
		r.sum.Unrecognized++
		return
	}
	var e struct {
		Message struct {
			ID      string          `json:"id"`
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"message"`
		IsError bool     `json:"is_error"`
		Result  string   `json:"result"`
		Errors  []string `json:"errors"`
		Subtype string   `json:"subtype"`
	}
	if json.Unmarshal(line, &e) != nil {
		r.gapLocked("undecodable_backend_event", len(line))
		return
	}
	switch kind {
	case "result":
		if e.IsError {
			msg := strings.TrimSpace(strings.Join(append(append([]string{}, e.Errors...), e.Result), "\n"))
			if msg == "" {
				msg = e.Subtype
			}
			r.emitLocked(Event{Type: TypeBackendError, Text: msg}, false)
		}
		return
	case "assistant", "user":
	default:
		return
	}
	role := "assistant"
	if kind == "user" {
		role = "user"
	}
	// content is either a string (a plain user message) or a list of blocks.
	var text string
	if json.Unmarshal(e.Message.Content, &text) == nil {
		if strings.TrimSpace(text) != "" {
			r.emitLocked(Event{Type: TypeMessage, Role: role, Origin: "backend", MessageID: e.Message.ID, Text: text}, false)
		}
		return
	}
	var blocks []struct {
		Type      string          `json:"type"`
		Text      string          `json:"text"`
		ID        string          `json:"id"`
		Name      string          `json:"name"`
		Input     json.RawMessage `json:"input"`
		ToolUseID string          `json:"tool_use_id"`
		Content   json.RawMessage `json:"content"`
		IsError   *bool           `json:"is_error"`
	}
	if json.Unmarshal(e.Message.Content, &blocks) != nil {
		r.gapLocked("undecodable_message_content", len(line))
		return
	}
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if strings.TrimSpace(b.Text) != "" {
				r.emitLocked(Event{Type: TypeMessage, Role: role, Origin: "backend", MessageID: e.Message.ID, Text: b.Text}, false)
			}
		case "thinking", "redacted_thinking":
			r.sum.SkippedBlocks++
		case "tool_use":
			r.toolCallLocked(b.ID, b.Name, claudeCategory(b.Name), compact(b.Input))
		case "tool_result":
			out, omitted := claudeResultText(b.Content)
			ev := Event{Type: TypeToolResult, ToolCallID: b.ToolUseID, Output: out, IsError: b.IsError, Omitted: omitted}
			ev.ToolName = r.tools[b.ToolUseID]
			delete(r.open, b.ToolUseID)
			r.emitLocked(ev, false)
		default:
			r.sum.Unrecognized++
		}
	}
}

func claudeResultText(raw json.RawMessage) (string, []string) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return "", nil
	}
	var out []string
	var omitted []string
	for _, p := range parts {
		if p.Type == "text" {
			out = append(out, p.Text)
		} else if p.Type != "" {
			omitted = appendUnique(omitted, p.Type)
		}
	}
	return strings.Join(out, "\n"), omitted
}

func claudeCategory(name string) string {
	switch name {
	case "Bash":
		return "shell"
	case "Read", "Grep", "Glob", "Edit", "Write", "MultiEdit", "NotebookEdit":
		return "file"
	case "WebSearch", "WebFetch":
		return "web"
	}
	if strings.HasPrefix(name, "mcp__") {
		return "mcp"
	}
	return "other"
}

func (r *Recorder) toolCallLocked(id, name, category, args string) {
	if id != "" {
		if _, dup := r.tools[id]; dup {
			return
		}
		r.tools[id] = name
		r.open[id] = true
	}
	r.emitLocked(Event{Type: TypeToolCall, ToolCallID: id, ToolName: name, ToolCategory: category, Arguments: args}, false)
}

// compact renders raw JSON on one line; invalid or empty input is dropped.
func compact(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var b bytes.Buffer
	if json.Compact(&b, raw) != nil {
		return ""
	}
	return b.String()
}

// Codex `exec --json` prints one event per item state change. Messages and command output arrive
// complete on item.completed; there are no text deltas.
func (r *Recorder) codexLine(kind string, line []byte) {
	switch kind {
	case "thread.started", "turn.started", "turn.completed", "item.updated":
		return
	case "item.started", "item.completed", "error", "turn.failed":
	default:
		r.sum.Unrecognized++
		return
	}
	var e struct {
		Message string `json:"message"`
		Error   struct {
			Message string `json:"message"`
		} `json:"error"`
		Item struct {
			ID               string          `json:"id"`
			Type             string          `json:"type"`
			Text             string          `json:"text"`
			Message          string          `json:"message"`
			Command          string          `json:"command"`
			AggregatedOutput string          `json:"aggregated_output"`
			ExitCode         *int            `json:"exit_code"`
			Status           string          `json:"status"`
			Server           string          `json:"server"`
			Tool             string          `json:"tool"`
			Arguments        json.RawMessage `json:"arguments"`
			Result           json.RawMessage `json:"result"`
			Error            *struct {
				Message string `json:"message"`
			} `json:"error"`
			Query   string `json:"query"`
			Changes []struct {
				Path string `json:"path"`
				Kind string `json:"kind"`
			} `json:"changes"`
		} `json:"item"`
	}
	if json.Unmarshal(line, &e) != nil {
		r.gapLocked("undecodable_backend_event", len(line))
		return
	}
	switch kind {
	case "error":
		r.emitLocked(Event{Type: TypeBackendError, Text: e.Message}, false)
		return
	case "turn.failed":
		r.emitLocked(Event{Type: TypeBackendError, Text: e.Error.Message}, false)
		return
	}
	it, done := e.Item, kind == "item.completed"
	switch it.Type {
	case "agent_message":
		if done && strings.TrimSpace(it.Text) != "" && !r.codexSeq["m:"+it.ID] {
			r.codexSeq["m:"+it.ID] = true
			r.emitLocked(Event{Type: TypeMessage, Role: "assistant", Origin: "backend", MessageID: it.ID, Text: it.Text}, false)
		}
	case "reasoning", "todo_list":
		if done {
			r.sum.SkippedBlocks++
		}
	case "error":
		if done {
			r.emitLocked(Event{Type: TypeBackendError, Text: firstOf(it.Message, it.Text)}, false)
		}
	case "command_execution", "mcp_tool_call", "file_change", "web_search":
		name, category, args := it.Type, "other", ""
		switch it.Type {
		case "command_execution":
			name, category, args = "command_execution", "shell", it.Command
		case "mcp_tool_call":
			name, category, args = it.Server+"."+it.Tool, "mcp", compact(it.Arguments)
		case "file_change":
			category = "file"
			var paths []string
			for _, c := range it.Changes {
				paths = append(paths, c.Kind+" "+c.Path)
			}
			args = strings.Join(paths, "\n")
		case "web_search":
			category, args = "web", it.Query
		}
		if !r.codexSeq["c:"+it.ID] {
			r.codexSeq["c:"+it.ID] = true
			r.tools[it.ID] = name
			r.open[it.ID] = true
			r.emitLocked(Event{Type: TypeToolCall, ToolCallID: it.ID, ToolName: name, ToolCategory: category, Arguments: args}, false)
		}
		if !done || r.codexSeq["r:"+it.ID] {
			return
		}
		r.codexSeq["r:"+it.ID] = true
		delete(r.open, it.ID)
		out := it.AggregatedOutput
		failed := it.Status == "failed" || it.Status == "declined"
		if it.Type == "mcp_tool_call" {
			out = compact(it.Result)
			if it.Error != nil && it.Error.Message != "" {
				out, failed = it.Error.Message, true
			}
		}
		if it.ExitCode != nil && *it.ExitCode != 0 {
			failed = true
		}
		ev := Event{Type: TypeToolResult, ToolCallID: it.ID, ToolName: name, ToolCategory: category, Output: out, ExitCode: it.ExitCode}
		if it.Status != "" || it.ExitCode != nil || it.Error != nil {
			ev.IsError = &failed
		}
		r.emitLocked(ev, false)
	default:
		if done {
			r.sum.Unrecognized++
		}
	}
}

func firstOf(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
