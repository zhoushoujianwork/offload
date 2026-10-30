package history

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"time"
)

// StatsLog is the run log as `stats` needs it: every record, but with report bodies (Result)
// kept only for the newest RecentLimit records that Aggregate can list. Metadata metrics then do
// not hold every historical report in memory. Corrupt counts non-empty lines that are not valid
// records; a damaged line may have been a call on any day, so it limits what the log can vouch for.
type StatsLog struct {
	Records []Record
	Corrupt int
}

// skipBody accepts a JSON value without copying it, so decoding a record's "result" allocates
// nothing for the report text.
type skipBody struct{}

func (*skipBody) UnmarshalJSON([]byte) error { return nil }

// metaRecord decodes every Record field except the report body: the shallower Result shadows
// Record.Result.
type metaRecord struct {
	Record
	Result skipBody `json:"result"`
}

func newLineScanner(f *os.File) *bufio.Scanner {
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	return sc
}

// ReadStats streams path and keeps report bodies (Result) only for the newest RecentLimit
// records that Aggregate can list. The first pass decodes metadata without materializing any
// report text and remembers which lines are listed; a second pass decodes only those lines in
// full. now and its location fix which records Aggregate would accept for the recent list (a
// readable timestamp that is not in the future). A missing log is empty.
func ReadStats(path string, now time.Time, caller ...string) (StatsLog, error) {
	var out StatsLog
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	defer f.Close()
	type keptRun struct {
		at   time.Time
		idx  int // index in out.Records
		line int // 1-based line number in the file
	}
	var kept []keptRun // newest first, at most RecentLimit
	sc := newLineScanner(f)
	for line := 1; sc.Scan(); line++ {
		raw := sc.Bytes()
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		var m metaRecord
		if json.Unmarshal(raw, &m) != nil {
			out.Corrupt++
			continue
		}
		r := m.Record
		normalizeCost(&r)
		idx := len(out.Records)
		out.Records = append(out.Records, r)
		if len(caller) > 0 && caller[0] != "" && CallerOrUnspecified(r.Caller) != caller[0] {
			continue
		}
		at, ok := ParseTS(r.TS, now.Location())
		if !ok || at.After(now) {
			continue
		}
		// Same order as Aggregate: newest timestamp first, later line first on a tie.
		pos := len(kept)
		for i, k := range kept {
			if !at.Before(k.at) {
				pos = i
				break
			}
		}
		if pos >= RecentLimit {
			continue
		}
		kept = append(kept, keptRun{})
		copy(kept[pos+1:], kept[pos:])
		kept[pos] = keptRun{at, idx, line}
		if len(kept) > RecentLimit {
			kept = kept[:RecentLimit]
		}
	}
	if err := sc.Err(); err != nil {
		return out, err
	}
	if len(kept) == 0 {
		return out, nil
	}
	byLine := make(map[int]int, len(kept))
	for _, k := range kept {
		byLine[k.line] = k.idx
	}
	if _, err := f.Seek(0, 0); err != nil {
		return out, err
	}
	sc = newLineScanner(f)
	for line := 1; sc.Scan() && len(byLine) > 0; line++ {
		idx, ok := byLine[line]
		if !ok {
			continue
		}
		delete(byLine, line)
		var full Record
		// Only attach the body if the line is still the same record (the log is append-only,
		// but never trust a file that changed between the passes).
		if json.Unmarshal(sc.Bytes(), &full) == nil && full.RunID == out.Records[idx].RunID && full.TS == out.Records[idx].TS {
			out.Records[idx].Result = full.Result
		}
	}
	return out, sc.Err()
}
