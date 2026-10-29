package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhoushoujianwork/offload/internal/history"
	"github.com/zhoushoujianwork/offload/internal/live"
)

func historyRun(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := cmdHistory(args, strings.NewReader(stdin), &out, &errb)
	return code, out.String(), errb.String()
}

func hrec(ts, session, task, result string) history.Record {
	return history.Record{TS: ts, Status: "ok", Session: session, Task: task, Result: result, Dir: "/p/x", Profile: "pr", Model: "m", Backend: "codex"}
}

type listDoc struct {
	SchemaVersion int  `json:"schema_version"`
	Total         int  `json:"total"`
	Offset        int  `json:"offset"`
	Limit         int  `json:"limit"`
	HasMore       bool `json:"has_more"`
	Conversations []struct {
		ID       string `json:"id"`
		Session  string `json:"session"`
		Title    string `json:"title"`
		Active   bool   `json:"active"`
		RunCount int    `json:"run_count"`
	} `json:"conversations"`
}

func decodeList(t *testing.T, out string) listDoc {
	t.Helper()
	var d listDoc
	if err := json.Unmarshal([]byte(out), &d); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	return d
}

func TestHistoryListEmptyContract(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	code, out, _ := historyRun(t, "", "list", "--json")
	if code != 0 {
		t.Fatalf("code=%d", code)
	}
	if !strings.Contains(out, `"conversations":[]`) {
		t.Errorf("array must be non-null: %s", out)
	}
	d := decodeList(t, out)
	if d.SchemaVersion != 1 || d.Total != 0 || d.Limit != 50 || d.Offset != 0 || d.HasMore {
		t.Errorf("doc: %+v", d)
	}
}

func TestHistoryPublicDispatch(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	var out, errb bytes.Buffer
	code := Run("test", []string{"history", "list", "--json"}, strings.NewReader(""), &out, &errb)
	if code != 0 || decodeList(t, out.String()).SchemaVersion != 1 {
		t.Fatalf("history dispatch: code=%d stderr=%s stdout=%s", code, errb.String(), out.String())
	}
}

func TestHistoryListPagingAndSearchAllRuns(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	for i := 0; i < 5; i++ {
		appendListRecord(t, hrec("2026-10-29 10:0"+string(rune('0'+i))+":00", "s"+string(rune('0'+i)), "task "+string(rune('0'+i)), "r"))
	}
	// The search term appears only in the second (later) run of s0.
	appendListRecord(t, hrec("2026-10-29 10:09:00", "s0", "follow up", "found the NEEDLE here"))

	_, out, _ := historyRun(t, "", "list", "--json", "--limit", "2")
	d := decodeList(t, out)
	if d.Total != 5 || len(d.Conversations) != 2 || !d.HasMore || d.Limit != 2 || d.Conversations[0].ID != "s0" {
		t.Errorf("page 1: %+v", d)
	}
	_, out, _ = historyRun(t, "", "list", "--json", "--limit", "2", "--offset", "4")
	d = decodeList(t, out)
	if len(d.Conversations) != 1 || d.HasMore || d.Offset != 4 {
		t.Errorf("last page: %+v", d)
	}
	_, out, _ = historyRun(t, "", "list", "--json", "--offset", "99")
	if d = decodeList(t, out); len(d.Conversations) != 0 || d.Total != 5 || !strings.Contains(out, `"conversations":[]`) {
		t.Errorf("past end: %s", out)
	}

	_, out, _ = historyRun(t, "  needle \n", "list", "--json", "--search-stdin")
	d = decodeList(t, out)
	if d.Total != 1 || d.Conversations[0].ID != "s0" || d.Conversations[0].RunCount != 2 {
		t.Errorf("search: %+v", d)
	}
	// Without the flag stdin is ignored.
	_, out, _ = historyRun(t, "needle", "list", "--json")
	if d = decodeList(t, out); d.Total != 5 {
		t.Errorf("stdin without flag filtered: %+v", d)
	}
	// A blank query matches everything.
	_, out, _ = historyRun(t, " \n", "list", "--json", "--search-stdin")
	if d = decodeList(t, out); d.Total != 5 {
		t.Errorf("blank query: %+v", d)
	}
	_, out, _ = historyRun(t, "no such text", "list", "--json", "--search-stdin")
	if d = decodeList(t, out); d.Total != 0 {
		t.Errorf("no match: %+v", d)
	}
}

func TestHistoryListNoFullReports(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	appendListRecord(t, hrec("2026-10-29 10:00:00", "s", "task", "SECRETRESULT"))
	_, out, _ := historyRun(t, "", "list", "--json")
	if strings.Contains(out, "SECRETRESULT") {
		t.Errorf("list contains a report: %s", out)
	}
}

func TestHistoryShowFullAndActiveExternalSession(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	long := strings.Repeat("y", history.RecentResultRunes*2)
	appendListRecord(t, hrec("2026-10-29 10:00:00", "S", "first", "r1"))
	appendListRecord(t, hrec("2026-10-29 11:00:00", "S", "second", long))
	state := live.State{PID: os.Getpid() + 1000, Phase: live.Running, Session: "S", Task: "external", StartedAt: time.Now(), LastEventAt: time.Now()}
	if err := live.Publish(state); err != nil {
		t.Fatal(err)
	}
	if err := live.Publish(live.State{PID: os.Getpid() + 1001, Phase: live.Done, Session: "T", StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	code, out, errs := historyRun(t, "", "show", "--json", "--id", "S")
	if code != 0 {
		t.Fatalf("code=%d err=%s", code, errs)
	}
	var d struct {
		SchemaVersion int `json:"schema_version"`
		Conversation  struct {
			ID     string `json:"id"`
			Active bool   `json:"active"`
		} `json:"conversation"`
		Runs []struct {
			ID       string `json:"id"`
			Result   string `json:"result"`
			ExitCode *int   `json:"exit_code"`
		} `json:"runs"`
		Active *live.State `json:"active"`
	}
	if err := json.Unmarshal([]byte(out), &d); err != nil {
		t.Fatal(err)
	}
	if d.SchemaVersion != 1 || d.Conversation.ID != "S" || !d.Conversation.Active || len(d.Runs) != 2 ||
		d.Runs[1].Result != long || d.Runs[0].ID == d.Runs[1].ID || d.Active == nil || d.Active.PID != state.PID {
		t.Errorf("show: %+v", d)
	}
	if !strings.Contains(out, `"exit_code":null`) {
		t.Error("nullable exit_code missing")
	}

	_, out, _ = historyRun(t, "", "list", "--json")
	if l := decodeList(t, out); len(l.Conversations) != 1 || !l.Conversations[0].Active {
		t.Errorf("list active: %s", out)
	}

	// A lost worker is not busy.
	live.StaleAfter = time.Nanosecond
	t.Cleanup(func() { live.StaleAfter = 30 * time.Second })
	time.Sleep(5 * time.Millisecond)
	_, out, _ = historyRun(t, "", "show", "--json", "--id", "S")
	if strings.Contains(out, `"active":true`) || !strings.Contains(out, `"active":null`) {
		t.Errorf("lost worker treated as active: %s", out)
	}
}

func TestHistoryShowOrphanAndUnknown(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	appendListRecord(t, history.Record{TS: "2026-10-29 10:00:00", Status: "error", Task: "t", Error: "config broken", Result: "config broken"})
	_, out, _ := historyRun(t, "", "list", "--json")
	d := decodeList(t, out)
	if len(d.Conversations) != 1 || !strings.HasPrefix(d.Conversations[0].ID, "run:") || d.Conversations[0].Session != "" {
		t.Fatalf("orphan: %s", out)
	}
	code, out, _ := historyRun(t, "", "show", "--json", "--id", d.Conversations[0].ID)
	if code != 0 || !strings.Contains(out, "config broken") || !strings.Contains(out, `"active":null`) {
		t.Errorf("orphan show: code=%d %s", code, out)
	}
	code, out, errs := historyRun(t, "", "show", "--json", "--id", "nope")
	if code != 2 || out != "" || !strings.Contains(errs, "no conversation") {
		t.Errorf("unknown: code=%d out=%q err=%q", code, out, errs)
	}
}

func TestHistoryInvalidArgs(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	big := strings.Repeat("a", 4097)
	cases := []struct {
		stdin string
		args  []string
	}{
		{"", nil}, {"", []string{"bogus"}}, {"", []string{"list"}}, {"", []string{"list", "--json", "extra"}},
		{"", []string{"list", "--json", "--limit", "0"}}, {"", []string{"list", "--json", "--limit", "201"}},
		{"", []string{"list", "--json", "--limit", "x"}}, {"", []string{"list", "--json", "--offset", "-1"}},
		{"", []string{"list", "--json", "--bogus"}}, {"", []string{"list", "--json", "--search", "q"}},
		{big, []string{"list", "--json", "--search-stdin"}}, {"\xff\xfe", []string{"list", "--json", "--search-stdin"}},
		{"", []string{"show", "--json"}}, {"", []string{"show", "--id", "x"}}, {"", []string{"show", "--json", "--id", "x", "y"}},
	}
	for _, c := range cases {
		if code, out, _ := historyRun(t, c.stdin, c.args...); code != 2 || out != "" {
			t.Errorf("%v: code=%d out=%q", c.args, code, out)
		}
	}
	// A query of exactly the limit is accepted.
	if code, _, _ := historyRun(t, strings.Repeat("a", 4096), "list", "--json", "--search-stdin"); code != 0 {
		t.Errorf("max query rejected: %d", code)
	}
}

func TestHistoryErrorsDoNotLeakQueryOrReports(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	appendListRecord(t, hrec("2026-10-29 10:00:00", "s", "TASKSECRET", "RESULTSECRET"))
	const q = "QUERYSECRET"
	for _, args := range [][]string{
		{"list", "--json", "--limit", "999", "--search-stdin"},
		{"list", "--json", "--offset", "-5", "--search-stdin"},
		{"show", "--json", "--id", "RESULTSECRET"},
		{"list", "--json", "--bogus=" + q},
	} {
		code, out, errs := historyRun(t, q, args...)
		if code != 2 || out != "" {
			t.Errorf("%v: code=%d out=%q", args, code, out)
		}
		for _, secret := range []string{q, "TASKSECRET", "RESULTSECRET"} {
			if strings.Contains(errs, secret) {
				t.Errorf("%v leaked %s: %q", args, secret, errs)
			}
		}
	}
	code, _, errs := historyRun(t, strings.Repeat(q, 1000), "list", "--json", "--search-stdin")
	if code != 2 || strings.Contains(errs, q) {
		t.Errorf("oversize: code=%d err=%q", code, errs)
	}
}

func TestHistoryUnreadableLogAndWriteErrors(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	// A directory in place of the log cannot be read as one.
	if err := os.MkdirAll(filepath.Join(filepath.Dir(history.Path()), "runs.jsonl"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"list", "--json"}, {"show", "--json", "--id", "x"}} {
		code, out, errs := historyRun(t, "", args...)
		if code != 2 || out != "" || !strings.Contains(errs, "read run log") {
			t.Errorf("%v: code=%d out=%q err=%q", args, code, out, errs)
		}
	}

	t.Setenv("XDG_STATE_HOME", t.TempDir())
	appendListRecord(t, hrec("2026-10-29 10:00:00", "s", "TASKSECRET", "r"))
	for _, args := range [][]string{{"list", "--json"}, {"show", "--json", "--id", "s"}} {
		var errb bytes.Buffer
		code := cmdHistory(args, strings.NewReader(""), historyFailWriter{}, &errb)
		if code != 2 || !strings.Contains(errb.String(), "write history JSON failed") || strings.Contains(errb.String(), "TASKSECRET") {
			t.Errorf("%v: code=%d err=%q", args, code, errb.String())
		}
	}
}

type historyFailWriter struct{}

func (historyFailWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }
