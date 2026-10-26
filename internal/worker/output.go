package worker

import (
	"bytes"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// maxRawKept bounds the raw stdout kept for diagnostics. It only matters when the worker printed
	// no recognizable events, where the raw text is the best error evidence there is.
	maxRawKept = 256 << 10
	// maxLineKept bounds one event line. The final report is a single line, so this is generous;
	// a longer line is dropped rather than buffered without limit.
	maxLineKept = 16 << 20
)

// eventParser folds a worker's newline-delimited events into a streamSummary one line at a time,
// so the full output never has to be kept or re-parsed.
type eventParser interface {
	feed(line []byte)
	summary() streamSummary
}

// parseAll feeds a complete buffer to p. It is the one-shot form of what activityWriter does.
func parseAll(p eventParser, out []byte) streamSummary {
	for _, line := range bytes.Split(out, []byte("\n")) {
		if line = bytes.TrimSpace(line); len(line) > 0 {
			p.feed(line)
		}
	}
	return p.summary()
}

// activityWriter parses the worker's stdout as it arrives and remembers when it last wrote. Memory
// stays bounded however much the worker prints: events are folded into the summary and
// discarded, and only a capped amount of raw text is kept.
type activityWriter struct {
	mu        sync.Mutex
	parser    eventParser
	pending   []byte // bytes of the current, unfinished line
	skipping  bool   // inside a line longer than maxLineKept; discard up to its newline
	raw       []byte
	rawCapped bool
	last      atomic.Int64 // unix nanoseconds of the latest write
}

func newActivityWriter(p eventParser) *activityWriter {
	w := &activityWriter{parser: p}
	w.last.Store(time.Now().UnixNano())
	return w
}

func (w *activityWriter) Write(p []byte) (int, error) {
	w.last.Store(time.Now().UnixNano())
	w.mu.Lock()
	defer w.mu.Unlock()
	if room := maxRawKept - len(w.raw); room > 0 {
		w.raw = append(w.raw, p[:min(room, len(p))]...)
	}
	if len(w.raw) >= maxRawKept {
		w.rawCapped = true
	}
	for data := p; len(data) > 0; {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			w.addPending(data)
			break
		}
		w.addPending(data[:i])
		w.endLine()
		data = data[i+1:]
	}
	return len(p), nil
}

func (w *activityWriter) addPending(b []byte) {
	if w.skipping {
		return
	}
	if len(w.pending)+len(b) > maxLineKept {
		w.pending, w.skipping = nil, true
		return
	}
	w.pending = append(w.pending, b...)
}

func (w *activityWriter) endLine() {
	if !w.skipping {
		if line := bytes.TrimSpace(w.pending); len(line) > 0 {
			w.parser.feed(line)
		}
	}
	w.skipping = false
	if cap(w.pending) > 1<<20 {
		w.pending = nil
	} else {
		w.pending = w.pending[:0]
	}
}

// finish parses a final line that had no trailing newline. Call it once the worker has exited.
func (w *activityWriter) finish() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.pending) > 0 {
		w.endLine()
	}
}

func (w *activityWriter) summary() streamSummary {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.parser.summary()
}

// String is the raw stdout kept for diagnostics, capped at maxRawKept.
func (w *activityWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.rawCapped {
		return string(w.raw) + "\n[output truncated]"
	}
	return string(w.raw)
}
