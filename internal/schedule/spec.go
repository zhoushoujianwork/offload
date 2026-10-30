// Package schedule stores scheduled offload tasks and runs them when they are due.
//
// A scheduled occurrence launches an ordinary `offload -json ...` child process, so worker slots,
// the global disable switch, execution policy and profile credentials behave exactly as for a
// manual run. This package never reads or stores a token.
package schedule

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "time/tzdata" // zone names must resolve on hosts without a system zoneinfo database
)

// Kind is the schedule syntax a task uses.
type Kind string

const (
	KindCron  Kind = "cron"  // five-field cron expression evaluated in the task's timezone
	KindEvery Kind = "every" // fixed interval measured from the moment the schedule was set
	KindAt    Kind = "at"    // one occurrence
)

// MinEvery is the shortest accepted interval.
const MinEvery = time.Minute

// Spec is a validated schedule.
type Spec struct {
	Kind     Kind   `json:"kind"`
	Expr     string `json:"expr"`     // cron expression, interval, or the time as entered
	Timezone string `json:"timezone"` // IANA name; used for cron fields, "at" parsing and display
	// Anchor is the start of an "every" interval, or the instant of an "at" schedule.
	Anchor *time.Time `json:"anchor,omitempty"`
}

// DetectTimezone returns the IANA name of the host's local zone from $TZ or /etc/localtime.
func DetectTimezone() (string, error) {
	if tz := os.Getenv("TZ"); tz != "" && tz != "Local" {
		tz = strings.TrimPrefix(tz, ":")
		if _, err := time.LoadLocation(tz); err == nil {
			return tz, nil
		}
	}
	if target, err := os.Readlink("/etc/localtime"); err == nil {
		target = filepath.ToSlash(target)
		if i := strings.LastIndex(target, "zoneinfo/"); i >= 0 {
			name := target[i+len("zoneinfo/"):]
			if _, err := time.LoadLocation(name); err == nil {
				return name, nil
			}
		}
	}
	return "", errors.New("cannot detect the local timezone; pass --tz with an IANA name such as UTC or Asia/Shanghai")
}

// LoadZone resolves an IANA timezone name. The ambiguous names "" and "Local" are refused so a
// stored schedule means the same thing on every host.
func LoadZone(name string) (*time.Location, error) {
	if name == "" || name == "Local" {
		return nil, errors.New("timezone must be an IANA name such as UTC or Asia/Shanghai")
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("unknown timezone %q (use an IANA name such as UTC or Asia/Shanghai)", name)
	}
	return loc, nil
}

var atLayouts = []string{"2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02 15:04:05", "2006-01-02 15:04"}

// NewSpec validates a schedule. now anchors an "every" interval and is the lower bound of an "at"
// time, which must lie in the future.
func NewSpec(kind Kind, expr, tz string, now time.Time) (Spec, error) {
	expr = strings.TrimSpace(expr)
	loc, err := LoadZone(tz)
	if err != nil {
		return Spec{}, err
	}
	s := Spec{Kind: kind, Expr: expr, Timezone: tz}
	switch kind {
	case KindCron:
		if _, err := parseCron(expr); err != nil {
			return Spec{}, err
		}
	case KindEvery:
		d, err := time.ParseDuration(expr)
		if err != nil {
			return Spec{}, fmt.Errorf("invalid interval %q (want a duration such as 90m or 6h)", expr)
		}
		if d < MinEvery {
			return Spec{}, fmt.Errorf("interval must be at least %s", MinEvery)
		}
		s.Expr = d.String()
		a := now.UTC().Truncate(time.Second)
		s.Anchor = &a
	case KindAt:
		at, err := parseAt(expr, loc)
		if err != nil {
			return Spec{}, err
		}
		if !at.After(now) {
			return Spec{}, fmt.Errorf("time %s is not in the future", at.In(loc).Format(time.RFC3339))
		}
		s.Anchor = &at
	default:
		return Spec{}, fmt.Errorf("unknown schedule kind %q", kind)
	}
	return s, nil
}

func parseAt(expr string, loc *time.Location) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, expr); err == nil {
		return t.UTC(), nil // an explicit offset wins over --tz
	}
	for _, layout := range atLayouts {
		if t, err := time.ParseInLocation(layout, expr, loc); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid time %q (want RFC3339 such as 2026-11-01T09:00:00+08:00, or 2026-11-01T09:00 in the task timezone)", expr)
}

// Describe is a one-line human description.
func (s Spec) Describe() string {
	switch s.Kind {
	case KindCron:
		return fmt.Sprintf("cron %q (%s)", s.Expr, s.Timezone)
	case KindEvery:
		return fmt.Sprintf("every %s", s.Expr)
	case KindAt:
		if s.Anchor != nil {
			if loc, err := LoadZone(s.Timezone); err == nil {
				return "once at " + s.Anchor.In(loc).Format("2006-01-02 15:04:05 MST")
			}
		}
		return "once at " + s.Expr
	}
	return string(s.Kind)
}

// Next returns the first occurrence strictly after the given instant. ok is false when none
// remains (a finished "at" schedule).
func (s Spec) Next(after time.Time) (next time.Time, ok bool, err error) {
	switch s.Kind {
	case KindAt:
		if s.Anchor != nil && s.Anchor.After(after) {
			return *s.Anchor, true, nil
		}
		return time.Time{}, false, nil
	case KindEvery:
		d, err := time.ParseDuration(s.Expr)
		if err != nil || d <= 0 || s.Anchor == nil {
			return time.Time{}, false, fmt.Errorf("invalid interval schedule %q", s.Expr)
		}
		if after.Before(*s.Anchor) {
			return s.Anchor.Add(d), true, nil
		}
		n := after.Sub(*s.Anchor)/d + 1
		return s.Anchor.Add(n * d), true, nil
	case KindCron:
		loc, err := LoadZone(s.Timezone)
		if err != nil {
			return time.Time{}, false, err
		}
		c, err := parseCron(s.Expr)
		if err != nil {
			return time.Time{}, false, err
		}
		t, ok := c.next(after, loc)
		return t, ok, nil
	}
	return time.Time{}, false, fmt.Errorf("unknown schedule kind %q", s.Kind)
}

// cronExpr is a parsed five-field expression: minute hour day-of-month month day-of-week.
type cronExpr struct {
	minutes, hours []int
	dom            [32]bool
	months         [13]bool
	dow            [7]bool
	domStar        bool
	dowStar        bool
}

var monthNames = map[string]int{"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6, "jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12}
var dowNames = map[string]int{"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6}

func parseCron(expr string) (*cronExpr, error) {
	f := strings.Fields(expr)
	if len(f) != 5 {
		return nil, fmt.Errorf("cron expression %q needs five fields: minute hour day-of-month month day-of-week", expr)
	}
	c := &cronExpr{}
	var err error
	var set []int
	if c.minutes, _, err = parseField(f[0], 0, 59, nil); err != nil {
		return nil, fmt.Errorf("minute: %w", err)
	}
	if c.hours, _, err = parseField(f[1], 0, 23, nil); err != nil {
		return nil, fmt.Errorf("hour: %w", err)
	}
	if set, c.domStar, err = parseField(f[2], 1, 31, nil); err != nil {
		return nil, fmt.Errorf("day-of-month: %w", err)
	}
	for _, v := range set {
		c.dom[v] = true
	}
	if set, _, err = parseField(f[3], 1, 12, monthNames); err != nil {
		return nil, fmt.Errorf("month: %w", err)
	}
	for _, v := range set {
		c.months[v] = true
	}
	if set, c.dowStar, err = parseField(f[4], 0, 7, dowNames); err != nil {
		return nil, fmt.Errorf("day-of-week: %w", err)
	}
	for _, v := range set {
		c.dow[v%7] = true // 7 is Sunday too
	}
	return c, nil
}

// parseField expands one field into a sorted value list. star is true when the field is a bare
// "*" or "*/n", which decides how day-of-month and day-of-week combine.
func parseField(s string, min, max int, names map[string]int) (vals []int, star bool, err error) {
	seen := map[int]bool{}
	star = strings.HasPrefix(s, "*") // as in classic cron: a field starting with * does not restrict the day
	num := func(v string) (int, error) {
		if n, ok := names[strings.ToLower(v)]; ok {
			return n, nil
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < min || n > max {
			return 0, fmt.Errorf("value %q out of range %d-%d", v, min, max)
		}
		return n, nil
	}
	for _, part := range strings.Split(s, ",") {
		if part == "" {
			return nil, false, errors.New("empty list entry")
		}
		base, step := part, 1
		if i := strings.IndexByte(part, '/'); i >= 0 {
			n, err := strconv.Atoi(part[i+1:])
			if err != nil || n < 1 {
				return nil, false, fmt.Errorf("invalid step in %q", part)
			}
			base, step = part[:i], n
		}
		lo, hi := min, max
		switch {
		case base == "*":
		case strings.Contains(base, "-"):
			ab := strings.SplitN(base, "-", 2)
			if lo, err = num(ab[0]); err != nil {
				return nil, false, err
			}
			if hi, err = num(ab[1]); err != nil {
				return nil, false, err
			}
			if lo > hi {
				return nil, false, fmt.Errorf("range %q is reversed", base)
			}
		default:
			if lo, err = num(base); err != nil {
				return nil, false, err
			}
			if strings.Contains(part, "/") {
				hi = max // "a/n" runs from a to the end of the range
			} else {
				hi = lo
			}
		}
		for v := lo; v <= hi; v += step {
			seen[v] = true
		}
	}
	for v := min; v <= max; v++ {
		if seen[v] {
			vals = append(vals, v)
		}
	}
	return vals, star, nil
}

func (c *cronExpr) dayMatches(y int, m time.Month, d int, wd time.Weekday) bool {
	if !c.months[m] {
		return false
	}
	domOK, dowOK := c.dom[d], c.dow[wd]
	if c.domStar || c.dowStar {
		return domOK && dowOK // a bare * matches every value, so this is the restricted field alone
	}
	return domOK || dowOK // both restricted: either may match, as in classic cron
}

// next walks calendar days in loc. Wall-clock times that do not exist (spring-forward gap) are
// skipped, and a wall-clock time that occurs twice (fall-back) fires once, at its first
// occurrence, so DST never causes a duplicate.
func (c *cronExpr) next(after time.Time, loc *time.Location) (time.Time, bool) {
	y, m, d := after.In(loc).Date()
	for i := 0; i < 366*9; i++ {
		day := time.Date(y, m, d+i, 12, 0, 0, 0, loc)
		yy, mm, dd := day.Date()
		if !c.dayMatches(yy, mm, dd, day.Weekday()) {
			continue
		}
		for _, h := range c.hours {
			for _, mi := range c.minutes {
				if t, ok := resolveWall(loc, yy, mm, dd, h, mi); ok && t.After(after) {
					return t, true
				}
			}
		}
	}
	return time.Time{}, false
}

// resolveWall maps a wall-clock time in loc to the earliest instant showing exactly that time.
func resolveWall(loc *time.Location, y int, m time.Month, d, h, mi int) (time.Time, bool) {
	wall := time.Date(y, m, d, h, mi, 0, 0, time.UTC)
	var best time.Time
	found := false
	for _, probe := range []time.Time{wall.Add(-24 * time.Hour), wall.Add(24 * time.Hour)} {
		_, off := probe.In(loc).Zone()
		c := wall.Add(-time.Duration(off) * time.Second)
		l := c.In(loc)
		if l.Year() == y && l.Month() == m && l.Day() == d && l.Hour() == h && l.Minute() == mi {
			if !found || c.Before(best) {
				best, found = c, true
			}
		}
	}
	return best, found
}
