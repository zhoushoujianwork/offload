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

// ReadStats streams path once. now and its location fix which records Aggregate would accept for
// the recent list (a readable timestamp that is not in the future). A missing log is empty.
func ReadStats(path string, now time.Time) (StatsLog, error) {
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
		at  time.Time
		idx int
	}
	var kept []keptRun // newest first, at most RecentLimit; the only records that retain Result
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var r Record
		if json.Unmarshal(line, &r) != nil {
			out.Corrupt++
			continue
		}
		normalizeCost(&r)
		idx := len(out.Records)
		at, ok := ParseTS(r.TS, now.Location())
		if !ok || at.After(now) {
			r.Result = ""
			out.Records = append(out.Records, r)
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
			r.Result = ""
			out.Records = append(out.Records, r)
			continue
		}
		out.Records = append(out.Records, r)
		kept = append(kept, keptRun{})
		copy(kept[pos+1:], kept[pos:])
		kept[pos] = keptRun{at, idx}
		if len(kept) > RecentLimit {
			out.Records[kept[RecentLimit].idx].Result = ""
			kept = kept[:RecentLimit]
		}
	}
	return out, sc.Err()
}
