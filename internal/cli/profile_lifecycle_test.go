package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhoushoujianwork/offload/internal/config"
	"github.com/zhoushoujianwork/offload/internal/live"
	"github.com/zhoushoujianwork/offload/internal/schedule"
)

const lifecycleConfig = `{"default":"a","future":42,"profiles":{"a":{"backend":"codex","future":17},"b":{"backend":"codex","aliases":["bee"]}}}`

func lifecycleSetup(t *testing.T) string {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	return profileCfg(t, lifecycleConfig)
}

func TestProfileDisableCommandsAndJSON(t *testing.T) {
	path := lifecycleSetup(t)
	for _, command := range []string{"disable", "enable"} {
		code, out, err := runProfileCLI([]string{"profile", command, "bee", "--json"}, "")
		if code != 0 {
			t.Fatalf("%s: %d %s", command, code, err)
		}
		snap := decodeSnap(t, out)
		p := snap.Profiles[1]
		if p.Disabled != (command == "disable") || p.Available == p.Disabled || !strings.Contains(out, `"disabled":`) || p.UnavailableReason != "" || p.UnavailableAt != "" {
			t.Fatal(out)
		}
	}
	for _, req := range []string{`{"name":"b","disabled":true}`, `{"name":"b","use_for":"keep disabled"}`, `{"name":"b","disabled":false}`} {
		code, _, err := runProfileCLI([]string{"profile", "save", "--json"}, req)
		if code != 0 {
			t.Fatal(err)
		}
		cfg, _ := config.Load(path)
		if cfg.Profiles["b"].Disabled == strings.Contains(req, `"disabled":false`) {
			t.Fatal("patch lost switch", req)
		}
	}
	for _, args := range [][]string{{"profile", "disable", "a"}, {"profile", "save", "--json"}} {
		before, _ := os.ReadFile(path)
		code, _, err := runProfileCLI(args, `{"name":"a","disabled":true}`)
		after, _ := os.ReadFile(path)
		if code != 2 || !strings.Contains(err, "set another default first") || string(before) != string(after) {
			t.Fatalf("%v: %d %s", args, code, err)
		}
	}
}

func TestDisabledRunEntryPoints(t *testing.T) {
	for _, kind := range []string{"explicit", "environment", "resume", "thread", "schedule", "scheduled-run"} {
		t.Run(kind, func(t *testing.T) {
			work, record := setup(t, okReply)
			var args []string
			if kind == "resume" || kind == "thread" {
				args = []string{"-d", work, "initial"}
				if kind == "thread" {
					args = []string{"-thread", "disabled-case", "-d", work, "initial"}
				}
				if code, _, err := runCLI(args...); code != 0 {
					t.Fatal(err)
				}
				if err := os.Remove(record); err != nil {
					t.Fatal(err)
				}
			}
			cfg, err := config.Load(config.Path())
			if err != nil {
				t.Fatal(err)
			}
			cfg.Default = "other"
			cfg.Profiles["other"] = cfg.Profiles["cheap"]
			if err := config.SetDefault(config.Path(), "cheap"); err != nil {
				t.Fatal(err)
			}
			// Add a new default through SaveProfile, then disable the original.
			if err := config.SaveProfile(config.Path(), "other", cfg.Profiles["other"], true); err != nil {
				t.Fatal(err)
			}
			if err := config.SetProfileDisabled(config.Path(), "cheap", true); err != nil {
				t.Fatal(err)
			}
			args = []string{"-d", work, "-p", "cheap", "blocked"}
			switch kind {
			case "environment":
				t.Setenv("OFFLOAD_PROFILE", "cheap")
				args = []string{"-d", work, "blocked"}
			case "resume":
				args = []string{"-r", "s-1", "blocked"}
			case "thread":
				args = []string{"-thread", "disabled-case", "-d", work, "blocked"}
			case "scheduled-run":
				args = append(schedule.BuildArgs(&schedule.Task{ID: "t-disabled", Profile: "cheap", Dir: work, Policy: "default"}, "test"), "blocked")
			case "schedule":
				args = []string{"schedule", "add", "--every", "1h", "-d", work, "-p", "cheap", "blocked"}
			}
			code, out, errOut := runCLI(args...)
			if code != 2 || !strings.Contains(out+errOut, `profile "cheap" is disabled; enable it with: offload profile enable cheap`) {
				t.Fatalf("%d %s %s", code, out, errOut)
			}
			if _, err := os.Stat(record); !os.IsNotExist(err) {
				t.Fatal("worker started", err)
			}
		})
	}
}

func TestDisabledProfileTestSuccessPreservesSwitch(t *testing.T) {
	path := setupRetest(t)
	if err := config.SetProfileDisabled(path, "b", true); err != nil {
		t.Fatal(err)
	}
	stubProbe(t, probeOutcome{OK: true})
	code, _, err := runProfileCLI([]string{"profile", "test", "b"}, "")
	if code != 0 {
		t.Fatal(err)
	}
	cfg, _ := config.Load(path)
	if !cfg.Profiles["b"].Disabled || cfg.Profiles["b"].Available() {
		t.Fatal("probe enabled disabled profile")
	}
}

func TestDeleteProfileGuardsAndPreservation(t *testing.T) {
	for _, guard := range []string{"default", "running", "queued", "schedule", "delete"} {
		t.Run(guard, func(t *testing.T) {
			path := lifecycleSetup(t)
			name := "bee"
			switch guard {
			case "default":
				name = "a"
			case "running", "queued":
				phase := live.Running
				if guard == "queued" {
					phase = live.Queued
				}
				if err := live.Publish(live.State{PID: os.Getpid(), Phase: phase, Profile: "b", StartedAt: time.Now()}); err != nil {
					t.Fatal(err)
				}
			case "schedule":
				if err := schedule.NewStore("").Save(&schedule.Task{ID: "t-reference", Profile: "bee"}); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := os.ReadFile(path)
			code, _, err := runProfileCLI([]string{"profile", "delete", name, "--json"}, "")
			after, _ := os.ReadFile(path)
			if guard != "delete" {
				if code != 2 || string(before) != string(after) {
					t.Fatalf("%d %s changed=%t", code, err, string(before) != string(after))
				}
				if guard == "schedule" && !strings.Contains(err, "t-reference") {
					t.Fatal(err)
				}
			} else {
				if code != 0 {
					t.Fatal(err)
				}
				cfg, _ := config.Load(path)
				if len(cfg.Profiles) != 1 || !strings.Contains(string(after), `"future": 42`) || !strings.Contains(string(after), `"future": 17`) {
					t.Fatal(string(after))
				}
			}
		})
	}
}

func TestAccountRemoveAndDeleteAccountGuards(t *testing.T) {
	for _, kind := range []string{"referenced", "delete-shared", "active", "queued", "unsigned", "linked", "delete-account", "delete-active-account", "no-account"} {
		t.Run(kind, func(t *testing.T) {
			path := lifecycleSetup(t)
			home, err := config.CreateManagedAccount("work")
			if err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(t.TempDir(), "auth.json")
			if err := os.WriteFile(target, []byte("do not touch"), 0600); err != nil {
				t.Fatal(err)
			}
			if kind == "linked" {
				if err := os.Symlink(target, filepath.Join(home, "auth.json")); err != nil {
					t.Fatal(err)
				}
			}
			raw := lifecycleConfig
			if kind == "referenced" || kind == "delete-shared" || kind == "delete-account" || kind == "delete-active-account" {
				raw = strings.Replace(raw, `"b":{"backend":"codex",`, `"b":{"backend":"codex","codex_account":"work",`, 1)
			}
			if kind == "delete-shared" {
				raw = strings.Replace(raw, `"a":{"backend":"codex",`, `"a":{"backend":"codex","codex_account":"work",`, 1)
			}
			if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			if kind == "active" || kind == "queued" || kind == "delete-active-account" {
				phase := live.Running
				if kind == "queued" {
					phase = live.Queued
				}
				if err := live.Publish(live.State{PID: os.Getpid(), Phase: phase, Profile: "old-profile", CodexAccount: "work", StartedAt: time.Now()}); err != nil {
					t.Fatal(err)
				}
			}
			args := []string{"accounts", "remove", "work", "--json"}
			if kind == "delete-account" || kind == "delete-shared" || kind == "delete-active-account" || kind == "no-account" {
				args = []string{"profile", "delete", "b", "--account", "--json"}
			}
			code, _, errOut := runProfileCLI(args, "")
			success := kind == "unsigned" || kind == "linked" || kind == "delete-account"
			if success {
				if code != 0 {
					t.Fatal(code, errOut)
				}
				if _, err := os.Stat(home); !os.IsNotExist(err) {
					t.Fatal("account not removed", err)
				}
			} else {
				after, _ := os.ReadFile(path)
				if code != 2 || string(after) != raw {
					t.Fatal(code, errOut, string(after))
				}
				if _, err := os.Stat(home); err != nil {
					t.Fatal("account changed on refusal", err)
				}
			}
			data, err := os.ReadFile(target)
			if err != nil || string(data) != "do not touch" {
				t.Fatal("touched link target", err)
			}
		})
	}
}

func TestDisabledProfileRealProbeKeepsSwitch(t *testing.T) {
	_, record := setup(t, okReply)
	cfg, err := config.Load(config.Path())
	if err != nil {
		t.Fatal(err)
	}
	if err := config.SaveProfile(config.Path(), "other", cfg.Profiles["cheap"], true); err != nil {
		t.Fatal(err)
	}
	if err := config.SetProfileDisabled(config.Path(), "cheap", true); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := runProfileCLI([]string{"profile", "test", "cheap"}, "")
	if code != 0 {
		t.Fatalf("%d %s %s", code, out, errOut)
	}
	if _, err := os.Stat(record); err != nil {
		t.Fatal("diagnostic worker did not start", err)
	}
	cfg, err = config.Load(config.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Profiles["cheap"].Disabled || cfg.Profiles["cheap"].Available() {
		t.Fatal("probe changed switch")
	}
}

func TestDisabledOnlyPatchDoesNotRequireAccountSignIn(t *testing.T) {
	path := lifecycleSetup(t)
	raw := strings.Replace(lifecycleConfig, `"b":{"backend":"codex",`, `"b":{"backend":"codex","codex_account":"unsigned",`, 1)
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"true", "false"} {
		code, _, err := runProfileCLI([]string{"profile", "save", "--json"}, `{"name":"b","disabled":`+value+`}`)
		if code != 0 {
			t.Fatal(code, err)
		}
	}
}
