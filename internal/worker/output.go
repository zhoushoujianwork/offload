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
	// maxCallbackLine bounds the lines handed to onLine; longer ones still feed the parser.
	maxCallbackLine = 256 << 10
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
	lineMu    sync.Mutex // keeps onLine calls in stream order without holding mu during the callback
	parser    eventParser
	onLine    func([]byte) error // optional; sees each complete line of at most maxCallbackLine bytes
	lines     [][]byte           // lines completed by the current Write, delivered after mu is released
	pending   []byte             // bytes of the current, unfinished line
	skipping  bool               // inside a line longer than maxLineKept; discard up to its newline
	raw       []byte
	rawCapped bool
	last      atomic.Int64 // 相对 activityEpoch 的单调时钟纳秒数
}

func newActivityWriter(p eventParser) *activityWriter {
	w := &activityWriter{parser: p}
	w.last.Store(time.Since(activityEpoch).Nanoseconds())
	return w
}

func (w *activityWriter) Write(p []byte) (int, error) {
	w.last.Store(time.Since(activityEpoch).Nanoseconds())
	if w.onLine != nil {
		w.lineMu.Lock()
		defer w.lineMu.Unlock()
	}
	w.mu.Lock()
	w.write(p)
	lines := w.lines
	w.lines = nil
	w.mu.Unlock()
	// A callback error is returned to the writer's caller, which stops the worker's output copy.
	for _, line := range lines {
		if err := w.onLine(line); err != nil {
			return len(p), err
		}
	}
	return len(p), nil
}

func (w *activityWriter) write(p []byte) {
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
		w.endLine(true)
		data = data[i+1:]
	}
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

func (w *activityWriter) endLine(emit bool) {
	if !w.skipping {
		if line := bytes.TrimSpace(w.pending); len(line) > 0 {
			if w.parser != nil {
				w.parser.feed(line)
			}
			if emit && w.onLine != nil && len(line) <= maxCallbackLine {
				w.lines = append(w.lines, bytes.Clone(line))
			}
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
		w.endLine(false) // an unterminated final line is parsed but not streamed to onLine
	}
}

func (w *activityWriter) summary() streamSummary {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.parser == nil {
		return streamSummary{}
	}
	return w.parser.summary()
}

// Bytes returns a copy of the raw stdout kept so far, capped at maxRawKept.
func (w *activityWriter) Bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return bytes.Clone(w.raw)
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
