package cli

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zhoushoujianwork/offload/internal/history"
	"github.com/zhoushoujianwork/offload/internal/live"
	"github.com/zhoushoujianwork/offload/internal/slots"
)

func TestCodexToolRestrictionFailsBeforeSlotAndLaunch(t *testing.T) {
	for _, flag := range [][]string{{"-allowed-tools", "Read"}, {"-disallowed-tools", "Bash"}, {"-tools-disabled"}} {
		t.Run(flag[0], func(t *testing.T) {
			work, record := setupCodex(t, codexReply)
			args := append([]string{"-json", "-p", "codex", "-d", work}, flag...)
			code, out, _ := runCLI(append(args, "inspect")...)
			rec := decodeRun(t, out)
			if code != 2 || rec.ErrorKind != "unsupported_tools" || rec.Status != "error" {
				t.Fatal(code, rec)
			}
			for _, want := range []string{flag[0], `"codex"`, "codex backend", "-on-unsupported-tools ignore", "Remove"} {
				if !strings.Contains(rec.Error, want) {
					t.Fatalf("message lacks %q: %s", want, rec.Error)
				}
			}
			if _, err := os.Stat(record); !os.IsNotExist(err) {
				t.Fatal("provider launched")
			}
			// No worker slot was requested and no run was published as started or processing.
			if entries, _ := os.ReadDir(slots.Dir()); len(entries) != 0 {
				t.Fatalf("slot directory used: %v", entries)
			}
			if states, _ := live.List(); len(states) != 0 {
				t.Fatalf("live state published: %+v", states)
			}
			if _, err := os.Stat(history.ProcessingPath()); !os.IsNotExist(err) {
				t.Fatal("processing log written")
			}
			if rec.ToolRestriction != "" || rec.WaitMs != nil || rec.ExecMs != nil || rec.Session != "" {
				t.Fatalf("failed setup looks like a started run: %+v", rec)
			}
		})
	}
}

func TestCodexToolRestrictionRejectedBeforeEventsStartRun(t *testing.T) {
	work, record := setupCodex(t, codexReply)
	code, _, _ := runCLI("-json", "-p", "codex", "-d", work, "-allowed-tools", "Read", "-on-unsupported-tools", "bogus", "inspect")
	if code != 2 {
		t.Fatal(code)
	}
	if _, err := os.Stat(record); !os.IsNotExist(err) {
		t.Fatal("provider launched")
	}
}

func TestCodexToolRestrictionDegradeIsRecordedAndCounted(t *testing.T) {
	work, record := setupCodex(t, codexReply)
	code, out, errOut := runCLI("-json", "-p", "codex", "-d", work, "-allowed-tools", "Read", "-on-unsupported-tools", "ignore", "inspect")
	rec := decodeRun(t, out)
	if code != 0 || rec.Status != "ok" || rec.Result != "CODEX REPORT" {
		t.Fatalf("%d %+v %s", code, rec, errOut)
	}
	if rec.ToolRestriction != "not_enforced" || !strings.Contains(out, `"tool_restriction":"not_enforced"`) {
		t.Fatalf("result does not show the restriction was ignored: %s", out)
	}
	if !strings.Contains(errOut, "tool_restriction=not_enforced") {
		t.Fatalf("meta line hides it: %s", errOut)
	}
	argv, err := os.ReadFile(record)
	if err != nil {
		t.Fatal("run did not execute")
	}
	if strings.Contains(strings.ToLower(string(argv)), "allowed") {
		t.Fatalf("restriction leaked into Codex argv: %s", argv)
	}
	rs, err := history.Read(history.Path())
	if err != nil || len(rs) != 1 || rs[0].ToolRestriction != "not_enforced" {
		t.Fatalf("history: %+v %v", rs, err)
	}
	// A second, unrestricted run must not be counted as not enforced.
	if code, _, e := runCLI("-json", "-p", "codex", "-d", work, "inspect"); code != 0 {
		t.Fatal(e)
	}
	code, sout, _ := statsRun(t, time.Now(), "--json", "--period", "all")
	if code != 0 || !strings.Contains(sout, `"tool_restriction_not_enforced":1`) || !strings.Contains(sout, `"runs":2`) ||
		!strings.Contains(sout, `"tool_restriction":"not_enforced"`) {
		t.Fatal(code, sout)
	}
	code, text, _ := statsRun(t, time.Now(), "--period", "all")
	if code != 0 || !strings.Contains(text, "tool restriction not enforced: 1 of 2 runs") {
		t.Fatal(code, text)
	}
}

func TestClaudeKeepsEnforcingToolRestrictions(t *testing.T) {
	for _, mode := range []string{"", "ignore", "fail"} {
		t.Run("mode="+mode, func(t *testing.T) {
			work, record := setup(t, okReply)
			args := []string{"-json", "-d", work, "-allowed-tools", "Read,Grep", "-disallowed-tools", "Bash"}
			if mode != "" {
				args = append(args, "-on-unsupported-tools", mode)
			}
			code, out, _ := runCLI(append(args, "inspect")...)
			rec := decodeRun(t, out)
			if code != 0 || rec.ToolRestriction != "" {
				t.Fatalf("%d %+v", code, rec)
			}
			argv, _ := os.ReadFile(record)
			for _, want := range []string{"--allowedTools\nRead Grep", "--disallowedTools\nBash"} {
				if !strings.Contains(string(argv), want) {
					t.Fatalf("Claude argv lacks %q: %s", want, argv)
				}
			}
		})
	}
}

func TestProfilesShowToolRestrictionSupport(t *testing.T) {
	profileCfg(t, profileListCfg)
	_, out, _ := runProfileCLI([]string{"profiles", "--json"}, "")
	snap := decodeSnap(t, out)
	got := map[string]bool{}
	for _, p := range snap.Profiles {
		got[p.Name] = p.ToolRestrictions
	}
	if !got["zeta"] || !got["alpha"] || got["cx"] || len(got) != 3 {
		t.Fatal(got)
	}
	_, text, _ := runProfileCLI([]string{"profiles"}, "")
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		want := "tool-limits=yes"
		if strings.Contains(line, "codex") {
			want = "tool-limits=no"
		}
		if !strings.Contains(line, want) {
			t.Fatalf("%q lacks %q", line, want)
		}
	}
}

func TestScheduleRejectsCodexToolRestrictionUnlessIgnored(t *testing.T) {
	work, _ := setupCodex(t, codexReply)
	base := []string{"schedule", "add", "--every", "1h", "-p", "codex", "-d", work, "--allowed-tools", "Read"}
	if code, _, e := runCLI(append(base, "task")...); code == 0 || !strings.Contains(e, "-allowed-tools cannot be enforced") {
		t.Fatal(code, e)
	}
	if code, _, e := runCLI(append(base, "--on-unsupported-tools", "ignore", "task")...); code != 0 {
		t.Fatal(code, e)
	}
}
