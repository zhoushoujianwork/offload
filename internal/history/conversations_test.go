package history

import (
	"strings"
	"testing"
	"time"
)

func convRec(ts, session, task, result string) Record {
	return Record{TS: ts, Status: "ok", Session: session, Task: task, Result: result, Dir: "/p", Profile: "pr", Model: "m", Backend: "codex"}
}

func TestConversationsGroupOrderAndFullResult(t *testing.T) {
	long := strings.Repeat("x", RecentResultRunes*3)
	rs := []Record{
		convRec("2026-10-29 10:00:00", "A", "first\nsecond line", "r1"),
		convRec("2026-10-29 11:00:00", "B", "other", "rb"),
		convRec("2026-10-29 12:00:00", "A", "follow up", long),
		convRec("2026-10-29 09:00:00", "A", "logged late", "r0"), // earlier TS appended later
	}
	cs := BuildConversations(rs, nil, time.UTC)
	if len(cs) != 2 || cs[0].Summary.ID != "A" || cs[1].Summary.ID != "B" {
		t.Fatalf("conversations: %+v", cs)
	}
	a := cs[0]
	var tasks []string
	for _, r := range a.Runs {
		tasks = append(tasks, r.Task)
	}
	if strings.Join(tasks, "|") != "logged late|first\nsecond line|follow up" {
		t.Errorf("order: %q", tasks)
	}
	if a.Runs[2].Result != long {
		t.Errorf("result truncated: %d runes", len([]rune(a.Runs[2].Result)))
	}
	s := a.Summary
	if s.Title != "logged late" || s.RunCount != 3 || s.StartedAt != "2026-10-29 09:00:00" || s.UpdatedAt != "2026-10-29 12:00:00" || s.Session != "A" {
		t.Errorf("summary: %+v", s)
	}
}

func TestConversationTitleBounded(t *testing.T) {
	got := title("\n  " + strings.Repeat("é", 300) + "\nnext")
	if n := len([]rune(got)); n != TitleRunes {
		t.Errorf("title runes = %d", n)
	}
	if title("") != "" {
		t.Error("empty task title")
	}
}

func TestConversationSummaryUsesLatestMetadata(t *testing.T) {
	a := convRec("2026-10-29 10:00:00", "A", "t", "r")
	b := convRec("2026-10-29 11:00:00", "A", "t2", "r")
	b.Profile, b.Model, b.Dir, b.Backend = "new", "m2", "/q", "claude"
	c := convRec("2026-10-29 12:00:00", "A", "t3", "r")
	c.Profile, c.Model, c.Dir, c.Backend = "", "", "", ""
	c.Status = "timeout"
	s := BuildConversations([]Record{a, b, c}, nil, time.UTC)[0].Summary
	if s.Profile != "new" || s.Model != "m2" || s.Dir != "/q" || s.Backend != "claude" || s.Status != "timeout" {
		t.Errorf("summary: %+v", s)
	}
}

func TestOrphanIDsUniqueStableAndVisible(t *testing.T) {
	fail := Record{TS: "2026-10-29 10:00:00", Status: "error", Task: "t", Error: "boom", Result: "boom"}
	rs := []Record{fail, fail, convRec("2026-10-29 10:00:00", "S", "t", "r")}
	cs := BuildConversations(rs, nil, time.UTC)
	if len(cs) != 3 {
		t.Fatalf("conversations: %d", len(cs))
	}
	seen := map[string]bool{}
	for _, c := range cs {
		if seen[c.Summary.ID] {
			t.Fatalf("duplicate id %s", c.Summary.ID)
		}
		seen[c.Summary.ID] = true
		if c.Summary.ID != "S" && (!strings.HasPrefix(c.Summary.ID, "run:") || c.Summary.Session != "" || c.Summary.Active) {
			t.Errorf("orphan summary: %+v", c.Summary)
		}
	}
	again := BuildConversations(append(append([]Record{}, rs...), convRec("2026-10-29 11:00:00", "S", "t", "r")), nil, time.UTC)
	for id := range seen {
		if _, ok := FindConversation(rs, id, nil, time.UTC); !ok {
			t.Errorf("id %s not found", id)
		}
	}
	for id := range seen {
		found := false
		for _, c := range again {
			found = found || c.Summary.ID == id
		}
		if !found {
			t.Errorf("id %s not stable after append", id)
		}
	}
	if c, _ := FindConversation(rs, "S", nil, time.UTC); c.Runs[0].ID == "" {
		t.Error("run id empty")
	}
	if _, ok := FindConversation(rs, "", nil, time.UTC); ok {
		t.Error("empty id found")
	}
	if _, ok := FindConversation(rs, "run:nope", nil, time.UTC); ok {
		t.Error("unknown id found")
	}
}

func TestInvalidTimestampsKeepRecords(t *testing.T) {
	rs := []Record{
		convRec("2026-10-29 10:00:00", "A", "one", ""),
		convRec("garbage", "A", "two", ""),
		convRec("2026-10-29 11:00:00", "A", "three", ""),
		convRec("", "Z", "only bad", ""),
	}
	cs := BuildConversations(rs, nil, time.UTC)
	if len(cs) != 2 {
		t.Fatalf("conversations: %d", len(cs))
	}
	var a Conversation
	for _, c := range cs {
		if c.Summary.ID == "A" {
			a = c
		}
	}
	if len(a.Runs) != 3 || a.Runs[0].Task != "one" || a.Runs[1].Task != "two" || a.Runs[2].Task != "three" {
		t.Errorf("runs: %+v", a.Runs)
	}
}

func TestConversationCostCoverageAndExclusions(t *testing.T) {
	one, two := 1.0, 2.0
	use := &TokenUsage{Input: 10, Output: 5}
	cum := 9.0
	codex := convRec("2026-10-29 10:00:00", "A", "t", "r")
	codex.CostUSD, codex.Usage, codex.UsageScope = &one, use, "run"
	claudeNew := convRec("2026-10-29 10:01:00", "A", "t", "r")
	claudeNew.Backend, claudeNew.CostUSD, claudeNew.CumulativeCostUSD = "claude", &two, &cum
	legacy := convRec("2026-10-29 10:02:00", "A", "t", "r")
	legacy.Backend, legacy.CostUSD = "claude", &two
	salv := convRec("2026-10-29 10:03:00", "A", "t", "r")
	salv.CostUSD, salv.Usage, salv.Salvaged = &two, use, true
	cumulativeOnly := convRec("2026-10-29 10:04:00", "A", "t", "r")
	cumulativeOnly.CumulativeUsage = &TokenUsage{Input: 999}
	scoped := convRec("2026-10-29 10:05:00", "A", "t", "r")
	scoped.Usage, scoped.UsageScope = use, "session"

	c := BuildConversations([]Record{codex, claudeNew, legacy, salv, cumulativeOnly, scoped}, nil, time.UTC)[0]
	s := c.Summary
	if s.RunCount != 6 || s.CostRuns != 2 || s.CostUSD == nil || *s.CostUSD != 3 {
		t.Errorf("cost: runs=%d costRuns=%d cost=%v", s.RunCount, s.CostRuns, s.CostUSD)
	}
	if s.UsageRuns != 1 || s.Usage == nil || s.Usage.Input != 10 || s.Usage.Output != 5 {
		t.Errorf("usage: %d %+v", s.UsageRuns, s.Usage)
	}
	for i, wantCost := range []bool{true, true, false, false, false, false} {
		if (c.Runs[i].CostUSD != nil) != wantCost {
			t.Errorf("run %d cost=%v", i, c.Runs[i].CostUSD)
		}
	}
	if c.Runs[3].Usage != nil || c.Runs[3].CostUSD != nil || !c.Runs[3].Salvaged {
		t.Errorf("salvaged run: %+v", c.Runs[3])
	}
	if c.Runs[4].Usage != nil || c.Runs[5].Usage != nil {
		t.Error("cumulative or non-run-scoped usage accepted")
	}

	empty := BuildConversations([]Record{convRec("2026-10-29 10:00:00", "E", "t", "r")}, nil, time.UTC)[0].Summary
	if empty.Usage != nil || empty.CostUSD != nil || empty.UsageRuns != 0 || empty.CostRuns != 0 {
		t.Errorf("missing accounting must stay null: %+v", empty)
	}
}

func TestConversationActiveAndEmpty(t *testing.T) {
	if cs := BuildConversations(nil, nil, nil); cs == nil || len(cs) != 0 {
		t.Errorf("empty = %#v", cs)
	}
	rs := []Record{convRec("2026-10-29 10:00:00", "A", "t", "r"), convRec("2026-10-29 10:00:00", "B", "t", "r")}
	cs := BuildConversations(rs, map[string]bool{"A": true}, time.UTC)
	for _, c := range cs {
		if c.Summary.Active != (c.Summary.ID == "A") {
			t.Errorf("%s active=%v", c.Summary.ID, c.Summary.Active)
		}
	}
	// Equal timestamps: the later record sorts first.
	if cs[0].Summary.ID != "B" {
		t.Errorf("tie order: %s first", cs[0].Summary.ID)
	}
}

func TestConversationMatchesAnyRun(t *testing.T) {
	a := convRec("2026-10-29 10:00:00", "Sess-1", "alpha task", "first")
	b := convRec("2026-10-29 11:00:00", "Sess-1", "beta", "NeedleResult")
	c := BuildConversations([]Record{a, b}, nil, time.UTC)[0]
	for _, q := range []string{"", "alpha", "needleresult", "sess-1", "/p", "pr", "m"} {
		if !c.Matches(q) {
			t.Errorf("query %q should match", q)
		}
	}
	if c.Matches("absent") {
		t.Error("absent matched")
	}
}

func TestConversationExposesAddDirsAdditively(t *testing.T) {
	old := convRec("2026-10-29 10:00:00", "A", "t", "r")
	first := convRec("2026-10-29 11:00:00", "A", "t2", "r")
	first.AddDirs = []string{"/x", "/y"}
	cleared := convRec("2026-10-29 12:00:00", "A", "t3", "r")
	c := BuildConversations([]Record{old, first, cleared}, nil, time.UTC)[0]
	if len(c.Summary.AddDirs) != 0 {
		t.Errorf("summary shows the latest run's set, got %v", c.Summary.AddDirs)
	}
	if len(c.Runs[0].AddDirs) != 0 || len(c.Runs[1].AddDirs) != 2 || c.Runs[1].AddDirs[0] != "/x" {
		t.Errorf("runs: %+v", c.Runs)
	}
	first.AddDirs[0] = "mutated"
	if c.Runs[1].AddDirs[0] != "/x" {
		t.Error("DTO aliases the record")
	}
}
