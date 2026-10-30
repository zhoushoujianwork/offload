package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/zhoushoujianwork/offload/internal/transcript"
)

const transcriptUsage = "usage: offload transcript read --json (--run-id <id> | --caller-run-id <id>) [--cursor <c>] [--limit N] [--max-bytes N] | " +
	"offload transcript list --json [--caller-run-id <id>] [--limit N] | offload transcript capabilities --json"

// capabilities is the document `offload transcript capabilities --json` prints. It is static: it
// describes this build, not any particular run. Per-run availability comes from `transcript read`.
type capabilitiesOutput struct {
	SchemaVersion int                 `json:"schema_version"`
	Interface     string              `json:"interface"`
	Capture       capabilityCapture   `json:"capture"`
	Backends      []capabilityBackend `json:"backends"`
	EventTypes    []string            `json:"event_types"`
	Limits        map[string]int64    `json:"limits"`
	Retention     map[string]any      `json:"retention"`
	Redaction     capabilityRedaction `json:"redaction"`
	Availability  []string            `json:"availability_values"`
	Completeness  []string            `json:"completeness_values"`
	Reading       capabilityReading   `json:"reading"`
	Unsupported   []string            `json:"not_provided"`
}

type capabilityCapture struct {
	Default     string `json:"default"`
	EnableWith  string `json:"enable_with"`
	Storage     string `json:"storage"`
	Permissions string `json:"permissions"`
}

type capabilityBackend struct {
	Backend    string   `json:"backend"`
	Source     string   `json:"source"`
	Supported  bool     `json:"supported"`
	TextDeltas bool     `json:"text_deltas"`
	Content    []string `json:"content"`
	Notes      string   `json:"notes"`
}

type capabilityRedaction struct {
	Applied bool     `json:"applied"`
	Removes []string `json:"removes"`
	Limits  string   `json:"limits"`
}

type capabilityReading struct {
	StartsBackend bool   `json:"starts_backend"`
	TakesSlot     bool   `json:"takes_worker_slot"`
	Cursor        string `json:"cursor"`
	ExitCode      string `json:"exit_code"`
}

func capabilities() capabilitiesOutput {
	return capabilitiesOutput{
		SchemaVersion: transcript.SchemaVersion,
		Interface:     transcript.InterfaceName,
		Capture: capabilityCapture{Default: "off", EnableWith: "-capture-transcript",
			Storage: "<state dir>/offload/transcripts/<run_id>.ndjson", Permissions: "directory 0700, file 0600, exclusive create, symlinks rejected"},
		Backends: []capabilityBackend{
			{Backend: "claude", Source: "claude -p --output-format stream-json --verbose (not ACP)", Supported: true, TextDeltas: false,
				Content: []string{"user_message", "assistant_message", "tool_call", "tool_result", "backend_error"},
				Notes:   "Complete messages per content block. offload does not request partial messages, so there are no text deltas. Thinking blocks are counted in skipped_blocks, never stored."},
			{Backend: "codex", Source: "codex exec --json (not ACP)", Supported: true, TextDeltas: false,
				Content: []string{"user_message", "assistant_message", "tool_call", "tool_result", "backend_error"},
				Notes:   "Items arrive on item.completed: agent messages, command execution with aggregated output and exit code, MCP calls, file changes, web searches. Reasoning and plan items are counted in skipped_blocks, never stored."},
		},
		EventTypes: []string{transcript.TypeCaptureStart, transcript.TypeAttemptStart, transcript.TypeMessage, transcript.TypeToolCall,
			transcript.TypeToolResult, transcript.TypeBackendError, transcript.TypeGap, transcript.TypeExecutionEnd},
		Limits: map[string]int64{"message_bytes": transcript.MaxMessageBytes, "tool_arguments_bytes": transcript.MaxArgumentBytes,
			"tool_output_bytes": transcript.MaxToolOutputBytes, "event_bytes": transcript.MaxEventBytes, "transcript_bytes": transcript.MaxCaptureBytes,
			"read_default_events": transcript.DefaultReadLimit, "read_max_events": transcript.MaxReadLimit,
			"read_default_bytes": transcript.DefaultReadBytes, "read_max_bytes": transcript.MaxReadBytes, "list_max_entries": transcript.MaxListEntries},
		Retention: map[string]any{"max_age_days": int(transcript.RetainFor.Hours() / 24), "max_files": transcript.RetainFiles,
			"max_total_bytes": transcript.RetainBytes, "applied": "when a new capture starts; a transcript whose writer is alive is never removed"},
		Redaction: capabilityRedaction{Applied: true,
			Removes: []string{"the worker token of the profile", "values of environment variables whose names contain KEY, TOKEN, SECRET, PASSWORD or CREDENTIAL", "Codex auth.json credentials", "common credential shapes (sk-, ghp_, github_pat_, xox*-, AKIA, JWT, bearer tokens, PEM private keys, name=value pairs for credential-like names)"},
			Limits:  "Best effort. Source code, prompts, file contents and command output are stored as the worker produced them. A secret that offload does not know and that has no recognizable shape is stored. Treat a transcript as sensitive data."},
		Availability: []string{transcript.Available, transcript.NotYetAvailable, transcript.NoCapturedContent, transcript.CaptureDisabled, transcript.Expired,
			transcript.Unreadable, transcript.UnsupportedBackend, transcript.UnsupportedVersion, transcript.NotFound, transcript.Ambiguous, transcript.CursorInvalid, transcript.IdentityMismatch},
		Completeness: []string{transcript.InProgress, transcript.Complete, transcript.Bounded, transcript.Incomplete},
		Reading: capabilityReading{Cursor: "opaque; pass next_cursor back unchanged. A cursor from another capture, a malformed one, or one past the end of the file yields cursor_invalid.",
			ExitCode: "0 whenever a document was printed, including every availability value; 2 for usage errors"},
		Unsupported: []string{"text deltas", "reasoning or thinking content", "retroactive transcripts for runs started without -capture-transcript",
			"images and other non-text tool output (listed in omitted)", "reading the private session files of Claude Code or Codex"},
	}
}

func cmdTranscript(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return fail(stderr, errors.New(transcriptUsage))
	}
	fs := flag.NewFlagSet("offload transcript "+args[0], flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	asJSON := fs.Bool("json", false, "")
	runID := fs.String("run-id", "", "")
	callerRunID := fs.String("caller-run-id", "", "")
	cursor := fs.String("cursor", "", "")
	limit := fs.Int("limit", 0, "")
	maxBytes := fs.Int("max-bytes", 0, "")
	switch args[0] {
	case "read", "list", "capabilities":
	default:
		return fail(stderr, errors.New(transcriptUsage))
	}
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 {
		return fail(stderr, errors.New(transcriptUsage))
	}
	if !*asJSON {
		return fail(stderr, fmt.Errorf("transcript %s needs --json", args[0]))
	}
	if *limit < 0 || *maxBytes < 0 {
		return fail(stderr, errors.New("--limit and --max-bytes must not be negative"))
	}
	if *callerRunID != "" && safeMeta(*callerRunID) != *callerRunID {
		return fail(stderr, errors.New("--caller-run-id must be 1-128 printable characters without spaces"))
	}
	switch args[0] {
	case "capabilities":
		return writeTranscriptJSON(stdout, stderr, capabilities())
	case "list":
		return writeTranscriptJSON(stdout, stderr, transcript.List(*callerRunID, *limit))
	}
	if (*runID == "") == (*callerRunID == "") && *runID == "" {
		return fail(stderr, errors.New("transcript read needs --run-id or --caller-run-id"))
	}
	if *runID != "" && !transcript.ValidRunID(*runID) {
		return fail(stderr, errors.New("--run-id must be a 32-character lowercase hex offload run ID"))
	}
	return writeTranscriptJSON(stdout, stderr, transcript.Read(transcript.ReadOptions{RunID: *runID, CallerRunID: *callerRunID,
		Cursor: *cursor, Limit: *limit, MaxBytes: *maxBytes}))
}

// writeTranscriptJSON encodes to memory first, so a failure never leaves a partial document.
func writeTranscriptJSON(stdout, stderr io.Writer, v any) int {
	b, err := json.Marshal(v)
	if err != nil {
		return fail(stderr, errors.New("encode transcript JSON failed"))
	}
	if _, err := stdout.Write(append(b, '\n')); err != nil {
		return fail(stderr, errors.New("write transcript JSON failed"))
	}
	return 0
}
