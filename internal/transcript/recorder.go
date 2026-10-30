package transcript

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/zhoushoujianwork/offload/internal/proc"
)

// Identity names the invocation a transcript belongs to.
type Identity struct {
	RunID       string
	CallerRunID string
	Caller      string
	Backend     string // claude | codex
	Model       string
	Session     string // the session being resumed, if any
}

// EndInfo is how the invocation ended.
type EndInfo struct {
	Status           string
	ExitCode         *int
	ErrorKind        string
	StopReason       string
	TerminationClass string
	Session          string
}

// Recorder appends the events of one invocation to its transcript file. Nothing it does can fail
// the invocation: a write error marks the capture incomplete and stops further writes.
type Recorder struct {
	mu       sync.Mutex
	w        io.WriteCloser
	id       Identity
	redact   *Redactor
	captureI string
	session  string
	attempt  int
	seq      int64
	size     int64
	sum      Summary
	gapSeen  bool
	closed   bool
	failed   bool
	now      func() time.Time
	tools    map[string]string // tool_call_id -> tool name, for results
	open     map[string]bool   // tool calls without a result yet
	codexSeq map[string]bool   // codex item ids whose tool_call was already written
}

// Open creates the transcript file for a run. The file is created exclusively with mode 0600 in a
// private directory; a symlink, an existing file or a directory open to other users is an error.
func Open(id Identity, red *Redactor) (*Recorder, error) {
	if !ValidRunID(id.RunID) {
		return nil, errors.New("invalid run id for transcript")
	}
	dir := Dir()
	if err := prepareDir(dir); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(Path(id.RunID), os.O_WRONLY|os.O_CREATE|os.O_EXCL|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	// Retention runs once per new capture, counting the new file, and never removes it.
	Prune(dir, id.RunID, time.Now())
	return newRecorder(f, id, red)
}

func newRecorder(w io.WriteCloser, id Identity, red *Redactor) (*Recorder, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, err
	}
	if red == nil {
		red = NewRedactor()
	}
	r := &Recorder{w: w, id: id, redact: red, captureI: hex.EncodeToString(b[:]), now: time.Now,
		tools: map[string]string{}, open: map[string]bool{}, codexSeq: map[string]bool{}}
	if validSession(id.Session) {
		r.session = id.Session
	}
	pid := os.Getpid()
	r.emitLocked(Event{Type: TypeCaptureStart, CaptureID: r.captureI, PID: pid, PIDStart: proc.StartToken(pid), ResumedFrom: id.Session}, true)
	if r.failed {
		w.Close()
		return nil, errors.New("write transcript failed")
	}
	return r, nil
}

// CaptureID is the random identity of this capture, copied into every cursor.
func (r *Recorder) CaptureID() string { return r.captureI }

// AddSecrets registers credential values to remove from everything written from now on.
func (r *Recorder) AddSecrets(values ...string) {
	if r != nil {
		r.redact.Add(values...)
	}
}

// BeginAttempt marks the launch of one backend process and records the prompt that process is given.
// kind is AttemptKindRun, or AttemptKindReport for offload's tool-less report step.
func (r *Recorder) BeginAttempt(kind, task string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.attempt++
	r.emitLocked(Event{Type: TypeAttemptStart, AttemptKind: kind}, false)
	origin := "task"
	if kind == AttemptKindReport {
		origin = "offload_salvage"
	}
	if strings.TrimSpace(task) != "" {
		r.emitLocked(Event{Type: TypeMessage, Role: "user", Origin: origin, Text: task}, false)
	}
}

// Skipped records backend output that could not be captured, so a reader sees a gap instead of a
// transcript that looks complete.
func (r *Recorder) Skipped(reason string, n int) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gapLocked(reason, n)
}

func (r *Recorder) gapLocked(reason string, n int) {
	r.sum.Gaps++
	r.sum.DroppedBytes += int64(n)
	r.emitLocked(Event{Type: TypeGap, Reason: reason, Bytes: n}, true)
}

// End writes execution_end and closes the file. It is safe to call once; later calls do nothing.
func (r *Recorder) End(info EndInfo) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	for id := range r.open {
		_ = id
		r.sum.OpenToolCalls++
	}
	if info.Session != "" && validSession(info.Session) {
		r.session = info.Session
	}
	r.sum.WriteFailed = r.failed
	e := Event{Type: TypeExecutionEnd, Status: info.Status, ExitCode: info.ExitCode, ErrorKind: info.ErrorKind,
		StopReason: info.StopReason, TerminationClass: info.TerminationClass}
	s := r.sum
	s.Events = int(r.seq) + 1 // counts execution_end itself
	e.Capture = &s
	r.emitLocked(e, true)
	r.closed = true
	r.w.Close()
}

func isContent(t string) bool {
	return t == TypeMessage || t == TypeToolCall || t == TypeToolResult || t == TypeBackendError
}

// emitLocked numbers, sanitizes, bounds and writes one event. Boundary events (force) may use the
// space reserved for execution_end.
func (r *Recorder) emitLocked(e Event, force bool) {
	if r.closed || r.failed {
		return
	}
	e.SchemaVersion = SchemaVersion
	e.Timestamp = r.now().UTC().Format(time.RFC3339Nano)
	e.RunID, e.CallerRunID, e.Caller = r.id.RunID, r.id.CallerRunID, r.id.Caller
	e.Backend, e.Model, e.Session, e.Attempt = r.id.Backend, r.id.Model, r.session, r.attempt
	if e.Type != TypeExecutionEnd {
		r.sanitize(&e)
	}
	e.Seq = r.seq + 1
	b := encode(&e)
	if e.Type != TypeExecutionEnd && len(b) > MaxEventBytes {
		shrinkToFit(&e)
		b = encode(&e)
	}
	limit := int64(MaxCaptureBytes - EndReserveBytes)
	if force {
		limit = MaxCaptureBytes
	}
	if e.Type != TypeExecutionEnd && r.size+int64(len(b)) > limit {
		r.sum.DroppedEvents++
		r.sum.DroppedBytes += int64(len(b))
		r.sum.CaptureTruncated = true
		if !r.gapSeen {
			r.gapSeen = true
			g := Event{Type: TypeGap, Reason: "capture_size_limit"}
			r.sum.Gaps++
			r.emitLocked(g, true)
		}
		return
	}
	n, err := r.w.Write(b)
	if err == nil && n != len(b) {
		err = io.ErrShortWrite
	}
	if err != nil {
		r.failed = true
		r.sum.WriteFailed = true
		return
	}
	r.seq++
	r.size += int64(len(b))
	r.sum.Events = int(r.seq)
	if isContent(e.Type) {
		r.sum.ContentEvents++
	}
	if len(e.Truncated) > 0 {
		r.sum.TruncatedEvents++
	}
	if e.Redacted {
		r.sum.Redactions++
	}
}

func encode(e *Event) []byte {
	b, err := json.Marshal(e)
	if err != nil {
		// Cannot happen for these field types; keep the stream numbered and visibly damaged.
		b = []byte(`{"schema_version":1,"type":"gap","reason":"encode_failed"}`)
	}
	return append(b, '\n')
}

// sanitize removes credentials, then cuts each free-text field to its bound.
func (r *Recorder) sanitize(e *Event) {
	red := 0
	clean := func(name string, p *string, max int) {
		if *p == "" {
			return
		}
		s, n := r.redact.Apply(strings.ToValidUTF8(*p, "�"))
		red += n
		if len(s) > max {
			if e.Truncated == nil {
				e.Truncated = map[string]int{}
			}
			e.Truncated[name] = len(s)
			s = cutUTF8(s, max)
		}
		*p = s
	}
	clean("text", &e.Text, MaxMessageBytes)
	clean("arguments", &e.Arguments, MaxArgumentBytes)
	clean("output", &e.Output, MaxToolOutputBytes)
	clean("tool_name", &e.ToolName, 256)
	clean("reason", &e.Reason, 256)
	if red > 0 {
		e.Redacted = true
	}
}

// shrinkToFit halves the largest free-text field until the serialized event fits MaxEventBytes.
// JSON escaping can multiply a field's size, so the per-field bounds alone do not guarantee it.
func shrinkToFit(e *Event) {
	for range 16 {
		if len(encode(e)) <= MaxEventBytes {
			return
		}
		fields := map[string]*string{"output": &e.Output, "text": &e.Text, "arguments": &e.Arguments}
		best, name := 0, ""
		for k, p := range fields {
			if len(*p) > best {
				best, name = len(*p), k
			}
		}
		if name == "" {
			return
		}
		if e.Truncated == nil {
			e.Truncated = map[string]int{}
		}
		if _, ok := e.Truncated[name]; !ok {
			e.Truncated[name] = best
		}
		*fields[name] = cutUTF8(*fields[name], best/2)
	}
}

func cutUTF8(s string, max int) string {
	if len(s) <= max {
		return s
	}
	for max > 0 && !utf8.RuneStart(s[max]) {
		max--
	}
	return s[:max]
}

func unmarshalEvent(b []byte, e *Event) error {
	if err := json.Unmarshal(bytes.TrimSpace(b), e); err != nil {
		return err
	}
	if e.Seq < 1 || e.Type == "" {
		return errors.New("not a transcript event")
	}
	return nil
}

// validSession mirrors the bounded session identifier rule of the metadata events.
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

func (r *Recorder) setSession(s string) {
	if validSession(s) {
		r.session = s
	}
}
