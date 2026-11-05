// Package runevents is the sanitized structured run event stream shared by the CLI
// -events-file and pkg/execution. Events carry correlation and allowlisted
// activity metadata only, never worker payloads.
package runevents

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

// Event is one NDJSON line of the sanitized event stream.
type Event struct {
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

// Writer numbers and sanitizes events. Each event is written to W (when non-nil)
// and then passed to Observe (when non-nil). The first error from either is sticky.
type Writer struct {
	mu      sync.Mutex
	W       io.Writer
	Observe func(Event) error
	seq     int
	Base    Event
	err     error
}

// New returns a Writer whose base identity comes from rec.
func New(w io.Writer, observe func(Event) error, rec history.Record) *Writer {
	return &Writer{W: w, Observe: observe, Base: Event{SchemaVersion: 1, RunID: rec.RunID, CallerRunID: SafeMeta(rec.CallerRunID), Caller: SafeCaller(rec.Caller), Session: SafeMeta(rec.Session), Backend: SafeMeta(rec.Backend), Model: SafeMeta(rec.Model)}}
}

// Open exclusively creates an absolute events file.
func Open(path string, rec history.Record) (*Writer, *os.File, error) {
	if !filepath.IsAbs(path) {
		return nil, nil, errors.New("events-file must be absolute")
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, nil, err
	}
	return New(f, nil, rec), f, nil
}

// Emit records one event. rec supplies start and final metadata.
func (e *Writer) Emit(kind, native, tool string, rec *history.Record) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.err != nil {
		return e.err
	}
	if kind == "progress" && e.seq >= 10001 {
		return nil
	}
	if rec != nil {
		e.Base.Backend = SafeMeta(rec.Backend)
		e.Base.Model = SafeMeta(rec.Model)
		e.Base.Session = SafeMeta(rec.Session)
	}
	e.seq++
	v := e.Base
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
			kind := SafeKind(rec.ErrorKind)
			v.ErrorKind = &kind
		}
	}
	var err error
	if e.W != nil {
		var b []byte
		b, err = json.Marshal(v)
		if err == nil {
			b = append(b, '\n')
			var n int
			n, err = e.W.Write(b)
			if err == nil && n != len(b) {
				err = io.ErrShortWrite
			}
		}
	}
	if err == nil && e.Observe != nil {
		err = e.Observe(v)
	}
	e.err = err
	return err
}

// Progress records an allowlisted progress event for one native output line.
func (e *Writer) Progress(line []byte) error {
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
		e.Base.Session = session
		e.mu.Unlock()
	}
	return e.Emit("progress", native, tool, nil)
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

func SafeMeta(s string) string {
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

func SafeKind(kind string) string {
	switch kind {
	case "", "configuration", "unsupported_tools", "launch", "execution", "timeout", "canceled", "events_io", "capacity", "rate_limit":
		return kind
	default:
		return "execution"
	}
}

// SafeCaller reports only a canonical caller; anything else is unspecified.
func SafeCaller(s string) string {
	if c, err := history.NormalizeCaller(s); err == nil {
		return c
	}
	return history.CallerUnspecified
}
