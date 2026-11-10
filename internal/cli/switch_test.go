package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhoushoujianwork/offload/internal/config"
	"github.com/zhoushoujianwork/offload/internal/history"
	"github.com/zhoushoujianwork/offload/internal/live"
	"github.com/zhoushoujianwork/offload/internal/runcontrol"
)

func switchHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	return home
}

func decodeSwitch(t *testing.T, out string) switchDocument {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(out))
	dec.DisallowUnknownFields()
	var d switchDocument
	if err := dec.Decode(&d); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	if dec.More() {
		t.Fatalf("more than one JSON document: %q", out)
	}
	return d
}

func TestSwitchJSONContractAndPersistence(t *testing.T) {
	work, record := setup(t, okReply)
	switchHome(t)
	if err := config.SetMode(config.Path(), "basic"); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(config.Path())
	if err != nil {
		t.Fatal(err)
	}

	code, out, e := runCLI("status", "--json")
	d := decodeSwitch(t, out)
	if code != 0 || !d.Enabled || d.NewExecutions != "allowed" || d.ErrorKind != "" || d.Running.Handling != "" || d.SchemaVersion != 1 {
		t.Fatalf("enabled status: code=%d %s %s", code, out, e)
	}

	code, out, e = runCLI("disable", "--json")
	d = decodeSwitch(t, out)
	if code != 0 || d.Enabled || d.NewExecutions != "blocked" || d.ErrorKind != "globally_disabled" || d.Reason != config.ErrDisabled.Error() ||
		d.Running.Handling != "not_stopped" || d.Instructions == nil || !d.Instructions.Updated || !d.Instructions.NewSessionRequired {
		t.Fatalf("disable: code=%d %s %s", code, out, e)
	}
	if strings.TrimSpace(e) != "" {
		t.Fatalf("JSON form must keep stderr quiet on success: %q", e)
	}

	// Every later invocation reads the same persisted state, so this is also the restart check.
	code, out, _ = runCLI("status", "--json")
	d = decodeSwitch(t, out)
	if code != 0 || d.Enabled || d.NewExecutions != "blocked" {
		t.Fatalf("status after disable: %d %s", code, out)
	}

	// New tasks and a different profile are refused with the stable kind, and no backend starts.
	for _, args := range [][]string{{"-json", "-d", work, "task"}, {"-json", "-p", "cheap", "-d", work, "task"}, {"-json", "-r", "old-session", "-d", work, "task"}} {
		code, out, e := runCLI(args...)
		if rec := decodeRun(t, out); code != 2 || rec.ErrorKind != "globally_disabled" || !strings.Contains(rec.Error, "globally disabled") {
			t.Fatalf("%v: code=%d %s %s", args, code, out, e)
		}
	}
	if _, err := os.Stat(record); !os.IsNotExist(err) {
		t.Fatalf("backend started while disabled: %v", err)
	}

	// Management stays available while disabled.
	for _, args := range [][]string{{"status"}, {"profiles", "--json"}, {"ls"}, {"agent", "status", "--json"}, {"mode"}} {
		if code, out, e := runCLI(args...); code != 0 {
			t.Fatalf("%v unavailable while disabled: %d %s %s", args, code, out, e)
		}
	}
	if _, out, _ := runCLI("status"); !strings.Contains(out, "Offload globally disabled: true") || !strings.Contains(out, "error_kind=globally_disabled") {
		t.Fatalf("text status: %q", out)
	}

	code, out, e = runCLI("enable", "--json")
	d = decodeSwitch(t, out)
	if code != 0 || !d.Enabled || d.NewExecutions != "allowed" || d.Running.Handling != "" {
		t.Fatalf("enable: %d %s %s", code, out, e)
	}
	if code, _, e := runCLI("-d", work, "task"); code != 0 {
		t.Fatalf("task after enable: %d %s", code, e)
	}

	// Profiles, default and the delegation mode survive the round trip.
	after, _ := os.ReadFile(config.Path())
	var b, a map[string]json.RawMessage
	_ = json.Unmarshal(before, &b)
	_ = json.Unmarshal(after, &a)
	for k, want := range b {
		if string(a[k]) != string(want) && k != "disabled" {
			t.Fatalf("%s changed: %s -> %s", k, want, a[k])
		}
	}
}

func TestSwitchJSONReportsRunningWorkWithoutStoppingIt(t *testing.T) {
	setup(t, okReply)
	switchHome(t)
	now := time.Now()
	for i, phase := range []string{live.Running, live.Running, live.Queued} {
		if err := live.Publish(live.State{PID: os.Getpid() + 100000 + i, Phase: phase, StartedAt: now, LastEventAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	code, out, e := runCLI("disable", "--json")
	d := decodeSwitch(t, out)
	if code != 0 || d.Running.Status != "ok" || d.Running.Processing == nil || *d.Running.Processing != 2 ||
		d.Running.Queued == nil || *d.Running.Queued != 1 || d.Running.Active == nil || *d.Running.Active != 3 || d.Running.Handling != "not_stopped" {
		t.Fatalf("running counts: %d %s %s", code, out, e)
	}
	// Disabling does not rewrite or remove any live state.
	states, err := live.List()
	if err != nil || len(states) != 3 {
		t.Fatalf("live states changed: %v %v", len(states), err)
	}
}

func TestSwitchJSONSavedButInstructionsConflict(t *testing.T) {
	setup(t, okReply)
	home := switchHome(t)
	skill := filepath.Join(home, ".agents", "skills", "offload", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(skill), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(skill, []byte("user-owned skill"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, _ := runCLI("disable", "--json")
	d := decodeSwitch(t, out)
	if code != 2 || d.Enabled || d.Instructions == nil || d.Instructions.Updated || d.Instructions.Error == "" {
		t.Fatalf("conflict: %d %s", code, out)
	}
	if config.CheckEnabled() != config.ErrDisabled {
		t.Fatal("the switch must stay saved when instructions conflict")
	}
	if b, _ := os.ReadFile(skill); string(b) != "user-owned skill" {
		t.Fatal("custom skill overwritten")
	}
}

func TestSwitchRejectsUnexpectedArguments(t *testing.T) {
	setup(t, okReply)
	switchHome(t)
	for _, args := range [][]string{{"disable", "now"}, {"enable", "--bogus"}, {"status", "extra"}, {"status", "--json", "extra"}} {
		if code, _, _ := runCLI(args...); code == 0 {
			t.Fatalf("%v accepted", args)
		}
	}
	if err := config.CheckEnabled(); err != nil {
		t.Fatalf("a rejected command changed the switch: %v", err)
	}
}

func TestDisabledRefusesProfileProbeAndKeepsAvailability(t *testing.T) {
	path := setupRetest(t)
	switchHome(t)
	if err := config.SetUnavailable(path, "a", "authentication failed: old", time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	calls := stubProbe(t, probeOutcome{OK: true})
	if err := config.SetDisabled(path, true); err != nil {
		t.Fatal(err)
	}
	cfgBefore, _ := os.ReadFile(path)

	var out, errb bytes.Buffer
	if code := cmdProfileTest([]string{"a"}, &out, &errb); code != 2 || *calls != 0 ||
		!strings.Contains(out.String(), "error_kind=globally_disabled") || !strings.Contains(errb.String(), "globally disabled") {
		t.Fatalf("manual check: code=%d calls=%d out=%q err=%q", code, *calls, out.String(), errb.String())
	}
	if _, err := retestProfile(t.Context(), path, "a"); err != config.ErrDisabled || *calls != 0 {
		t.Fatalf("automatic path: err=%v calls=%d", err, *calls)
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(after, cfgBefore) {
		t.Fatalf("a refused check rewrote the configuration:\n%s", after)
	}

	// A worker-level refusal during the probe is not recorded as a profile failure either.
	if err := config.SetDisabled(path, false); err != nil {
		t.Fatal(err)
	}
	old := probeProfile
	probeProfile = func(_ context.Context, p, _ string) probeOutcome {
		_ = config.SetDisabled(p, true)
		return probeOutcome{ErrorKind: config.DisabledErrorKind, Message: config.ErrDisabled.Error()}
	}
	t.Cleanup(func() { probeProfile = old })
	if _, err := retestProfile(t.Context(), path, "b"); err != config.ErrDisabled {
		t.Fatalf("err=%v", err)
	}
	cfg, _ := config.Load(path)
	if cfg.Profiles["b"].Unavailable != nil {
		t.Fatalf("a disabled refusal marked the profile unavailable: %+v", cfg.Profiles["b"].Unavailable)
	}
}

func TestDisabledRefusesTaskResumeBeforeLaunch(t *testing.T) {
	work, _ := setup(t, okReply)
	switchHome(t)
	t.Setenv("FAKE_EXIT", "1")
	code, out, _ := runCLI("-json", "-d", work, "-idle", "0", "inspect")
	if code == 0 {
		t.Fatal("fixture must fail")
	}
	first := decodeRun(t, out)
	marker := filepath.Join(t.TempDir(), "resume-launched")
	m, err := runcontrol.Read(first.RunID)
	if err != nil {
		t.Fatal(err)
	}
	m.ResumeCommand = []string{"/bin/sh", "-c", "echo launched > " + marker}
	m.ResumeDirectory = work
	raw, _ := json.Marshal(m)
	if err := os.WriteFile(runcontrol.Path(first.RunID), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := config.SetDisabled(config.Path(), true); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"task", "resume", "--id", first.RunID, "--json"}, {"resume", "--id", first.RunID, "--json"}} {
		code, out, e := runCLI(args...)
		if code != 2 || !strings.Contains(out+e, "globally disabled") {
			t.Fatalf("%v: code=%d %s %s", args, code, out, e)
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("saved resume entry point launched while disabled: %v", err)
	}
	// The task document says why, instead of only reporting that resume is unavailable.
	if d, err := taskSnapshot(first.RunID); err != nil || d.CanResume || d.ResumeReason != config.ErrDisabled.Error() {
		t.Fatalf("task show while disabled: %+v %v", d, err)
	}
	// The source run stays resumable after the switch is turned back on.
	if _, found, err := history.FindRun(first.RunID); err != nil || !found {
		t.Fatalf("history lost: %v %v", found, err)
	}
	if err := config.SetDisabled(config.Path(), false); err != nil {
		t.Fatal(err)
	}
	if d, err := taskSnapshot(first.RunID); err != nil || !d.CanResume {
		t.Fatalf("resume unavailable after enable: %+v %v", d, err)
	}
}
