package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/zhoushoujianwork/offload/internal/history"
)

func terminated(ts, session, class string) history.Record {
	return history.Record{TS: ts, Status: "error", Session: session, Dir: "/p/x", Model: "m",
		Termination: &history.Termination{Class: class, SupervisorRequested: class != history.ClassKilled}}
}

// stats counts canceled, interrupted and killed runs separately, in text and JSON, and the
// recent list carries each run's class.
func TestStatsCountsTerminationClassesSeparately(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.FixedZone("X", 3600))
	appendListRecord(t, history.Record{TS: "2026-10-29 11:50:00", Status: "ok", Session: "ok"})
	appendListRecord(t, history.Record{TS: "2026-10-29 11:51:00", Status: "error", Session: "plain"})
	appendListRecord(t, terminated("2026-10-29 11:52:00", "c1", history.ClassCanceled))
	appendListRecord(t, terminated("2026-10-29 11:53:00", "i1", history.ClassInterrupted))
	appendListRecord(t, terminated("2026-10-29 11:54:00", "i2", history.ClassInterrupted))
	appendListRecord(t, terminated("2026-10-29 11:55:00", "k1", history.ClassKilled))

	code, out, _ := statsRun(t, now, "--json")
	if code != 0 {
		t.Fatalf("code=%d", code)
	}
	var doc struct {
		Totals map[string]json.RawMessage `json:"totals"`
		Recent []struct {
			Session          string `json:"session"`
			TerminationClass string `json:"termination_class"`
		} `json:"recent"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{"runs": "6", "error": "5", "canceled": "1", "interrupted": "2", "killed": "1"} {
		if string(doc.Totals[k]) != want {
			t.Errorf("totals.%s = %s, want %s", k, doc.Totals[k], want)
		}
	}
	classes := map[string]string{}
	for _, r := range doc.Recent {
		classes[r.Session] = r.TerminationClass
	}
	for session, want := range map[string]string{"ok": "", "plain": "", "c1": "canceled", "i1": "interrupted", "i2": "interrupted", "k1": "killed"} {
		if got, ok := classes[session]; !ok || got != want {
			t.Errorf("recent[%s] class = %q (present %v), want %q", session, got, ok, want)
		}
	}

	_, text, _ := statsRun(t, now)
	if want := "ended without a result: canceled 1 (requested), interrupted 2 (shutdown, resumable), killed 1 (unrequested)"; !strings.Contains(text, want) {
		t.Errorf("text stats missing %q:\n%s", want, text)
	}
}

func TestListShowsTerminationClass(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	code1 := 1
	r := terminated("2026-10-29 11:55:00", "k1", history.ClassKilled)
	r.ExitCode = &code1
	appendListRecord(t, r)
	_, out, _ := runCLI("ls")
	if !strings.Contains(out, "ERROR (exit=1) [killed]") {
		t.Errorf("ls does not show the class:\n%s", out)
	}
}
