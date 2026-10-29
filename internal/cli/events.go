package cli

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/zhoushoujianwork/offload/internal/history"
)

// Events contain only correlation and allowlisted activity metadata, never worker payloads.
type supervisorEvent struct {
	StopReason string `json:"stop_reason,omitempty"`
	// TerminationClass is canceled, interrupted or killed on a final event of a worker that ended
	// without a result.
	TerminationClass string  `json:"termination_class,omitempty"`
	IdleTimeout      string  `json:"idle_timeout,omitempty"`
	SchemaVersion    int     `json:"schema_version"`
	Type             string  `json:"type"`
	RunID            string  `json:"run_id"`
	CallerRunID      string  `json:"caller_run_id"`
	Seq              int     `json:"seq"`
	Timestamp        string  `json:"timestamp"`
	Session          string  `json:"session"`
	Backend          string  `json:"backend"`
	Model            string  `json:"model"`
	NativeType       string  `json:"native_type,omitempty"`
	ToolCategory     string  `json:"tool_category,omitempty"`
	Status           string  `json:"status,omitempty"`
	ExitCode         *int    `json:"exit_code,omitempty"`
	ErrorKind        *string `json:"error_kind,omitempty"`
	Caller           string  `json:"caller"`
}
type eventWriter struct {
	mu   sync.Mutex
	w    io.Writer
	seq  int
	base supervisorEvent
	err  error
}

func openEvents(path string, rec history.Record) (*eventWriter, *os.File, error) {
	if !filepath.IsAbs(path) {
		return nil, nil, errors.New("events-file must be absolute")
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, nil, err
	}
	e := &eventWriter{w: f, base: supervisorEvent{SchemaVersion: 1, RunID: rec.RunID, CallerRunID: safeMeta(rec.CallerRunID), Caller: safeCaller(rec.Caller), Session: safeMeta(rec.Session), Backend: safeMeta(rec.Backend), Model: safeMeta(rec.Model)}}
	return e, f, nil
}
func (e *eventWriter) emit(kind, native, tool string, rec *history.Record) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.err != nil {
		return e.err
	}
	if kind == "progress" && e.seq >= 10001 {
		return nil
	}
	if rec != nil {
		e.base.Backend = safeMeta(rec.Backend)
		e.base.Model = safeMeta(rec.Model)
		e.base.Session = safeMeta(rec.Session)
	}
	e.seq++
	v := e.base
	v.Type = kind
	v.Seq = e.seq
	v.Timestamp = time.Now().UTC().Format(time.RFC3339Nano)
	v.NativeType = native
	v.ToolCategory = tool
	if rec != nil {

		if kind == "final" {
			v.StopReason = rec.StopReason
			v.TerminationClass = rec.TerminationClass()
			v.IdleTimeout = rec.IdleTimeout
			v.Status = rec.Status
			v.ExitCode = rec.ExitCode
			kind := safeKind(rec.ErrorKind)
			v.ErrorKind = &kind
		}
	}
	b, err := json.Marshal(v)
	if err == nil {
		b = append(b, '\n')
		var n int
		n, err = e.w.Write(b)
		if err == nil && n != len(b) {
			err = io.ErrShortWrite
		}
	}
	e.err = err
	return err
}
func (e *eventWriter) progress(line []byte) error {
	var v struct {
		Type    string `json:"type"`
		Session string `json:"session_id"`
		Thread  string `json:"thread_id"`
		Item    struct {
			Type string `json:"type"`
		} `json:"item"`
		Message json.RawMessage `json:"message"`
	}
	if json.Unmarshal(line, &v) != nil {
		return nil
	}
	native := "other"
	switch v.Type {
	case "system", "assistant", "user", "result", "thread.started", "turn.started", "turn.completed", "turn.failed", "item.started", "item.completed", "item.updated", "error":
		native = v.Type
	}
	tool := ""
	switch v.Item.Type {
	case "command_execution":
		tool = "shell"
	case "file_change":
		tool = "file"
	case "web_search":
		tool = "web"
	case "mcp_tool_call":
		tool = "mcp"
	}
	if tool == "" {
		var message struct {
			Content []struct {
				Type string `json:"type"`
				Name string `json:"name"`
			} `json:"content"`
		}
		_ = json.Unmarshal(v.Message, &message)
		for _, block := range message.Content {
			if block.Type != "tool_use" {
				continue
			}
			switch block.Name {
			case "Bash":
				tool = "shell"
			case "Read", "Grep", "Glob", "Edit", "Write":
				tool = "file"
			case "WebSearch", "WebFetch":
				tool = "web"
			default:
				tool = "other"
			}
			break
		}
	}
	// Session identifiers are bounded; reject payload-like values.
	session := v.Session
	if session == "" {
		session = v.Thread
	}
	if validSession(session) {
		e.mu.Lock()
		e.base.Session = session
		e.mu.Unlock()
	}
	return e.emit("progress", native, tool, nil)
}
func validSession(s string) bool {
	if len(s) == 0 || len(s) > 128 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func safeMeta(s string) string {
	if len(s) > 128 {
		return ""
	}
	for _, c := range s {
		if c < 33 || c > 126 {
			return ""
		}
	}
	return s
}

func safeKind(kind string) string {
	switch kind {
	case "", "configuration", "unsupported_tools", "launch", "execution", "timeout", "canceled", "events_io", "capacity", "rate_limit":
		return kind
	default:
		return "execution"
	}
}

// safeCaller reports only a canonical caller; anything else is unspecified.
func safeCaller(s string) string {
	if c, err := history.NormalizeCaller(s); err == nil {
		return c
	}
	return history.CallerUnspecified
}
