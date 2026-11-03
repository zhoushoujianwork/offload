package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhoushoujianwork/offload/internal/config"
)

func TestRunRecoversMissingTerminalUsageFromNativeTurn(t *testing.T) {
	stateEnv(t)
	o := testOpts(t)
	o.Model = "gpt-6.1-sol"
	o.Profile = config.Profile{Backend: "codex", Model: o.Model, ServiceTier: "priority"}
	o.StartedAt = time.Now().Add(-10 * time.Second)
	stamp := "NATIVE_TIME"
	dir, _ := json.Marshal(o.Dir)
	task, _ := json.Marshal(o.Task)
	native := fmt.Sprintf(`{"type":"session_meta","payload":{"id":"%s"}}
{"type":"event_msg","timestamp":"%s","payload":{"type":"task_started","turn_id":"turn"}}
{"type":"turn_context","payload":{"turn_id":"turn","cwd":%s,"model":"gpt-6.1-sol"}}
{"type":"response_item","payload":{"type":"message","role":"user","content":[{"text":%s}]}}
{"type":"event_msg","payload":{"type":"thread_settings_applied","thread_settings":{"service_tier":"priority"}}}
{"type":"token_usage_record","timestamp":"%s","payload":{"turn_id":"turn","turn_token_usage":%s,"thread_token_usage":%s}}
{"type":"event_msg","timestamp":"%s","payload":{"type":"task_complete","turn_id":"turn"}}
`, usageSession, stamp, dir, task, stamp, freshUsage, freshUsage, stamp)
	path := writeBaseline(t, native)
	bin := filepath.Join(t.TempDir(), "codex")
	template := filepath.Join(filepath.Dir(bin), "native-template")
	if err := os.WriteFile(template, []byte(native), 0600); err != nil {
		t.Fatal(err)
	}
	quote := func(v string) string { return "'" + strings.ReplaceAll(v, "'", "'\"'\"'") + "'" }
	script := "#!/bin/sh\nstamp=$(date -u +%Y-%m-%dT%H:%M:%SZ)\nsed \"s/NATIVE_TIME/$stamp/g\" " + quote(template) + " > " + quote(path) + "\nprintf '%s\\n' '{\"type\":\"thread.started\",\"thread_id\":\"" + usageSession + "\"}' '{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"REPORT\"}}' '{\"type\":\"turn.completed\"}'\n"
	if err := os.WriteFile(bin, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OFFLOAD_CODEX", bin)
	r, err := Run(context.Background(), o)
	if err != nil || r.Status != "ok" || r.Usage == nil || r.Usage.Input != 28594 || r.UsageSource != "native_codex_session" || r.CostUSD == nil || r.AccountingEvidence == "" {
		t.Fatalf("err=%v %+v", err, r)
	}
}
