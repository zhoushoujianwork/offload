package history

import "time"

// DayCalls is the number of recorded calls that started on one local calendar day. Covered is
// false when the run log cannot tell (the day precedes the first recorded call), in which case
// Calls is meaningless.
type DayCalls struct {
	Start   time.Time
	Calls   int
	Covered bool
}

// CallWindow counts recorded calls (one per run record, as Totals.Runs) by the local calendar
// day of the invocation start timestamp: the Days completed days before today, oldest first,
// and today so far.
type CallWindow struct {
	Days  []DayCalls
	Today DayCalls
	// First is the start of the calendar day of the earliest readable record, or zero for an
	// empty log. Earlier days are unknown, not zero.
	First time.Time
	// Skipped counts records with an unreadable or future timestamp; they belong to no day, so
	// any window may be missing calls.
	Skipped int
}

// CountDailyCalls splits records into calendar days in now's location in one pass.
func CountDailyCalls(rs []Record, now time.Time, days int) CallWindow {
	y, m, d := now.Date()
	bounds := make([]time.Time, days+2) // days completed days, today, and the end of today
	for i := range bounds {
		bounds[i] = time.Date(y, m, d-days+i, 0, 0, 0, 0, now.Location())
	}
	counts := make([]int, days+1)
	w := CallWindow{}
	var first time.Time
	for _, r := range rs {
		at, ok := ParseTS(r.TS, now.Location())
		if !ok || at.After(now) {
			w.Skipped++
			continue
		}
		if first.IsZero() || at.Before(first) {
			first = at
		}
		if at.Before(bounds[0]) {
			continue
		}
		for i := days; i >= 0; i-- {
			if !at.Before(bounds[i]) {
				counts[i]++
				break
			}
		}
	}
	if !first.IsZero() {
		fy, fm, fd := first.Date()
		w.First = time.Date(fy, fm, fd, 0, 0, 0, 0, now.Location())
	}
	for i := 0; i <= days; i++ {
		dc := DayCalls{Start: bounds[i], Calls: counts[i], Covered: !w.First.IsZero() && !bounds[i].Before(w.First)}
		if i == days {
			w.Today = dc
			// Today is always reported as recorded calls; with an empty log that is a true zero.
			w.Today.Covered = true
		} else {
			w.Days = append(w.Days, dc)
		}
	}
	return w
}

// UncoveredBy counts admitted runs that started in [ds, de) but have no processing events, such
// as runs recorded by a version that did not write them. Runs that were never admitted (failed
// setup, cancelled while queued) never processed anything and are not counted.
func UncoveredBy(rs []Record, ds, de time.Time, log ProcessingLog) int {
	n := 0
	for _, r := range rs {
		if r.ExecMs == nil {
			continue
		}
		at, ok := ParseTS(r.TS, ds.Location())
		if !ok || at.Before(ds) || !at.Before(de) {
			continue
		}
		if r.RunID == "" || !log.Seen[r.RunID] {
			n++
		}
	}
	return n
}
