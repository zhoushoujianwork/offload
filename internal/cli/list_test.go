package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode"

	"github.com/chzyer/readline"
	"github.com/zhoushoujianwork/offload/internal/history"
)

func appendListRecord(t *testing.T, r history.Record) {
	t.Helper()
	if err := history.Append(history.Path(), r); err != nil {
		t.Fatal(err)
	}
}

func TestListEmptyHistory(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	for _, emptyFile := range []bool{false, true} {
		if emptyFile {
			if err := os.MkdirAll(filepath.Dir(history.Path()), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(history.Path(), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if code, out, errOut := runCLI("ls"); code != 0 || out != "No runs yet.\n" || errOut != "" {
			t.Fatalf("emptyFile=%v: code=%d out=%q stderr=%q", emptyFile, code, out, errOut)
		}
	}
}

func TestListCountAndAppendOrder(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	for i := 0; i < 12; i++ {
		// Reverse timestamps ensure display follows append order, not timestamp order.
		appendListRecord(t, history.Record{TS: fmt.Sprintf("time-%02d", 12-i), Session: fmt.Sprintf("session-%02d-full-exact-id", i)})
	}
	for _, tc := range []struct {
		args  []string
		start int
	}{
		{[]string{"ls"}, 2}, {[]string{"ls", "3"}, 9}, {[]string{"ls", "20"}, 0},
	} {
		code, out, errOut := runCLI(tc.args...)
		if code != 0 || errOut != "" {
			t.Fatalf("%v: code=%d stderr=%q", tc.args, code, errOut)
		}
		blocks := strings.Split(strings.TrimSuffix(out, "\n"), "\n\n")
		if len(blocks) != 12-tc.start {
			t.Fatalf("%v: got %d blocks", tc.args, len(blocks))
		}
		for i, block := range blocks {
			lines := strings.Split(block, "\n")
			want := fmt.Sprintf("  Session: session-%02d-full-exact-id", tc.start+i)
			if len(lines) != 4 || lines[3] != want {
				t.Fatalf("block %d: %q, want fourth line %q", i, block, want)
			}
		}
	}
	for _, args := range [][]string{{"ls", "0"}, {"ls", "-1"}, {"ls", "no"}, {"ls", "1", "extra"}} {
		if code, out, errOut := runCLI(args...); code != 2 || out != "" || !strings.Contains(errOut, "usage: offload ls [n]") {
			t.Errorf("%v: code=%d out=%q stderr=%q", args, code, out, errOut)
		}
	}
}

func TestListSummaryMetrics(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	zero, one, timeout := 0, 1, 124
	ms := int64(107056)
	appendListRecord(t, history.Record{TS: "2026-10-26T10:00:00Z", Status: "ok", ExitCode: &zero,
		Profile: "cheap", Model: "m-1", Dir: "/private/work/app", Task: "review", Session: "full-session-id",
		Turns: 4, CostUSD: floatPtr(0), CostIsEstimate: true, ElapsedMs: &ms})
	appendListRecord(t, history.Record{TS: "legacy", Status: "error", ExitCode: &one, CostUSD: floatPtr(0), DurationMs: 4200})
	appendListRecord(t, history.Record{TS: "stopped", Status: "timeout", ExitCode: &timeout, CostUSD: floatPtr(0.25), CostIsEstimate: true})
	appendListRecord(t, history.Record{TS: "unknown", Status: "ok"})
	code, out, errOut := runCLI("ls")
	if code != 0 || errOut != "" {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	wants := []string{
		"2026-10-26T10:00:00Z | OK | 1m47s | 4 turns | est. $0.0000\n  app | cheap / m-1\n  review\n  Session: full-session-id",
		"legacy | ERROR (exit=1) | elapsed unknown | 0 turns | cost unavailable (legacy zero is ambiguous)",
		"stopped | TIMEOUT (exit=124) | elapsed unknown | 0 turns | est. $0.2500",
		"unknown | OK (exit=unknown) | elapsed unknown | 0 turns | cost unavailable (worker did not report cost)",
		"  Session: unavailable",
	}
	for _, want := range wants {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %q", want, out)
		}
	}
	for _, unwanted := range []string{"exit=0", "/private/work", "4200", "\x1b", "calls", "billed"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("unexpected %q in %q", unwanted, out)
		}
	}
}

func TestListElapsed(t *testing.T) {
	for _, tc := range []struct {
		ms   int64
		want string
	}{{0, "0ms"}, {6, "6ms"}, {999, "999ms"}, {1000, "1s"}, {107056, "1m47s"}, {3723999, "1h2m3s"}, {-1, "elapsed unknown"}} {
		if got := listElapsed(&tc.ms); got != tc.want {
			t.Errorf("%dms: got %q want %q", tc.ms, got, tc.want)
		}
	}
	if got := listElapsed(nil); got != "elapsed unknown" {
		t.Errorf("nil: %q", got)
	}
}

func TestListUnicodeTaskAndLongProfile(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	profile := strings.Repeat("long-profile-", 15) + "GPT-5.4"
	appendListRecord(t, history.Record{Profile: profile, Model: "gpt-5.4", Task: "start\n\t\x1b\x00" + strings.Repeat("界", 80), Session: "exact-1234567890-abcdefghijklmnopqrstuvwxyz"})
	_, out, _ := runCLI("ls")
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != 4 || lines[1] != "  project unavailable | "+profile || lines[3] != "  Session: exact-1234567890-abcdefghijklmnopqrstuvwxyz" {
		t.Fatalf("summary: %q", out)
	}
	task := strings.TrimPrefix(lines[2], "  ")
	if width := (readline.Runes{}).WidthAll([]rune(task)); width > 100 || width < 98 || !strings.HasSuffix(task, "...") {
		t.Errorf("task width=%d task=%q", width, task)
	}
	for _, r := range task {
		if unicode.IsControl(r) {
			t.Errorf("unsanitized control %U", r)
		}
	}
}

// Saved run fixtures, one per cost case; no worker or paid model is invoked.
const costFixtures = `{"ts":"t-available","status":"ok","exit_code":0,"backend":"claude","cost_usd":0.0753,"cost_is_estimate":true,"session":"s1"}
{"ts":"t-zero","status":"ok","exit_code":0,"backend":"claude","cost_usd":0,"cost_is_estimate":true,"session":"s2"}
{"ts":"t-missing","status":"ok","exit_code":0,"backend":"claude","cost_usd":null,"cost_is_estimate":true,"session":"s3"}
{"ts":"t-legacy-nonzero","status":"ok","exit_code":0,"cost_usd":0.5,"session":"s4"}
{"ts":"t-legacy-zero","status":"ok","exit_code":0,"cost_usd":0,"session":"s5"}
{"ts":"t-codex","status":"ok","exit_code":0,"backend":"codex","cost_usd":null,"cost_is_estimate":false,"session":"s6"}
{"ts":"t-invalid","status":"ok","exit_code":0,"cost_usd":-1,"cost_is_estimate":true,"session":"s7"}
`

func TestListCostFromSavedFixtures(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := os.MkdirAll(filepath.Dir(history.Path()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(history.Path(), []byte(costFixtures), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := runCLI("ls", "20")
	if code != 0 || errOut != "" {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	for ts, want := range map[string]string{
		"t-available":      "est. $0.0753",
		"t-zero":           "est. $0.0000",
		"t-missing":        "cost unavailable (worker did not report cost)",
		"t-legacy-nonzero": "est. $0.5000",
		"t-legacy-zero":    "cost unavailable (legacy zero is ambiguous)",
		"t-codex":          "cost unavailable (Codex does not report USD cost)",
		"t-invalid":        "cost unavailable (invalid value recorded)",
	} {
		if !strings.Contains(out, ts+" | OK | elapsed unknown | 0 turns | "+want+"\n") {
			t.Errorf("%s: want %q in %q", ts, want, out)
		}
	}
	// Unavailable must never be rendered as a zero price.
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "unavailable (") && strings.Contains(line, "$") {
			t.Errorf("unavailable cost shows a price: %q", line)
		}
	}
	if strings.Contains(out, "cost unknown") {
		t.Errorf("stale unexplained wording in %q", out)
	}
}
