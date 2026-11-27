package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhoushoujianwork/offload/internal/history"
	"github.com/zhoushoujianwork/offload/internal/transcript"
)

func TestAgyACPProfileRunAndResume(t *testing.T) {
	work, _ := setup(t, okReply)
	dir := t.TempDir()
	agy := filepath.Join(dir, "agy")
	const script = `#!/bin/sh
set -eu
[ "$1" = acp ] && [ "$2" = --add-dir ] && [ "$3" = "$AGY_TEST_MOUNT" ] || exit 10
read_frame() { IFS= read -r frame; }
read_frame
case "$frame" in *'"method":"initialize"'*'"protocolVersion":1'*) ;; *) exit 11 ;; esac
echo '{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1,"agentCapabilities":{"sessionCapabilities":{"resume":{}}}}}'
read_frame
case "$frame" in
  *'"method":"session/new"'*) report="new" ;;
  *'"method":"session/resume"'*'"sessionId":"agy-session"'*) report="resumed" ;;
  *) exit 12 ;;
esac
echo '{"jsonrpc":"2.0","id":2,"result":{"sessionId":"agy-session","configOptions":[{"id":"model","currentValue":"default-model"}]}}'
read_frame
case "$frame" in *'"method":"session/set_config_option"'*'"configId":"model"'*'"value":"chosen-model"'*) ;; *) exit 13 ;; esac
echo '{"jsonrpc":"2.0","id":3,"result":{"configOptions":[{"id":"model","currentValue":"chosen-model"}]}}'
read_frame
case "$frame" in *'"method":"session/set_config_option"'*'"configId":"thought_level"'*'"value":"low"'*) ;; *) exit 14 ;; esac
echo '{"jsonrpc":"2.0","id":4,"result":{"configOptions":[{"id":"model","currentValue":"chosen-model"}]}}'
read_frame
case "$frame" in *'"method":"session/prompt"'*'"type":"text"'*'"sessionId":"agy-session"'*) ;; *) exit 15 ;; esac
if [ "${AGY_TEST_HANG:-}" = 1 ]; then exec sleep 60; fi
echo '{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"agy-session","update":{"sessionUpdate":"tool_call","toolCallId":"tool","title":"Test","kind":"execute"}}}'
echo '{"jsonrpc":"2.0","id":"permission","method":"session/request_permission","params":{"sessionId":"agy-session","options":[{"kind":"allow_once","optionId":"allow"},{"kind":"reject_once","optionId":"deny"}]}}'
read_frame
case "$frame" in *'"id":"permission"'*'"optionId":"deny"'*'"outcome":"selected"'*) ;; *) exit 16 ;; esac
echo '{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"agy-session","update":{"sessionUpdate":"tool_call_update","toolCallId":"tool","status":"failed"}}}'
echo '{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"foreign","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"ignored"}}}}'
printf '{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"agy-session","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"%s "}}}}\n' "$report"
echo '{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"agy-session","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"report"}}}}'
echo '{"jsonrpc":"2.0","id":5,"result":{"stopReason":"end_turn"}}'
# Some servers exit immediately instead of waiting for the client to close stdin.
`
	if err := os.WriteFile(agy, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	mount := t.TempDir()
	t.Setenv("AGY_TEST_MOUNT", mount)
	input := strings.NewReader(`{"name":"agy","create":true,"backend":"agy","model":"chosen-model","reasoning_effort":"low"}`)
	var output, stderr strings.Builder
	if code := cmdProfile([]string{"save", "--json"}, input, &output, &stderr); code != 0 {
		t.Fatal(stderr.String())
	}
	if code, out, err := runCLI("-p", "agy", "-d", work, "--add-dir", mount, "first task"); code != 0 || !strings.Contains(out, "new report") {
		t.Fatalf("fresh code=%d stdout=%q stderr=%q", code, out, err)
	}
	if code, out, err := runCLI("-r", "agy-session", "next task"); code != 0 || !strings.Contains(out, "resumed report") {
		t.Fatalf("resume code=%d stdout=%q stderr=%q", code, out, err)
	}
	records, err := history.Read(history.Path())
	if err != nil || len(records) != 2 {
		t.Fatalf("history: %v %v", records, err)
	}
	for _, record := range records {
		if record.Backend != "agy" || record.NativeProtocol != "acp" || record.ObservedModel != "chosen-model" ||
			record.Session != "agy-session" || record.Turns != 1 || record.Denials != 1 ||
			record.Usage != nil || record.CostUSD != nil || record.UsageUnavailable == "" || record.CostUnavailable == "" {
			t.Fatalf("unexpected ACP history: %+v", record)
		}
		capture := transcript.Read(transcript.ReadOptions{RunID: record.RunID})
		found := false
		for _, raw := range capture.Events {
			var event transcript.Event
			if err := json.Unmarshal(raw, &event); err != nil {
				t.Fatal(err)
			}
			if event.Type == transcript.TypeMessage && event.Role == "assistant" && event.Text == record.Result {
				found = true
			}
		}
		if capture.Availability != transcript.Available || !found {
			t.Fatalf("missing ACP report in transcript: %+v", capture)
		}
	}

	t.Setenv("AGY_TEST_HANG", "1")
	if code, _, err := runCLI("-r", "agy-session", "-t", "2s", "time out"); code != 124 {
		t.Fatalf("timeout code=%d stderr=%q", code, err)
	}
	records, err = history.Read(history.Path())
	if err != nil || len(records) != 3 {
		t.Fatalf("history after timeout: %v %v", records, err)
	}
	last := records[2]
	if last.Status != "timeout" || last.Session != "agy-session" || last.Cleanup != "" {
		t.Fatalf("ACP timeout lost its session or left an owned process: %+v", last)
	}
}
