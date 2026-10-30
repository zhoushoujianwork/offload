package schedule

import (
	"testing"
	"time"
)

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func nextOf(t *testing.T, expr, tz, after string) string {
	t.Helper()
	sp, err := NewSpec(KindCron, expr, tz, time.Now())
	if err != nil {
		t.Fatalf("%q: %v", expr, err)
	}
	n, ok, err := sp.Next(mustTime(t, after))
	if err != nil || !ok {
		t.Fatalf("%q next: ok=%v err=%v", expr, ok, err)
	}
	return n.UTC().Format(time.RFC3339)
}

func TestCronNextInExplicitTimezone(t *testing.T) {
	tests := []struct{ name, expr, tz, after, want string }{
		{"shanghai daily is utc+8", "30 9 * * *", "Asia/Shanghai", "2026-11-01T00:00:00Z", "2026-11-01T01:30:00Z"},
		{"same expr in utc", "30 9 * * *", "UTC", "2026-11-01T00:00:00Z", "2026-11-01T09:30:00Z"},
		{"strictly after: equal instant moves on", "30 9 * * *", "UTC", "2026-11-01T09:30:00Z", "2026-11-02T09:30:00Z"},
		{"weekdays skip the weekend", "0 8 * * 1-5", "UTC", "2026-10-30T09:00:00Z", "2026-11-02T08:00:00Z"}, // Fri -> Mon
		{"step", "*/15 * * * *", "UTC", "2026-11-01T10:07:00Z", "2026-11-01T10:15:00Z"},
		{"month names and dow names", "0 0 1 jan mon", "UTC", "2026-11-01T00:00:00Z", "2027-01-01T00:00:00Z"}, // either dom=1 or Monday; 1 Jan 2027 is Friday, dom matches
		{"both day fields restricted: either matches", "0 0 15 * fri", "UTC", "2026-11-01T00:00:00Z", "2026-11-06T00:00:00Z"},
		{"sunday as 7", "0 0 * * 7", "UTC", "2026-11-01T12:00:00Z", "2026-11-08T00:00:00Z"},
		{"feb 29 waits for leap year", "0 0 29 2 *", "UTC", "2026-11-01T00:00:00Z", "2028-02-29T00:00:00Z"},
		{"negative offset zone crosses the date", "0 22 * * *", "America/Los_Angeles", "2026-11-01T00:00:00Z", "2026-11-01T05:00:00Z"}, // 22:00 PDT on Oct 31
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := nextOf(t, tc.expr, tc.tz, tc.after); got != tc.want {
				t.Errorf("got %s want %s", got, tc.want)
			}
		})
	}
}

// 2026-03-08 02:30 does not exist in New York; 2026-11-01 01:30 happens twice.
func TestCronDaylightSavingNeverSkipsOrDuplicates(t *testing.T) {
	// Spring forward: the 02:30 wall time is skipped that day, the next run is the following day.
	if got := nextOf(t, "30 2 * * *", "America/New_York", "2026-03-08T00:00:00Z"); got != "2026-03-09T06:30:00Z" {
		t.Errorf("spring-forward gap: got %s", got)
	}
	// Fall back: 01:30 occurs at 05:30Z (EDT) and 06:30Z (EST). It fires once, at the first.
	first := nextOf(t, "30 1 * * *", "America/New_York", "2026-11-01T00:00:00Z")
	if first != "2026-11-01T05:30:00Z" {
		t.Fatalf("fall-back first occurrence: got %s", first)
	}
	if got := nextOf(t, "30 1 * * *", "America/New_York", first); got != "2026-11-02T06:30:00Z" {
		t.Errorf("fall-back duplicate: got %s", got)
	}
	// An ordinary time keeps its wall clock across the change.
	if got := nextOf(t, "0 9 * * *", "America/New_York", "2026-11-01T14:30:00Z"); got != "2026-11-02T14:00:00Z" {
		t.Errorf("wall clock after fall-back: got %s", got)
	}
}

func TestCronRejectsInvalid(t *testing.T) {
	for _, expr := range []string{"", "* * * *", "* * * * * *", "60 * * * *", "* 24 * * *", "* * 0 * *", "* * 32 * *", "* * * 13 *", "* * * * 8",
		"5-1 * * * *", "*/0 * * * *", "a * * * *", "1,,2 * * * *", "* * * foo *"} {
		if _, err := NewSpec(KindCron, expr, "UTC", time.Now()); err == nil {
			t.Errorf("%q should be rejected", expr)
		}
	}
}

func TestTimezoneMustBeExplicitIANA(t *testing.T) {
	for _, tz := range []string{"", "Local", "Mars/Olympus", "EST5EDTX"} {
		if _, err := NewSpec(KindCron, "* * * * *", tz, time.Now()); err == nil {
			t.Errorf("timezone %q should be rejected", tz)
		}
	}
}

func TestEveryAndAt(t *testing.T) {
	now := mustTime(t, "2026-11-01T10:00:00Z")
	e, err := NewSpec(KindEvery, "90m", "UTC", now)
	if err != nil {
		t.Fatal(err)
	}
	n, _, _ := e.Next(now)
	if n != now.Add(90*time.Minute) {
		t.Errorf("first interval: %s", n)
	}
	// Occurrences stay on the anchor grid however late the query is.
	n, _, _ = e.Next(now.Add(5*time.Hour + 7*time.Minute))
	if n != now.Add(6*time.Hour) {
		t.Errorf("grid: %s", n)
	}
	if _, err := NewSpec(KindEvery, "30s", "UTC", now); err == nil {
		t.Error("sub-minute interval accepted")
	}
	if _, err := NewSpec(KindEvery, "soon", "UTC", now); err == nil {
		t.Error("bad interval accepted")
	}

	at, err := NewSpec(KindAt, "2026-11-01T19:00", "Asia/Shanghai", now)
	if err != nil {
		t.Fatal(err)
	}
	if n, ok, _ := at.Next(now); !ok || n.Format(time.RFC3339) != "2026-11-01T11:00:00Z" {
		t.Errorf("at in zone: %v %v", n, ok)
	}
	if _, ok, _ := at.Next(mustTime(t, "2026-11-01T11:00:00Z")); ok {
		t.Error("at has an occurrence after it fired")
	}
	off, err := NewSpec(KindAt, "2026-11-01T18:00:00+08:00", "America/New_York", now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n, _, _ := off.Next(now.Add(-time.Hour)); n.UTC().Format(time.RFC3339) != "2026-11-01T10:00:00Z" {
		t.Errorf("explicit offset must win over --tz: %s", n)
	}
	if _, err := NewSpec(KindAt, "2026-11-01T09:00:00Z", "UTC", now); err == nil {
		t.Error("past time accepted")
	}
	if _, err := NewSpec(KindAt, "tomorrow", "UTC", now); err == nil {
		t.Error("garbage time accepted")
	}
}
