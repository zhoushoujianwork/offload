package execution_test

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhoushoujianwork/offload/internal/transcript"
	"github.com/zhoushoujianwork/offload/pkg/execution"
)

// The public readers must produce byte-identical JSON to the CLI's reader.
func TestPublicTranscriptMatchesInternalReader(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "state"))
	id := strings.Repeat("e", 32)
	rec, err := transcript.Open(transcript.Identity{RunID: id, CallerRunID: "c1", Caller: "agh", Backend: "codex", Model: "m"}, transcript.NewRedactor())
	if err != nil {
		t.Fatal(err)
	}
	rec.BeginAttempt("fresh", "task")
	rec.Line([]byte(`{"type":"item.completed","item":{"id":"m","type":"agent_message","text":"hello"}}`))
	rec.End(transcript.EndInfo{Status: "ok"})
	same := func(a, b any) {
		t.Helper()
		x, _ := json.Marshal(a)
		y, _ := json.Marshal(b)
		if string(x) != string(y) {
			t.Fatalf("public %s\ninternal %s", x, y)
		}
	}
	for _, o := range []transcript.ReadOptions{{RunID: id}, {RunID: id, Limit: 1}, {RunID: strings.Repeat("f", 32)}, {RunID: id, Cursor: "bad"}, {CallerRunID: "c1", MaxBytes: 64}} {
		same(execution.ReadTranscript(o), transcript.Read(o))
	}
	same(execution.ListTranscripts("c1", 5), transcript.List("c1", 5))
	if r := execution.ReadTranscript(execution.TranscriptReadOptions{RunID: id}); r.Availability != transcript.Available || len(r.Events) == 0 {
		t.Fatalf("%+v", r)
	}
}
