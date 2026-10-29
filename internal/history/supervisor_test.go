package history

import (
	"path/filepath"
	"testing"
)

func TestSupervisorFieldsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runs.jsonl")
	rec := Record{RunID: "internal", CallerRunID: "caller:1", Backend: "codex", ExecutionPolicy: "managed-development", ErrorKind: "execution", Status: "error"}
	if err := Append(path, rec); err != nil {
		t.Fatal(err)
	}
	rs, err := Read(path)
	if err != nil || len(rs) != 1 {
		t.Fatal(rs, err)
	}
	got := rs[0]
	if got.RunID != rec.RunID || got.CallerRunID != rec.CallerRunID || got.ExecutionPolicy != rec.ExecutionPolicy || got.ErrorKind != rec.ErrorKind || got.Backend != rec.Backend {
		t.Fatal(got)
	}
}
