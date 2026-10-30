package transcript

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/zhoushoujianwork/offload/internal/history"
)

// InterfaceName identifies this interface in every response.
const InterfaceName = "offload.transcript"

// ReadOptions selects one invocation and a position in its transcript. At least one of RunID and
// CallerRunID is required; when both are given they must belong to the same invocation.
type ReadOptions struct {
	RunID       string
	CallerRunID string
	Cursor      string
	Limit       int // events per page; 0 means DefaultReadLimit
	MaxBytes    int // bytes read per page; 0 means DefaultReadBytes
}

// Cursor format: t1.<capture_id>.<byte offset>.<next seq>. It is opaque to clients.
func makeCursor(captureID string, offset, nextSeq int64) string {
	return "t1." + captureID + "." + strconv.FormatInt(offset, 10) + "." + strconv.FormatInt(nextSeq, 10)
}

func parseCursor(c string) (captureID string, offset, nextSeq int64, ok bool) {
	parts := strings.Split(c, ".")
	if len(parts) != 4 || parts[0] != "t1" || len(parts[1]) != 16 {
		return
	}
	var err1, err2 error
	offset, err1 = strconv.ParseInt(parts[2], 10, 64)
	nextSeq, err2 = strconv.ParseInt(parts[3], 10, 64)
	if err1 != nil || err2 != nil || offset < 0 || nextSeq < 1 {
		return
	}
	return parts[1], offset, nextSeq, true
}

func base(avail, reason string) Response {
	return Response{SchemaVersion: SchemaVersion, Interface: InterfaceName, Availability: avail, Reason: reason, Events: []json.RawMessage{}}
}

// Read returns one bounded page of a transcript. It only reads files: it never starts a backend,
// takes a worker slot or changes any state, and it only opens the file of the run that was asked for.
func Read(o ReadOptions) Response {
	if o.Limit <= 0 {
		o.Limit = DefaultReadLimit
	}
	o.Limit = min(o.Limit, MaxReadLimit)
	if o.MaxBytes <= 0 {
		o.MaxBytes = DefaultReadBytes
	}
	o.MaxBytes = max(MaxEventBytes, min(o.MaxBytes, MaxReadBytes))

	dir := Dir()
	if _, err := os.Lstat(dir); err == nil {
		if err := checkDir(dir); err != nil {
			return base(Unreadable, err.Error())
		}
	}
	runID := o.RunID
	if runID == "" {
		matches := findByCaller(dir, o.CallerRunID)
		switch len(matches) {
		case 0:
			return missing("", o.CallerRunID)
		case 1:
			runID = matches[0].RunID
		default:
			r := base(Ambiguous, "several invocations share this caller_run_id; pick one run_id")
			r.Candidates = matches
			return r
		}
	}
	f, st, err := openRegular(Path(runID))
	if errors.Is(err, os.ErrNotExist) {
		return missing(runID, o.CallerRunID)
	}
	if err != nil {
		return base(Unreadable, err.Error())
	}
	defer f.Close()
	head, err := headOf(f)
	switch {
	case errors.Is(err, errNoHead):
		return base(NotYetAvailable, "the transcript has no complete first event yet")
	case err != nil:
		return base(Unreadable, "the first event cannot be read")
	case head.SchemaVersion > SchemaVersion || head.SchemaVersion < 1:
		return base(UnsupportedVersion, fmt.Sprintf("transcript schema_version %d is not supported; this reader supports %d", head.SchemaVersion, SchemaVersion))
	case head.RunID != runID:
		return base(Unreadable, "transcript identity does not match its file")
	case head.Backend != "claude" && head.Backend != "codex":
		return base(UnsupportedBackend, "backend "+safeText(head.Backend)+" has no transcript capture")
	case o.CallerRunID != "" && head.CallerRunID != o.CallerRunID:
		return base(IdentityMismatch, "run_id and caller_run_id do not belong to the same invocation")
	}
	size := st.Size()
	resp := base(Available, "")
	resp.Run = &RunInfo{RunID: head.RunID, CallerRunID: head.CallerRunID, Backend: head.Backend, Model: head.Model,
		Session: head.Session, CaptureID: head.CaptureID, StartedAt: head.Timestamp}

	offset, wantSeq := int64(0), int64(1)
	if o.Cursor != "" {
		capID, off, seq, ok := parseCursor(o.Cursor)
		switch {
		case !ok:
			return cursorInvalid(resp, "malformed cursor")
		case capID != head.CaptureID:
			return cursorInvalid(resp, "cursor belongs to a different capture")
		case off > size:
			return cursorInvalid(resp, "cursor is beyond the end of the transcript (file shrank)")
		}
		if off > 0 {
			var prev [1]byte
			if _, err := f.ReadAt(prev[:], off-1); err != nil || prev[0] != '\n' {
				return cursorInvalid(resp, "cursor is not on an event boundary")
			}
		}
		offset, wantSeq = off, seq
	}

	end, endSeen := tailEnd(f, size)
	buf := make([]byte, min(int64(o.MaxBytes), size-offset))
	n, _ := f.ReadAt(buf, offset)
	buf = buf[:n]
	reachedEOF := offset+int64(n) >= size

	pos, emitted, corrupt, gaps := 0, 0, 0, 0
	for pos < len(buf) && emitted < o.Limit {
		i := bytes.IndexByte(buf[pos:], '\n')
		if i < 0 {
			break
		}
		line := bytes.TrimSpace(buf[pos : pos+i])
		pos += i + 1
		var e Event
		if err := unmarshalEvent(line, &e); err != nil || e.RunID != runID {
			corrupt++
			continue
		}
		if e.Seq != wantSeq {
			gaps++
		}
		wantSeq = e.Seq + 1
		resp.Events = append(resp.Events, json.RawMessage(bytes.Clone(line)))
		emitted++
	}
	rest := buf[pos:]
	more := bytes.IndexByte(rest, '\n') >= 0 || !reachedEOF
	if !more && reachedEOF {
		resp.PartialTailBytes = len(rest)
	}
	resp.HasMore = more
	resp.NextCursor = makeCursor(head.CaptureID, offset+int64(pos), wantSeq)
	resp.CorruptLines = corrupt

	switch {
	case endSeen:
		resp.State = StateEnded
		resp.Summary = end.Capture
	case processAlive(head.PID, head.PIDStart):
		resp.State = StateRunning
	default:
		resp.State = StateAbandoned
	}
	resp.EndOfStream = resp.State != StateRunning && !resp.HasMore && resp.PartialTailBytes == 0
	resp.Completeness, resp.CompletenessReasons = assess(resp.State, end.Capture, corrupt, gaps)
	if resp.State == StateEnded && end.Capture != nil && end.Capture.ContentEvents == 0 {
		// Boundary events are still returned; there is just nothing to present.
		resp.Availability, resp.Reason = NoCapturedContent, "the invocation ended without capturable content"
	}
	if endSeen && validSession(end.Session) {
		resp.Run.Session = end.Session
	} else if emitted > 0 {
		var last Event
		if json.Unmarshal(resp.Events[emitted-1], &last) == nil && validSession(last.Session) {
			resp.Run.Session = last.Session
		}
	}
	return resp
}

func cursorInvalid(r Response, reason string) Response {
	r.Availability, r.Reason, r.NextCursor = CursorInvalid, reason+"; restart from the beginning and discard events already shown", ""
	r.Events = []json.RawMessage{}
	return r
}

// tailEnd reports whether the last complete line of the first size bytes is execution_end.
func tailEnd(f *os.File, size int64) (Event, bool) {
	const window = 32 << 10
	start := max(0, size-window)
	buf := make([]byte, size-start)
	n, _ := f.ReadAt(buf, start)
	buf = buf[:n]
	if len(buf) == 0 || buf[len(buf)-1] != '\n' {
		return Event{}, false
	}
	buf = buf[:len(buf)-1]
	i := bytes.LastIndexByte(buf, '\n')
	if i < 0 && start > 0 {
		return Event{}, false
	}
	var e Event
	if unmarshalEvent(buf[i+1:], &e) != nil || e.Type != TypeExecutionEnd {
		return Event{}, false
	}
	return e, true
}

// assess states how much of the invocation the transcript covers.
func assess(state string, sum *Summary, corrupt, seqGaps int) (string, []string) {
	var reasons []string
	if corrupt > 0 {
		reasons = append(reasons, "corrupt_lines")
	}
	if seqGaps > 0 {
		reasons = append(reasons, "sequence_gap")
	}
	switch state {
	case StateRunning:
		return InProgress, reasons
	case StateAbandoned:
		return Incomplete, append(reasons, "no_execution_end")
	}
	if sum == nil {
		return Incomplete, append(reasons, "no_summary")
	}
	add := func(cond bool, why string) {
		if cond {
			reasons = append(reasons, why)
		}
	}
	add(sum.Gaps > 0, "gaps")
	add(sum.DroppedEvents > 0, "dropped_events")
	add(sum.CaptureTruncated, "capture_size_limit")
	add(sum.WriteFailed, "write_failed")
	add(sum.Unrecognized > 0, "unrecognized_backend_events")
	add(sum.OpenToolCalls > 0, "tool_calls_without_result")
	if len(reasons) > 0 {
		return Incomplete, reasons
	}
	if sum.TruncatedEvents > 0 {
		return Bounded, []string{"truncated_fields"}
	}
	return Complete, nil
}

func safeText(s string) string {
	if len(s) > 32 {
		s = s[:32]
	}
	return strings.Map(func(r rune) rune {
		if r < 33 || r > 126 {
			return -1
		}
		return r
	}, s)
}

// missing classifies a run whose transcript file does not exist, using the run log.
func missing(runID, callerRunID string) Response {
	rs, err := history.Read(history.Path())
	if err != nil {
		return base(Unreadable, "the run log cannot be read")
	}
	var hit *history.Record
	for i := len(rs) - 1; i >= 0; i-- {
		r := rs[i]
		if (runID != "" && r.RunID == runID) || (runID == "" && callerRunID != "" && r.CallerRunID == callerRunID) {
			hit = &rs[i]
			break
		}
	}
	if hit == nil {
		return base(NotFound, "no invocation with that identity is known; it may not have finished yet, or it did not request capture")
	}
	if b := hit.Backend; b != "" && b != "claude" && b != "codex" {
		return base(UnsupportedBackend, "backend "+safeText(b)+" has no transcript capture")
	}
	r := base(CaptureDisabled, "the invocation did not request -capture-transcript, or predates the feature")
	switch hit.Transcript {
	case "captured":
		r.Availability, r.Reason = Expired, "the transcript was removed by retention"
	case "not_created":
		r.Availability, r.Reason = NoCapturedContent, "the invocation failed before capture started"
	case "failed":
		r.Availability, r.Reason = Unreadable, "the transcript could not be created"
	}
	return r
}

// findByCaller scans the stored transcripts' first events for a caller_run_id. Only the newest
// RetainFiles files are looked at, and only their first event is read.
func findByCaller(dir, caller string) []ListEntry {
	if caller == "" {
		return nil
	}
	var out []ListEntry
	for _, f := range storeFiles(dir) {
		if e, ok := entryOf(f); ok && e.CallerRunID == caller {
			out = append(out, e)
		}
	}
	return out
}

func entryOf(f storeFile) (ListEntry, bool) {
	head, err := readHead(f.path)
	if err != nil {
		return ListEntry{}, false
	}
	return ListEntry{RunID: head.RunID, CallerRunID: head.CallerRunID, CaptureID: head.CaptureID, StartedAt: head.Timestamp,
		Availability: Available, SizeBytes: f.size}, true
}

// List returns metadata of stored transcripts, newest first, optionally for one caller_run_id.
// It never returns transcript content.
func List(caller string, limit int) ListResponse {
	if limit <= 0 || limit > MaxListEntries {
		limit = MaxListEntries
	}
	out := ListResponse{SchemaVersion: SchemaVersion, Interface: InterfaceName, Transcripts: []ListEntry{}}
	dir := Dir()
	if checkDir(dir) != nil {
		return out
	}
	for _, f := range storeFiles(dir) {
		e, ok := entryOf(f)
		if !ok || (caller != "" && e.CallerRunID != caller) {
			continue
		}
		if len(out.Transcripts) == limit {
			out.Truncated = true
			break
		}
		out.Transcripts = append(out.Transcripts, e)
	}
	return out
}
