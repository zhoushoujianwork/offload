// Package transcript captures and serves the presentable content of one offload invocation:
// user and assistant messages, tool calls and their results, and the invocation boundaries.
//
// Capture is opt-in per invocation (-capture-transcript). Each invocation writes its own private
// NDJSON file keyed by its run ID under the offload state directory. Reading is a separate,
// read-only operation on that file: it never starts a provider session or takes a worker slot, and
// it never looks at the private session files of Claude Code or Codex.
package transcript

import "encoding/json"

// SchemaVersion is the schema_version of every event and response document.
const SchemaVersion = 1

// Event types written to a transcript.
const (
	TypeCaptureStart  = "capture_start" // first event of every file; carries capture_id
	TypeAttemptStart  = "attempt_start" // a backend process is about to be launched
	TypeMessage       = "message"       // a user or assistant message
	TypeToolCall      = "tool_call"     // a tool call started, with its arguments when available
	TypeToolResult    = "tool_result"   // output, result or error of a tool call
	TypeBackendError  = "backend_error" // an error reported by the backend itself
	TypeGap           = "gap"           // content that could not be captured; coverage is incomplete
	TypeExecutionEnd  = "execution_end" // last event; absent when offload died before writing it
	AttemptKindRun    = "run"           // the requested task
	AttemptKindReport = "salvage"       // the tool-less report step offload runs after a stopped Claude worker
)

// Bounds. Every value is a hard limit, not a target.
const (
	MaxMessageBytes    = 64 << 10  // text of one message, before JSON escaping
	MaxArgumentBytes   = 16 << 10  // tool arguments
	MaxToolOutputBytes = 32 << 10  // tool output or result
	MaxEventBytes      = 128 << 10 // one serialized event including its newline
	MaxCaptureBytes    = 8 << 20   // size of one transcript file
	EndReserveBytes    = 16 << 10  // kept free so execution_end can always be written
	MaxLineBytes       = 4 << 20   // longest backend output line that is parsed
	maxIDBytes         = 256

	DefaultReadLimit = 200
	MaxReadLimit     = 1000
	DefaultReadBytes = 256 << 10
	MaxReadBytes     = 1 << 20
	MaxListEntries   = 100
)

// Event is one line of a transcript file. Fields not used by an event type are omitted.
type Event struct {
	SchemaVersion int    `json:"schema_version"`
	Seq           int64  `json:"seq"` // 1, 2, 3...: strictly increasing, no holes in a healthy file
	Timestamp     string `json:"ts"`
	RunID         string `json:"run_id"`
	CallerRunID   string `json:"caller_run_id"`
	Caller        string `json:"caller"`
	Backend       string `json:"backend"`
	Model         string `json:"model"`
	Session       string `json:"session"` // native session ID once known; "" before
	Attempt       int    `json:"attempt"` // 0 before the first attempt_start
	Type          string `json:"type"`

	CaptureID   string `json:"capture_id,omitempty"`
	PID         int    `json:"pid,omitempty"`       // capture_start: the offload process that writes this file
	PIDStart    string `json:"pid_start,omitempty"` // capture_start: its start time, to tell a recycled PID
	AttemptKind string `json:"attempt_kind,omitempty"`

	Role      string `json:"role,omitempty"`   // message: user | assistant
	Origin    string `json:"origin,omitempty"` // message: task | offload_salvage | backend
	MessageID string `json:"message_id,omitempty"`
	Text      string `json:"text,omitempty"`

	ToolCallID   string `json:"tool_call_id,omitempty"`
	ToolName     string `json:"tool_name,omitempty"`
	ToolCategory string `json:"tool_category,omitempty"`
	Arguments    string `json:"arguments,omitempty"`
	Output       string `json:"output,omitempty"`
	IsError      *bool  `json:"is_error,omitempty"`
	ExitCode     *int   `json:"exit_code,omitempty"` // tool_result for a command; execution_end for the invocation

	Reason string `json:"reason,omitempty"` // gap
	Bytes  int    `json:"bytes,omitempty"`  // gap: size of the content that was dropped

	Status           string   `json:"status,omitempty"` // execution_end: ok | error | timeout
	ErrorKind        string   `json:"error_kind,omitempty"`
	StopReason       string   `json:"stop_reason,omitempty"`
	TerminationClass string   `json:"termination_class,omitempty"`
	Capture          *Summary `json:"capture,omitempty"` // execution_end

	// Truncated maps a field name to its original size in bytes when the field was cut to fit a bound.
	Truncated   map[string]int `json:"truncated,omitempty"`
	Omitted     []string       `json:"omitted,omitempty"` // content kinds that exist but are not text (for example image)
	Redacted    bool           `json:"redacted,omitempty"`
	ResumedFrom string         `json:"resumed_from,omitempty"` // capture_start: the session this invocation resumes
}

// Summary is written into execution_end and lets a client tell a complete transcript from a
// partial one without having read every page.
type Summary struct {
	Events           int   `json:"events"`
	ContentEvents    int   `json:"content_events"` // message, tool_call, tool_result and backend_error
	Gaps             int   `json:"gaps"`
	TruncatedEvents  int   `json:"truncated_events"`
	DroppedEvents    int   `json:"dropped_events"`
	DroppedBytes     int64 `json:"dropped_bytes"`
	Redactions       int   `json:"redactions"`
	CaptureTruncated bool  `json:"capture_truncated"` // MaxCaptureBytes was reached; later content is missing
	WriteFailed      bool  `json:"write_failed"`
	SkippedBlocks    int   `json:"skipped_blocks"`      // backend blocks left out by design (reasoning, plans)
	Unrecognized     int   `json:"unrecognized_events"` // backend events this version cannot map; coverage is unknown
	OpenToolCalls    int   `json:"open_tool_calls"`     // tool calls that never received a result (interrupted run)
}

// Availability values of a read response.
const (
	Available          = "available"
	NotYetAvailable    = "not_yet_available"   // the file exists but its first event is not complete yet
	NoCapturedContent  = "no_captured_content" // the invocation ended and produced no capturable content
	CaptureDisabled    = "capture_disabled"    // the run exists but capture was not requested (or predates the feature)
	Expired            = "expired"             // removed by retention
	Unreadable         = "unreadable"          // unsafe path or permissions, I/O error, or damaged data
	UnsupportedBackend = "unsupported_backend" // the run used a backend this interface cannot capture
	UnsupportedVersion = "unsupported_version" // the file was written by a newer schema than this reader knows
	NotFound           = "not_found"
	Ambiguous          = "ambiguous" // a caller-run-id matches several invocations; see candidates
	CursorInvalid      = "cursor_invalid"
	IdentityMismatch   = "identity_mismatch" // run-id and caller-run-id do not belong together
)

// States of an invocation seen by a reader.
const (
	StateRunning   = "running" // the writer process is alive; no attempt_start yet means the backend has not launched
	StateEnded     = "ended"
	StateAbandoned = "abandoned" // no execution_end and nothing alive: offload was killed or could not write
)

// Completeness values.
const (
	InProgress = "in_progress"
	Complete   = "complete"   // every event captured, nothing cut
	Bounded    = "bounded"    // every event captured, but some fields were cut (see truncated)
	Incomplete = "incomplete" // content is missing
)

// Response is the document `offload transcript read --json` prints.
type Response struct {
	SchemaVersion int    `json:"schema_version"`
	Interface     string `json:"interface"`
	Availability  string `json:"availability"`
	Reason        string `json:"reason,omitempty"`

	Run   *RunInfo `json:"run,omitempty"`
	State string   `json:"state,omitempty"`

	Events           []json.RawMessage `json:"events"`
	NextCursor       string            `json:"next_cursor,omitempty"` // pass back as --cursor; absent when no cursor applies
	HasMore          bool              `json:"has_more"`
	EndOfStream      bool              `json:"end_of_stream"`
	PartialTailBytes int               `json:"partial_tail_bytes,omitempty"`

	Completeness        string   `json:"completeness,omitempty"`
	CompletenessReasons []string `json:"completeness_reasons,omitempty"`
	Summary             *Summary `json:"summary,omitempty"` // from execution_end, once the invocation ended
	CorruptLines        int      `json:"corrupt_lines,omitempty"`

	Candidates []ListEntry `json:"candidates,omitempty"`
}

// RunInfo identifies the invocation a response belongs to.
type RunInfo struct {
	RunID       string `json:"run_id"`
	CallerRunID string `json:"caller_run_id"`
	Backend     string `json:"backend"`
	Model       string `json:"model"`
	Session     string `json:"session"`
	CaptureID   string `json:"capture_id"`
	StartedAt   string `json:"started_at"`
}

// ListEntry is one transcript known to the store.
type ListEntry struct {
	RunID        string `json:"run_id"`
	CallerRunID  string `json:"caller_run_id"`
	CaptureID    string `json:"capture_id,omitempty"`
	StartedAt    string `json:"started_at"`
	Availability string `json:"availability"` // available | expired | unreadable
	SizeBytes    int64  `json:"size_bytes,omitempty"`
}

// ListResponse is the document `offload transcript list --json` prints.
type ListResponse struct {
	SchemaVersion int         `json:"schema_version"`
	Interface     string      `json:"interface"`
	Transcripts   []ListEntry `json:"transcripts"`
	Truncated     bool        `json:"truncated"` // more than MaxListEntries matched; only the newest are shown
}
