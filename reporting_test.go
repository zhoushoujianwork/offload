package main

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func decodeRun(t *testing.T, out string) Record {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(out))
	var rec Record
	if err := dec.Decode(&rec); err != nil {
		t.Fatalf("decode result: %v; out=%q", err, out)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		t.Fatalf("expected one JSON object, extra=%v err=%v", extra, err)
	}
	return rec
}

func TestJSONRunReportsMeasuredElapsedAndEstimatedCost(t *testing.T) {
	work, _ := setup(t, okReply)
	cfg, err := loadConfig(configPath())
	if err != nil {
		t.Fatal(err)
	}
	prof := cfg.Profiles["cheap"]
	prof.TokenCmd = "sleep 0.06; printf tok-123"
	cfg.Profiles["cheap"] = prof
	data, _ := json.Marshal(cfg)
	if err := os.WriteFile(configPath(), data, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_STDERR", "worker diagnostic")

	code, out, errOut := runCLI("-json", "-d", work, "summarize")
	rec := decodeRun(t, out)
	if code != 0 || rec.ExitCode == nil || *rec.ExitCode != code || rec.Status != "ok" || rec.Result != "REPORT" {
		t.Fatalf("code=%d rec=%+v stderr=%q", code, rec, errOut)
	}
	if rec.ElapsedMs == nil || *rec.ElapsedMs < 50 || *rec.ElapsedMs == rec.DurationMs || rec.DurationMs != 1200 {
		t.Fatalf("must measure setup time separately from worker duration: %+v", rec)
	}
	if rec.CostUSD == nil || *rec.CostUSD != 0.01 || !rec.CostIsEstimate {
		t.Fatalf("missing estimated cost: %+v", rec)
	}
	if rec.Profile != "cheap" || rec.Model != "m-1" || rec.Session != "s-1" || rec.Turns != 3 {
		t.Fatalf("metadata: %+v", rec)
	}
	if strings.Count(errOut, "\n") != 1 || strings.Contains(errOut, "worker diagnostic") ||
		!strings.Contains(errOut, "estimated_cost=$0.0100") || !strings.Contains(errOut, "elapsed=") || !strings.Contains(errOut, "exit_code=0") {
		t.Fatalf("expected one metric summary: %q", errOut)
	}
	rs, err := readRecords(historyPath())
	if err != nil || len(rs) != 1 || rs[0].ElapsedMs == nil || *rs[0].ElapsedMs != *rec.ElapsedMs {
		t.Fatalf("history: %+v, %v", rs, err)
	}
	for _, args := range [][]string{{"ls"}, {"show", "s-1"}} {
		code, output, stderr := runCLI(args...)
		if code != 0 || !strings.Contains(output, "estimated_cost=$0.0100") || !strings.Contains(output, "elapsed=") || !strings.Contains(output, "exit_code=0") {
			t.Fatalf("history output=%q code=%d err=%q", output, code, stderr)
		}
	}
}

func TestWorkerCostAvailability(t *testing.T) {
	for _, tc := range []struct {
		name, cost string
		want       *float64
	}{
		{"missing", "", nil}, {"null", `,"total_cost_usd":null`, nil}, {"negative", `,"total_cost_usd":-1`, nil},
		{"reported zero", `,"total_cost_usd":0`, floatPtr(0)}, {"reported estimate", `,"total_cost_usd":0.125`, floatPtr(0.125)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			work, _ := setup(t, `{"result":"REPORT","session_id":"cost-session"`+tc.cost+`}`)
			code, out, errOut := runCLI("-json", "-d", work, "check")
			rec := decodeRun(t, out)
			if code != 0 || !rec.CostIsEstimate {
				t.Fatalf("code=%d rec=%+v", code, rec)
			}
			if tc.want == nil {
				if rec.CostUSD != nil || !strings.Contains(out, `"cost_usd":null`) || !strings.Contains(errOut, "estimated_cost=unknown") {
					t.Fatalf("unknown cost was fabricated: out=%q err=%q", out, errOut)
				}
			} else if rec.CostUSD == nil || *rec.CostUSD != *tc.want {
				t.Fatalf("cost=%v want=%v", rec.CostUSD, tc.want)
			}
			if tc.name == "reported zero" {
				rs, err := readRecords(historyPath())
				if err != nil || len(rs) != 1 || rs[0].CostUSD == nil || *rs[0].CostUSD != 0 {
					t.Fatalf("explicit zero must survive history: %+v err=%v", rs, err)
				}
			}
		})
	}
}

func TestFailedWorkerAndTimeoutMetrics(t *testing.T) {
	t.Run("worker exits nonzero with report", func(t *testing.T) {
		work, _ := setup(t, okReply)
		t.Setenv("FAKE_EXIT", "7")
		t.Setenv("FAKE_SLEEP", "0.03")
		code, out, errOut := runCLI("-json", "-d", work, "fail")
		rec := decodeRun(t, out)
		if code != 1 || rec.ExitCode == nil || *rec.ExitCode != 1 || rec.Status != "error" || rec.Error == "" || rec.ElapsedMs == nil || *rec.ElapsedMs < 20 || rec.CostUSD == nil {
			t.Fatalf("failure metrics: code=%d rec=%+v stderr=%q", code, rec, errOut)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		work, _ := setup(t, okReply)
		t.Setenv("FAKE_SLEEP", "1")
		started := time.Now()
		code, out, errOut := runCLI("-json", "-d", work, "-t", "40ms", "slow")
		rec := decodeRun(t, out)
		if code != 124 || rec.ExitCode == nil || *rec.ExitCode != 124 || rec.Status != "timeout" || rec.ElapsedMs == nil || *rec.ElapsedMs < 40 || rec.CostUSD != nil || rec.Error == "" {
			t.Fatalf("timeout metrics: code=%d rec=%+v stderr=%q", code, rec, errOut)
		}
		if time.Since(started) > 900*time.Millisecond {
			t.Fatal("timeout waited for a descendant's inherited output pipe")
		}
		rs, err := readRecords(historyPath())
		if err != nil || len(rs) != 1 || rs[0].ElapsedMs == nil || *rs[0].ExitCode != 124 {
			t.Fatalf("timeout history: %+v err=%v", rs, err)
		}
	})
	t.Run("invalid worker output", func(t *testing.T) {
		work, _ := setup(t, "bad output")
		code, out, _ := runCLI("-json", "-d", work, "bad")
		rec := decodeRun(t, out)
		if code != 1 || rec.ElapsedMs == nil || rec.CostUSD != nil || !strings.Contains(rec.Error, "invalid JSON") || rec.Result != "bad output" {
			t.Fatalf("invalid output metrics: %+v code=%d", rec, code)
		}
	})
}

func TestSetupErrorsHaveStructuredMetrics(t *testing.T) {
	for _, tc := range []struct {
		name   string
		modify func(t *testing.T)
		args   []string
	}{
		{"profile", func(t *testing.T) {}, []string{"-p", "missing"}},
		{"preflight", func(t *testing.T) {
			preflight = func(string) error {
				time.Sleep(20 * time.Millisecond)
				return errors.New("gateway unreachable in test")
			}
		}, nil},
		{"token", func(t *testing.T) {
			cfg, err := loadConfig(configPath())
			if err != nil {
				t.Fatal(err)
			}
			prof := cfg.Profiles["cheap"]
			prof.TokenCmd = "sleep 0.02; exit 1"
			cfg.Profiles["cheap"] = prof
			data, _ := json.Marshal(cfg)
			if err := os.WriteFile(configPath(), data, 0o600); err != nil {
				t.Fatal(err)
			}
		}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			work, _ := setup(t, okReply)
			tc.modify(t)
			args := append([]string{"-json", "-d", work}, tc.args...)
			args = append(args, "check")
			code, out, errOut := runCLI(args...)
			rec := decodeRun(t, out)
			if code != 2 || rec.ExitCode == nil || *rec.ExitCode != 2 || rec.ElapsedMs == nil || rec.CostUSD != nil || rec.Error == "" || strings.Count(errOut, "\n") != 1 {
				t.Fatalf("setup error: code=%d rec=%+v err=%q", code, rec, errOut)
			}
			if (tc.name == "token" || tc.name == "preflight") && *rec.ElapsedMs < 15 {
				t.Fatalf("setup step omitted from elapsed: %+v", rec)
			}
			rs, err := readRecords(historyPath())
			if err != nil || len(rs) != 1 || *rs[0].ExitCode != 2 {
				t.Fatalf("setup history: %+v err=%v", rs, err)
			}
		})
	}
}

func TestLegacyHistoryMetricsAreNotFabricated(t *testing.T) {
	setup(t, okReply)
	old := `{"ts":"old","status":"ok","profile":"cheap","model":"m-1","session":"old-cost","cost_usd":0.5,"duration_ms":4200,"result":"OLD","dir":"/tmp","task":"old"}` + "\n" +
		`{"ts":"old","status":"error","session":"old-zero","cost_usd":0,"duration_ms":0,"result":"FAILED"}` + "\n" +
		`{"ts":"old","status":"error","session":"old-missing","result":"FAILED"}` + "\n"
	if err := os.MkdirAll(filepath.Dir(historyPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(historyPath(), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	rs, err := readRecords(historyPath())
	if err != nil || len(rs) != 3 {
		t.Fatalf("legacy records: %+v err=%v", rs, err)
	}
	if rs[0].CostUSD == nil || *rs[0].CostUSD != 0.5 || rs[0].DurationMs != 4200 || rs[0].ElapsedMs != nil || rs[0].ExitCode != nil {
		t.Fatalf("legacy positive estimate: %+v", rs[0])
	}
	for _, rec := range rs[1:] {
		if rec.CostUSD != nil || rec.ElapsedMs != nil || rec.ExitCode != nil {
			t.Fatalf("fabricated old metrics: %+v", rec)
		}
	}
	_, out, _ := runCLI("show", "old-cost")
	if !strings.Contains(out, "estimated_cost=$0.5000") || !strings.Contains(out, "elapsed=unknown") || !strings.Contains(out, "exit_code=unknown") || !strings.Contains(out, "OLD") {
		t.Fatalf("legacy show: %q", out)
	}
	_, out, _ = runCLI("show", "old-zero")
	if !strings.Contains(out, "estimated_cost=unknown") || strings.Contains(out, "$0.0000") {
		t.Fatalf("legacy zero: %q", out)
	}
}

func floatPtr(v float64) *float64 { return &v }
